/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package libgit2

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/go-logr/logr"
	git2go "github.com/libgit2/git2go/v33"

	"github.com/fluxcd/pkg/gitutil"
	"github.com/fluxcd/pkg/version"

	"github.com/fluxcd/source-controller/pkg/git"
	"github.com/fluxcd/source-controller/pkg/git/libgit2/managed"
)

const defaultRemoteName = "origin"

// CheckoutStrategyForOptions returns the git.CheckoutStrategy for the given
// git.CheckoutOptions.
func CheckoutStrategyForOptions(ctx context.Context, opt git.CheckoutOptions) git.CheckoutStrategy {
	if opt.RecurseSubmodules {
		logr.FromContextOrDiscard(ctx).Info(fmt.Sprintf("git submodule recursion not supported by implementation '%s'", Implementation))
	}
	switch {
	case opt.Commit != "":
		return &CheckoutCommit{Commit: opt.Commit}
	case opt.SemVer != "":
		return &CheckoutSemVer{SemVer: opt.SemVer}
	case opt.Tag != "":
		return &CheckoutTag{
			Tag:          opt.Tag,
			LastRevision: opt.LastRevision,
		}
	default:
		branch := opt.Branch
		if branch == "" {
			branch = git.DefaultBranch
		}
		return &CheckoutBranch{
			Branch:       branch,
			LastRevision: opt.LastRevision,
		}
	}
}

type CheckoutBranch struct {
	Branch       string
	LastRevision string
}

func (c *CheckoutBranch) Checkout(ctx context.Context, path, url string, opts *git.AuthOptions) (_ *git.Commit, err error) {
	defer recoverPanic(&err)

	err = registerManagedTransportOptions(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	transportOptsURL := opts.TransportOptionsURL
	remoteCallBacks := managed.RemoteCallbacks()
	defer managed.RemoveTransportOptions(transportOptsURL)

	repo, remote, err := initializeRepoWithRemote(ctx, path, url, opts)
	if err != nil {
		return nil, err
	}
	// Open remote connection.
	err = remote.ConnectFetch(&remoteCallBacks, nil, nil)
	if err != nil {
		remote.Free()
		repo.Free()
		return nil, fmt.Errorf("unable to fetch-connect to remote '%s': %w", url, gitutil.LibGit2Error(err))
	}
	defer func() {
		remote.Disconnect()
		remote.Free()
		repo.Free()
	}()

	// When the last observed revision is set, check whether it is still the
	// same at the remote branch. If so, short-circuit the clone operation here.
	if c.LastRevision != "" {
		heads, err := remote.Ls(c.Branch)
		if err != nil {
			return nil, fmt.Errorf("unable to remote ls for '%s': %w", url, gitutil.LibGit2Error(err))
		}
		if len(heads) > 0 {
			hash := heads[0].Id.String()
			currentRevision := fmt.Sprintf("%s/%s", c.Branch, hash)
			if currentRevision == c.LastRevision {
				// Construct a partial commit with the existing information.
				c := &git.Commit{
					Hash:      git.Hash(hash),
					Reference: "refs/heads/" + c.Branch,
				}
				return c, nil
			}
		}
	}

	// Limit the fetch operation to the specific branch, to decrease network usage.
	err = remote.Fetch([]string{c.Branch},
		&git2go.FetchOptions{
			DownloadTags:    git2go.DownloadTagsNone,
			RemoteCallbacks: remoteCallBacks,
		},
		"")
	if err != nil {
		return nil, fmt.Errorf("unable to fetch remote '%s': %w", url, gitutil.LibGit2Error(err))
	}

	branch, err := repo.References.Lookup(fmt.Sprintf("refs/remotes/origin/%s", c.Branch))
	if err != nil {
		return nil, fmt.Errorf("unable to lookup branch '%s' for '%s': %w", c.Branch, url, gitutil.LibGit2Error(err))
	}
	defer branch.Free()

	upstreamCommit, err := repo.LookupCommit(branch.Target())
	if err != nil {
		return nil, fmt.Errorf("unable to lookup commit '%s' for '%s': %w", c.Branch, url, gitutil.LibGit2Error(err))
	}
	defer upstreamCommit.Free()

	// We try to lookup the branch (and create it if it doesn't exist), so that we can
	// switch the repo to the specified branch. This is done so that users of this api
	// can expect the repo to be at the desired branch, when cloned.
	localBranch, err := repo.LookupBranch(c.Branch, git2go.BranchLocal)
	if git2go.IsErrorCode(err, git2go.ErrorCodeNotFound) {
		localBranch, err = repo.CreateBranch(c.Branch, upstreamCommit, false)
		if err != nil {
			return nil, fmt.Errorf("unable to create local branch '%s': %w", c.Branch, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("unable to lookup branch '%s': %w", c.Branch, err)
	}
	defer localBranch.Free()

	tree, err := repo.LookupTree(upstreamCommit.TreeId())
	if err != nil {
		return nil, fmt.Errorf("unable to lookup tree for branch '%s': %w", c.Branch, err)
	}
	defer tree.Free()

	err = repo.CheckoutTree(tree, &git2go.CheckoutOpts{
		// the remote branch should take precedence if it exists at this point in time.
		Strategy: git2go.CheckoutForce,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to checkout tree for branch '%s': %w", c.Branch, err)
	}

	// Set the current head to point to the requested branch.
	err = repo.SetHead("refs/heads/" + c.Branch)
	if err != nil {
		return nil, fmt.Errorf("unable to set HEAD to branch '%s':%w", c.Branch, err)
	}

	// Use the current worktree's head as reference for the commit to be returned.
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("unable to resolve HEAD: %w", err)
	}
	defer head.Free()

	cc, err := repo.LookupCommit(head.Target())
	if err != nil {
		return nil, fmt.Errorf("unable to lookup HEAD commit '%s' for branch '%s': %w", head.Target(), c.Branch, err)
	}
	defer cc.Free()

	return buildCommit(cc, "refs/heads/"+c.Branch), nil
}

type CheckoutTag struct {
	Tag          string
	LastRevision string
}

func (c *CheckoutTag) Checkout(ctx context.Context, path, url string, opts *git.AuthOptions) (_ *git.Commit, err error) {
	defer recoverPanic(&err)

	err = registerManagedTransportOptions(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	transportOptsURL := opts.TransportOptionsURL
	remoteCallBacks := managed.RemoteCallbacks()
	defer managed.RemoveTransportOptions(transportOptsURL)

	repo, remote, err := initializeRepoWithRemote(ctx, path, url, opts)
	if err != nil {
		return nil, err
	}
	// Open remote connection.
	err = remote.ConnectFetch(&remoteCallBacks, nil, nil)
	if err != nil {
		remote.Free()
		repo.Free()
		return nil, fmt.Errorf("unable to fetch-connect to remote '%s': %w", url, gitutil.LibGit2Error(err))
	}
	defer func() {
		remote.Disconnect()
		remote.Free()
		repo.Free()
	}()

	// When the last observed revision is set, check whether it is still the
	// same at the remote branch. If so, short-circuit the clone operation here.
	if c.LastRevision != "" {
		heads, err := remote.Ls(c.Tag)
		if err != nil {
			return nil, fmt.Errorf("unable to remote ls for '%s': %w", url, gitutil.LibGit2Error(err))
		}
		if len(heads) > 0 {
			hash := heads[0].Id.String()
			currentRevision := fmt.Sprintf("%s/%s", c.Tag, hash)
			var same bool
			if currentRevision == c.LastRevision {
				same = true
			} else if len(heads) > 1 {
				hash = heads[1].Id.String()
				currentAnnotatedRevision := fmt.Sprintf("%s/%s", c.Tag, hash)
				if currentAnnotatedRevision == c.LastRevision {
					same = true
				}
			}
			if same {
				// Construct a partial commit with the existing information.
				c := &git.Commit{
					Hash:      git.Hash(hash),
					Reference: "refs/tags/" + c.Tag,
				}
				return c, nil
			}
		}
	}

	err = remote.Fetch([]string{c.Tag},
		&git2go.FetchOptions{
			DownloadTags:    git2go.DownloadTagsAuto,
			RemoteCallbacks: remoteCallBacks,
		},
		"")

	if err != nil {
		return nil, fmt.Errorf("unable to fetch remote '%s': %w", url, gitutil.LibGit2Error(err))
	}

	cc, err := checkoutDetachedDwim(repo, c.Tag)
	if err != nil {
		return nil, err
	}
	defer cc.Free()
	return buildCommit(cc, "refs/tags/"+c.Tag), nil
}

type CheckoutCommit struct {
	Commit string
}

func (c *CheckoutCommit) Checkout(ctx context.Context, path, url string, opts *git.AuthOptions) (_ *git.Commit, err error) {
	defer recoverPanic(&err)

	err = registerManagedTransportOptions(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	transportOptsURL := opts.TransportOptionsURL
	defer managed.RemoveTransportOptions(transportOptsURL)

	repo, err := git2go.Clone(transportOptsURL, path, &git2go.CloneOptions{
		FetchOptions: git2go.FetchOptions{
			DownloadTags:    git2go.DownloadTagsNone,
			RemoteCallbacks: managed.RemoteCallbacks(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("unable to clone '%s': %w", url, gitutil.LibGit2Error(err))
	}
	defer repo.Free()
	oid, err := git2go.NewOid(c.Commit)
	if err != nil {
		return nil, fmt.Errorf("could not create oid for '%s': %w", c.Commit, err)
	}
	cc, err := checkoutDetachedHEAD(repo, oid)
	if err != nil {
		return nil, fmt.Errorf("git checkout error: %w", err)
	}
	return buildCommit(cc, ""), nil
}

type CheckoutSemVer struct {
	SemVer string
}

func (c *CheckoutSemVer) Checkout(ctx context.Context, path, url string, opts *git.AuthOptions) (_ *git.Commit, err error) {
	defer recoverPanic(&err)

	err = registerManagedTransportOptions(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	transportOptsURL := opts.TransportOptionsURL
	defer managed.RemoveTransportOptions(transportOptsURL)

	verConstraint, err := semver.NewConstraint(c.SemVer)
	if err != nil {
		return nil, fmt.Errorf("semver parse error: %w", err)
	}

	repo, err := git2go.Clone(transportOptsURL, path, &git2go.CloneOptions{
		FetchOptions: git2go.FetchOptions{
			DownloadTags:    git2go.DownloadTagsAll,
			RemoteCallbacks: managed.RemoteCallbacks(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("unable to clone '%s': %w", url, gitutil.LibGit2Error(err))
	}
	defer repo.Free()

	tags := make(map[string]string)
	tagTimestamps := make(map[string]time.Time)
	if err := repo.Tags.Foreach(func(name string, id *git2go.Oid) error {
		cleanName := strings.TrimPrefix(name, "refs/tags/")
		// The given ID can refer to both a commit and a tag, as annotated tags contain additional metadata.
		// Due to this, first attempt to resolve it as a simple tag (commit), but fallback to attempting to
		// resolve it as an annotated tag in case this results in an error.
		if c, err := repo.LookupCommit(id); err == nil {
			defer c.Free()
			// Use the commit metadata as the decisive timestamp.
			tagTimestamps[cleanName] = c.Committer().When
			tags[cleanName] = name
			return nil
		}
		t, err := repo.LookupTag(id)
		if err != nil {
			return fmt.Errorf("could not lookup '%s' as simple or annotated tag: %w", cleanName, err)
		}
		defer t.Free()
		commit, err := t.Peel(git2go.ObjectCommit)
		if err != nil {
			return fmt.Errorf("could not get commit for tag '%s': %w", t.Name(), err)
		}
		defer commit.Free()
		c, err := commit.AsCommit()
		if err != nil {
			return fmt.Errorf("could not get commit object for tag '%s': %w", t.Name(), err)
		}
		defer c.Free()
		tagTimestamps[t.Name()] = c.Committer().When
		tags[t.Name()] = name
		return nil
	}); err != nil {
		return nil, err
	}

	var matchedVersions semver.Collection
	for tag := range tags {
		v, err := version.ParseVersion(tag)
		if err != nil {
			continue
		}
		if !verConstraint.Check(v) {
			continue
		}
		matchedVersions = append(matchedVersions, v)
	}
	if len(matchedVersions) == 0 {
		return nil, fmt.Errorf("no match found for semver: %s", c.SemVer)
	}

	// Sort versions
	sort.SliceStable(matchedVersions, func(i, j int) bool {
		left := matchedVersions[i]
		right := matchedVersions[j]

		if !left.Equal(right) {
			return left.LessThan(right)
		}

		// Having tag target timestamps at our disposal, we further try to sort
		// versions into a chronological order. This is especially important for
		// versions that differ only by build metadata, because it is not considered
		// a part of the comparable version in Semver
		return tagTimestamps[left.Original()].Before(tagTimestamps[right.Original()])
	})
	v := matchedVersions[len(matchedVersions)-1]
	t := v.Original()

	cc, err := checkoutDetachedDwim(repo, t)
	if err != nil {
		return nil, err
	}
	defer cc.Free()
	return buildCommit(cc, "refs/tags/"+t), nil
}

// checkoutDetachedDwim attempts to perform a detached HEAD checkout by first DWIMing the short name
// to get a concrete reference, and then calling checkoutDetachedHEAD.
func checkoutDetachedDwim(repo *git2go.Repository, name string) (*git2go.Commit, error) {
	ref, err := repo.References.Dwim(name)
	if err != nil {
		return nil, fmt.Errorf("unable to find '%s': %w", name, err)
	}
	defer ref.Free()
	c, err := ref.Peel(git2go.ObjectCommit)
	if err != nil {
		return nil, fmt.Errorf("could not get commit for ref '%s': %w", ref.Name(), err)
	}
	defer c.Free()
	cc, err := c.AsCommit()
	if err != nil {
		return nil, fmt.Errorf("could not get commit object for ref '%s': %w", ref.Name(), err)
	}
	defer cc.Free()
	return checkoutDetachedHEAD(repo, cc.Id())
}

// checkoutDetachedHEAD attempts to perform a detached HEAD checkout for the given commit.
func checkoutDetachedHEAD(repo *git2go.Repository, oid *git2go.Oid) (*git2go.Commit, error) {
	cc, err := repo.LookupCommit(oid)
	if err != nil {
		return nil, fmt.Errorf("git commit '%s' not found: %w", oid.String(), err)
	}
	if err = repo.SetHeadDetached(cc.Id()); err != nil {
		cc.Free()
		return nil, fmt.Errorf("could not detach HEAD at '%s': %w", oid.String(), err)
	}
	if err = repo.CheckoutHead(&git2go.CheckoutOptions{
		Strategy: git2go.CheckoutForce,
	}); err != nil {
		cc.Free()
		return nil, fmt.Errorf("git checkout error: %w", err)
	}
	return cc, nil
}

// headCommit returns the current HEAD of the repository, or an error.
func headCommit(repo *git2go.Repository) (*git2go.Commit, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, err
	}
	defer head.Free()
	c, err := repo.LookupCommit(head.Target())
	if err != nil {
		return nil, err
	}
	return c, nil
}

func buildCommit(c *git2go.Commit, ref string) *git.Commit {
	sig, msg, _ := c.ExtractSignature()
	return &git.Commit{
		Hash:      []byte(c.Id().String()),
		Reference: ref,
		Author:    buildSignature(c.Author()),
		Committer: buildSignature(c.Committer()),
		Signature: sig,
		Encoded:   []byte(msg),
		Message:   c.Message(),
	}
}

func buildSignature(s *git2go.Signature) git.Signature {
	return git.Signature{
		Name:  s.Name,
		Email: s.Email,
		When:  s.When,
	}
}

// initializeRepoWithRemote initializes or opens a repository at the given path
// and configures it with the given transport opts URL (as a placeholder for the
// actual target url). If a remote already exists with a different URL, it overwrites
// it with the provided transport opts URL.
func initializeRepoWithRemote(ctx context.Context, path, url string, opts *git.AuthOptions) (*git2go.Repository, *git2go.Remote, error) {
	repo, err := git2go.InitRepository(path, false)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to init repository for '%s': %w", url, gitutil.LibGit2Error(err))
	}

	transportOptsURL := opts.TransportOptionsURL
	remote, err := repo.Remotes.Create(defaultRemoteName, transportOptsURL)
	if err != nil {
		// If the remote already exists, lookup the remote.
		if git2go.IsErrorCode(err, git2go.ErrorCodeExists) {
			remote, err = repo.Remotes.Lookup(defaultRemoteName)
			if err != nil {
				repo.Free()
				return nil, nil, fmt.Errorf("unable to create or lookup remote '%s'", defaultRemoteName)
			}

			if remote.Url() != transportOptsURL {
				err = repo.Remotes.SetUrl("origin", transportOptsURL)
				if err != nil {
					repo.Free()
					remote.Free()
					return nil, nil, fmt.Errorf("unable to configure remote %s origin with url %s", defaultRemoteName, url)
				}

				// refresh the remote
				remote, err = repo.Remotes.Lookup(defaultRemoteName)
				if err != nil {
					repo.Free()
					return nil, nil, fmt.Errorf("unable to create or lookup remote '%s'", defaultRemoteName)
				}
			}
		} else {
			repo.Free()
			return nil, nil, fmt.Errorf("unable to create remote for '%s': %w", url, gitutil.LibGit2Error(err))
		}
	}
	return repo, remote, nil
}

// registerManagedTransportOptions registers the given url and it's transport options.
// Callers must make sure to call `managed.RemoveTransportOptions()` to avoid increase in
// memory consumption.
// We store the target URL, auth options, etc. mapped to TransporOptsURL because managed transports
// don't provide a way for any kind of dependency injection.
// This lets us have a way of doing interop between application level code and transport level code
// which enables us to fetch the required credentials, context, etc. at the transport level.
func registerManagedTransportOptions(ctx context.Context, url string, authOpts *git.AuthOptions) error {
	if authOpts == nil {
		return errors.New("can't checkout using libgit2 with an empty set of auth options")
	}
	if authOpts.TransportOptionsURL == "" {
		return errors.New("can't checkout using libgit2 without a valid transport auth id")
	}
	managed.AddTransportOptions(authOpts.TransportOptionsURL, managed.TransportOptions{
		TargetURL:    url,
		AuthOpts:     authOpts,
		ProxyOptions: &git2go.ProxyOptions{Type: git2go.ProxyTypeAuto},
		Context:      ctx,
	})
	return nil
}

func recoverPanic(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("recovered from git2go panic: %v", r)
	}
}

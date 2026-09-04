package whatchanged

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/mod/semver"
)

// resolveCommit resolves rev to the commit it names. Its errors say what is
// wrong with the revision and, where the repository shows it, what to do
// about it: a shallow clone, a branch that was never fetched, a repository
// with no commits yet.
func resolveCommit(repo *git.Repository, rev string) (plumbing.Hash, error) {
	hash, err := repo.ResolveRevision(plumbing.Revision(rev))
	switch {
	case err == nil:
		return *hash, nil
	case errors.Is(err, plumbing.ErrReferenceNotFound):
		return plumbing.ZeroHash, fmt.Errorf("@%s: %s", rev, notFound(repo, rev))
	case errors.Is(err, io.EOF), errors.Is(err, plumbing.ErrObjectNotFound):
		// go-git runs out of parents walking ~N or ^ past the root commit,
		// or past the boundary of a shallow clone.
		return plumbing.ZeroHash, fmt.Errorf("@%s: the history does not reach back that far%s", rev, shallowHint(repo))
	default:
		return plumbing.ZeroHash, fmt.Errorf("@%s: %w", rev, err)
	}
}

// notFound explains a revision the repository does not have: what the
// repository does have, and the one most likely reason it lacks this one.
func notFound(repo *git.Repository, rev string) string {
	if branch, unborn := unbornHead(repo); unborn && (rev == "HEAD" || rev == branch) {
		return fmt.Sprintf("the repository has no commits yet (%s is an unborn branch)", branch)
	}
	msg := "no such tag, branch or commit" + refList(repo)
	switch remote := remoteBranch(repo, rev); {
	case remote != "":
		return msg + "; did you mean @" + remote + "?"
	case fetchHint(repo, rev) != "":
		return msg + fetchHint(repo, rev)
	case shallowHint(repo) != "":
		return msg + shallowHint(repo)
	case looksLikeTag(rev) && !hasTags(repo):
		return msg + "; the clone has no tags at all: git fetch --tags brings them"
	default:
		return msg + "; a revision is written @<tag>, @<branch>, @<commit> or @HEAD~2, and @latest is the newest release tag"
	}
}

// unbornHead reports whether HEAD points at a branch with no commits, as in
// a freshly initialized repository, and names the branch.
func unbornHead(repo *git.Repository) (branch string, unborn bool) {
	head, err := repo.Reference(plumbing.HEAD, false)
	if err != nil || head.Type() != plumbing.SymbolicReference {
		return "", false
	}
	if _, err := repo.Reference(head.Target(), true); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", false
	}
	return head.Target().Short(), true
}

// remoteBranch returns "origin/rev" when rev is not a local branch but a
// remote has a branch of that name, as after a clone or a CI checkout that
// created no local branches; "" otherwise.
func remoteBranch(repo *git.Repository, rev string) string {
	remotes, err := repo.Remotes()
	if err != nil {
		return ""
	}
	for _, r := range remotes {
		name := r.Config().Name + "/" + rev
		if _, err := repo.Reference(plumbing.NewRemoteReferenceName(r.Config().Name, rev), true); err == nil {
			return name
		}
	}
	return ""
}

// fetchHint suggests the fetch for a remote-tracking branch, "origin/main",
// that a configured remote has not delivered yet: a CI checkout fetches one
// ref, and a clone only the branches that existed at the time.
func fetchHint(repo *git.Repository, rev string) string {
	name := strings.TrimPrefix(rev, "refs/remotes/")
	remote, branch, ok := strings.Cut(name, "/")
	if !ok || branch == "" {
		return ""
	}
	remotes, err := repo.Remotes()
	if err != nil {
		return ""
	}
	for _, r := range remotes {
		if r.Config().Name == remote {
			return fmt.Sprintf("; fetch it first: git fetch %s %s", remote, branch)
		}
	}
	return ""
}

// shallowHint says how to complete a shallow clone, whose missing history
// and tags are the usual reason a revision or a release cannot be found,
// or "" for a full clone. In GitHub Actions the fix is a checkout option.
func shallowHint(repo *git.Repository) string {
	if shallow, err := repo.Storer.Shallow(); err != nil || len(shallow) == 0 {
		return ""
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		return "; the checkout is shallow: check out with fetch-depth: 0 to have the whole history and its tags"
	}
	return "; the clone is shallow: git fetch --unshallow --tags brings the whole history and its tags"
}

// looksLikeTag reports whether rev is spelled like a release tag, "v1.2.3"
// or "sub/v1.2.3".
func looksLikeTag(rev string) bool {
	return semver.IsValid(path.Base(rev))
}

// hasTags reports whether the repository has any tag.
func hasTags(repo *git.Repository) bool {
	return len(tagNames(repo)) > 0
}

// tagNames lists the repository's tags, newest first: valid semantic
// versions by version, the rest by name after them.
func tagNames(repo *git.Repository) []string {
	refs, err := repo.Tags()
	if err != nil {
		return nil
	}
	var tags []string
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		tags = append(tags, ref.Name().Short())
		return nil
	})
	slices.SortFunc(tags, func(a, b string) int {
		if va, vb := semver.IsValid(a), semver.IsValid(b); va != vb {
			if va {
				return -1
			}
			return 1
		} else if va {
			return semver.Compare(b, a)
		}
		return strings.Compare(a, b)
	})
	return tags
}

// branchNames lists the repository's local branches, sorted.
func branchNames(repo *git.Repository) []string {
	refs, err := repo.Branches()
	if err != nil {
		return nil
	}
	var branches []string
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		branches = append(branches, ref.Name().Short())
		return nil
	})
	slices.Sort(branches)
	return branches
}

// refList describes the repository's branches and tags for an error
// message, a few of each, or "" when it has neither.
func refList(repo *git.Repository) string {
	var parts []string
	if branches := branchNames(repo); len(branches) > 0 {
		parts = append(parts, "branches: "+fewOf(branches))
	}
	if tags := tagNames(repo); len(tags) > 0 {
		parts = append(parts, "tags: "+fewOf(tags))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// fewOf joins the first few names and counts the rest.
func fewOf(names []string) string {
	const show = 6
	more := ""
	if len(names) > show {
		more = fmt.Sprintf(", and %d more", len(names)-show)
		names = names[:show]
	}
	return strings.Join(names, ", ") + more
}

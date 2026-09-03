package whatchanged

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/mod/semver"

	"github.com/shazow/go-whatchanged/internal/modres"
	"github.com/shazow/go-whatchanged/internal/release"
)

// LatestRelease is the pseudo-revision that names the newest release tag of
// the module among the ancestors of the head commit: "what changed since the
// last release". Release tags are the ones the go command would publish,
// "v1.2.3" (or "sub/v1.2.3" for a module in a subdirectory) with the major
// version the module path calls for. Tags on the head commit itself do not
// count, so that on a freshly tagged commit the diff describes that release
// instead of being empty.
const LatestRelease = "@latest"

// resolveBase turns the base revision into a concrete one, resolving
// LatestRelease, and reports the semantic version the base denotes when it
// is a release tag of the module (so the summary can suggest the next
// version). Problems reading the head side's go.mod are left for loadSide to
// report, unless LatestRelease depends on it.
func resolveBase(open openFunc, base string, head sideSpec, rel string, env modres.Env) (rev, version string, err error) {
	var tags release.Tags
	modPath, err := headModulePath(open, head, rel, env)
	if err == nil {
		tags, err = release.TagsFor(modPath, rel)
	}
	if base != LatestRelease {
		if err != nil {
			return base, "", nil
		}
		return base, tags.Version(tagName(base)), nil
	}
	if err == nil {
		rev, version, err = latestTag(open, tags, head.rev, modPath)
	}
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", LatestRelease, err)
	}
	return rev, version, nil
}

// tagName strips the ref prefixes a user may type in front of a tag.
func tagName(rev string) string {
	rev = strings.TrimPrefix(rev, "refs/")
	return strings.TrimPrefix(rev, "tags/")
}

// headModulePath reads the module path from the head side's go.mod.
func headModulePath(open openFunc, head sideSpec, rel string, env modres.Env) (string, error) {
	s, err := head.mount(open, rel)
	if err != nil {
		return "", err
	}
	res, err := modres.New(s.overlay, s.root, env)
	if err != nil {
		return "", fmt.Errorf("%s: %w", s.label, err)
	}
	return res.ModPath(), nil
}

// candidate is a release tag and the version it denotes.
type candidate struct {
	tag, version string
}

// latestTag returns the highest release tag among the proper ancestors of
// headRev, "" for the working tree, whose commit is HEAD. Annotated tags are
// followed to the commit they point at; tags on anything but a commit are
// ignored.
func latestTag(open openFunc, tags release.Tags, headRev, modPath string) (tag, version string, err error) {
	repo, err := open()
	if err != nil {
		return "", "", err
	}
	if headRev == "" {
		headRev = "HEAD"
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(headRev))
	if err != nil {
		return "", "", fmt.Errorf("resolve %q: %w", headRev, err)
	}
	if _, err := repo.CommitObject(*hash); err != nil {
		return "", "", fmt.Errorf("%q is not a commit", headRev)
	}
	head := *hash
	byCommit := map[plumbing.Hash][]candidate{}
	refs, err := repo.Tags()
	if err != nil {
		return "", "", err
	}
	total := 0
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		v := tags.Version(name)
		if v == "" {
			return nil
		}
		h := ref.Hash()
		if t, err := repo.TagObject(h); err == nil {
			c, err := t.Commit()
			if err != nil {
				return nil
			}
			h = c.Hash
		}
		total++
		byCommit[h] = append(byCommit[h], candidate{tag: name, version: v})
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if total == 0 {
		return "", "", fmt.Errorf("no release tags for %s (looking for tags like %q)", modPath, tags.Example())
	}

	// Walk the ancestors of head, stopping once every tagged commit has been
	// seen. The head commit itself is skipped.
	var best candidate
	remaining := len(byCommit)
	log, err := repo.Log(&git.LogOptions{From: head})
	if err != nil {
		return "", "", err
	}
	err = log.ForEach(func(c *object.Commit) error {
		if c.Hash == head {
			return nil
		}
		cands, ok := byCommit[c.Hash]
		if !ok {
			return nil
		}
		for _, cand := range cands {
			if best.version == "" || semver.Compare(cand.version, best.version) > 0 {
				best = cand
			}
		}
		remaining--
		if remaining == 0 {
			return storer.ErrStop
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if best.version == "" {
		return "", "", fmt.Errorf("none of the %d release tag(s) for %s is an ancestor of %s", total, modPath, headRev)
	}
	return best.tag, best.version, nil
}

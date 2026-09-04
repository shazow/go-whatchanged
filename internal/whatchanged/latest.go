package whatchanged

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/shazow/go-whatchanged/internal/modfetch"
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

// resolveSides turns the sides' queries into concrete revisions and
// versions: a module side's query through src, and a git base of
// LatestRelease into the newest release tag. It also reports the semantic
// version the base denotes, when it is a release tag or a released module
// version, so that the summary can suggest the next version.
func resolveSides(ctx context.Context, base, head sideSpec, env modres.Env, src modfetch.Source) (b, h sideSpec, baseVersion string, err error) {
	for i, spec := range []*sideSpec{&base, &head} {
		if spec.mod.Path == "" {
			continue
		}
		if src == nil {
			return base, head, "", readOnlyError(spec.mod)
		}
		v, err := src.Resolve(ctx, spec.mod.Path, spec.mod.Version)
		if err != nil {
			return base, head, "", err
		}
		spec.mod = v
		if i == 0 && !module.IsPseudoVersion(v.Version) && semver.IsValid(v.Version) {
			baseVersion = v.Version
		}
	}
	if base.mod.Path != "" {
		return base, head, baseVersion, nil
	}
	rev, version, err := resolveBase(ctx, base, head, env, src)
	if err != nil {
		return base, head, "", err
	}
	base.rev = rev
	return base, head, version, nil
}

// resolveBase turns a git base revision into a concrete one, resolving
// LatestRelease against the head's module, and reports the semantic version
// the base denotes when it is a release tag of the module. Problems reading
// the head side's go.mod are left for loadSide to report, unless
// LatestRelease depends on it.
func resolveBase(ctx context.Context, base, head sideSpec, env modres.Env, src modfetch.Source) (rev, version string, err error) {
	var tags release.Tags
	modPath := head.mod.Path
	if modPath == "" {
		modPath, err = headModulePath(ctx, head, env, src)
	}
	if err == nil {
		tags, err = release.TagsFor(modPath, base.rel)
	}
	if base.rev != LatestRelease {
		if err != nil {
			return base.rev, "", nil
		}
		return base.rev, tags.Version(tagName(base.rev)), nil
	}
	// Problems with the head side name it themselves; only the search for
	// the tag is reported as @latest's.
	if err != nil {
		return "", "", err
	}
	repo, err := base.open()
	if err != nil {
		return "", "", err
	}
	headRev := head.rev
	if headRev == "" {
		headRev = "HEAD"
	}
	headHash, err := resolveCommit(repo, headRev)
	if err != nil {
		return "", "", err
	}
	rev, version, err = latestTag(repo, tags, headHash, headRev, modPath)
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
func headModulePath(ctx context.Context, head sideSpec, env modres.Env, src modfetch.Source) (string, error) {
	s, err := head.mount(ctx, src)
	if err != nil {
		return "", err
	}
	res, err := s.resolver(env)
	if err != nil {
		return "", err
	}
	return res.ModPath(), nil
}

// candidate is a release tag and the version it denotes.
type candidate struct {
	tag, version string
}

// latestTag returns the highest release tag among the proper ancestors of
// head, the commit headRev names. Annotated tags are followed to the commit
// they point at; tags on anything but a commit are ignored.
func latestTag(repo *git.Repository, tags release.Tags, head plumbing.Hash, headRev, modPath string) (tag, version string, err error) {
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
		switch {
		case shallowHint(repo) != "":
			return "", "", fmt.Errorf("no release tags for %s (looking for tags like %q)%s", modPath, tags.Example(), shallowHint(repo))
		case !hasTags(repo):
			return "", "", fmt.Errorf("no release tags for %s (looking for tags like %q); the clone has no tags at all: git fetch --tags brings them, if the repository has any", modPath, tags.Example())
		default:
			// The tags there are do not fit the module: a nested module
			// tagged without its directory prefix, or the other way around.
			return "", "", fmt.Errorf("no release tags for %s (looking for tags like %q; tags: %s)", modPath, tags.Example(), fewOf(tagNames(repo)))
		}
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
	// A shallow clone ends without the parents of its oldest commits:
	// the history reachable from head simply stops there.
	if err != nil && !errors.Is(err, plumbing.ErrObjectNotFound) {
		return "", "", err
	}
	if best.version == "" {
		// The tag on the head commit itself is the usual case: a freshly
		// tagged release, whose diff against itself would be empty.
		if onHead := byCommit[head]; len(onHead) > 0 {
			newest := slices.MaxFunc(onHead, func(a, b candidate) int { return semver.Compare(a.version, b.version) })
			return "", "", fmt.Errorf("%s is the newest release tag reachable from %s, but it is on %s itself, which @latest skips; name it instead: @%s", newest.tag, headRev, headRev, newest.tag)
		}
		var names []string
		for _, cands := range byCommit {
			for _, c := range cands {
				names = append(names, c.tag)
			}
		}
		slices.SortFunc(names, func(a, b string) int { return semver.Compare(tags.Version(b), tags.Version(a)) })
		return "", "", fmt.Errorf("none of the %d release tag(s) for %s (%s) is an ancestor of %s%s", total, modPath, fewOf(names), headRev, shallowHint(repo))
	}
	return best.tag, best.version, nil
}

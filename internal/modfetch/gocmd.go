package modfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/mod/module"
)

// GoCommand is a Source that runs the go command from PATH, which handles
// GOPROXY, GOPRIVATE, the checksum database and credentials on the tool's
// behalf. It writes only under GOMODCACHE: every command runs outside any
// module, with GOWORK=off, so the go.mod, go.sum and go.work of the
// repository are never read, let alone written.
type GoCommand struct {
	// Stderr receives a "downloading path version" line per download, since
	// the go command prints no progress in JSON mode, and whatever the go
	// command does print on its standard error; nil discards both. The go
	// command's text is also kept for error messages.
	Stderr io.Writer

	mu      sync.Mutex
	fetches map[module.Version]*fetch
}

// fetch is one module version being, or done being, downloaded, so that
// concurrent callers share a single go mod download per version.
type fetch struct {
	done chan struct{}
	mod  *Module
	err  error
}

// Resolve implements Source with go list -m. A canonical version resolves
// to itself without running anything.
func (g *GoCommand) Resolve(ctx context.Context, path, query string) (module.Version, error) {
	if module.CanonicalVersion(query) == query {
		return module.Version{Path: path, Version: query}, nil
	}
	what := "go list -m " + path + "@" + query
	out, stderr, err := g.run(ctx, "list", "-m", "-e", "-json", path+"@"+query)
	var m struct {
		Path, Version string
		Error         *struct{ Err string }
	}
	if jerr := json.Unmarshal(out, &m); jerr != nil || m.Path == "" {
		return module.Version{}, failure(what, stderr, err)
	}
	if m.Error != nil {
		return module.Version{}, failure(what, m.Error.Err, nil)
	}
	return module.Version{Path: m.Path, Version: m.Version}, nil
}

// Fetch implements Source with go mod download.
func (g *GoCommand) Fetch(ctx context.Context, mod module.Version) (*Module, error) {
	owned := g.claim([]module.Version{mod})
	if f, ok := owned[mod]; ok {
		results, err := g.download(ctx, []module.Version{mod})
		f.mod, f.err = results[mod].unpack(err)
		close(f.done)
		return f.mod, f.err
	}
	g.mu.Lock()
	f := g.fetches[mod]
	g.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.mod, f.err
}

// Prefetch implements Source with one go mod download for every version in
// mods that no Fetch or Prefetch has taken on yet.
func (g *GoCommand) Prefetch(ctx context.Context, mods []module.Version) error {
	owned := g.claim(mods)
	if len(owned) == 0 {
		return nil
	}
	list := slices.SortedFunc(maps.Keys(owned), func(a, b module.Version) int {
		return strings.Compare(a.String(), b.String())
	})
	results, err := g.download(ctx, list)
	for mod, f := range owned {
		f.mod, f.err = results[mod].unpack(err)
		close(f.done)
	}
	return err
}

// claim registers the versions in mods that no Fetch or Prefetch has yet
// taken on and returns their entries; the caller must fill and close every
// one it gets back.
func (g *GoCommand) claim(mods []module.Version) map[module.Version]*fetch {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fetches == nil {
		g.fetches = map[module.Version]*fetch{}
	}
	owned := map[module.Version]*fetch{}
	for _, mod := range mods {
		if _, ok := g.fetches[mod]; ok {
			continue
		}
		f := &fetch{done: make(chan struct{})}
		g.fetches[mod] = f
		owned[mod] = f
	}
	return owned
}

// result is one module's outcome in a download.
type result struct {
	mod *Module
	err error
}

// unpack returns the result, or the download's own error for a module it
// did not report on: a command that could not run at all, or one whose
// output stopped short.
func (r result) unpack(batchErr error) (*Module, error) {
	switch {
	case r.mod != nil || r.err != nil:
		return r.mod, r.err
	case batchErr != nil:
		return nil, batchErr
	default:
		return nil, errors.New("go mod download: no report for the module in its output")
	}
}

// download runs one go mod download for mods and returns what it reported
// about each. go mod download carries on past a module it cannot get and
// reports the failure in that module's JSON object, so an error comes back
// only when the command produced no report at all.
func (g *GoCommand) download(ctx context.Context, mods []module.Version) (map[module.Version]result, error) {
	what := "go mod download " + mods[0].String()
	if len(mods) > 1 {
		what = fmt.Sprintf("go mod download (%d modules)", len(mods))
	}
	args := []string{"mod", "download", "-json"}
	for _, mod := range mods {
		if g.Stderr != nil {
			fmt.Fprintf(g.Stderr, "downloading %s %s\n", mod.Path, mod.Version)
		}
		args = append(args, mod.String())
	}
	out, stderr, err := g.run(ctx, args...)

	results := map[module.Version]result{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var m struct {
			Path, Version, Error string
			GoMod, Dir           string
		}
		if derr := dec.Decode(&m); derr != nil || m.Path == "" {
			break
		}
		mod := module.Version{Path: m.Path, Version: m.Version}
		switch {
		case m.Error != "":
			results[mod] = result{err: failure("go mod download "+mod.String(), m.Error, nil)}
		case m.Dir == "" || m.GoMod == "":
			results[mod] = result{err: fmt.Errorf("go mod download %s: no directory in its report", mod)}
		default:
			gomod, rerr := os.ReadFile(m.GoMod)
			if rerr != nil {
				results[mod] = result{err: fmt.Errorf("go mod download %s: %w", mod, rerr)}
				continue
			}
			results[mod] = result{mod: &Module{Version: mod, Dir: m.Dir, GoMod: gomod}}
		}
	}
	if len(results) == 0 {
		return nil, failure(what, stderr, err)
	}
	return results, nil
}

// run executes the go command with args and returns its standard output
// and error, and the error from running it. See GoCommand for the
// environment it runs in.
func (g *GoCommand) run(ctx context.Context, args ...string) (stdout []byte, stderr string, err error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	// The root of the filesystem is the one directory with no go.mod above
	// it: from there the go command cannot find, and so cannot touch, the
	// module the user is in.
	wd, _ := os.Getwd()
	cmd.Dir = filepath.VolumeName(wd) + string(filepath.Separator)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GO111MODULE=on")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if g.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&errb, g.Stderr)
	}
	err = cmd.Run()
	return out.Bytes(), errb.String(), err
}

// failure is the error for a failed command: its message when it gave one,
// else the error from running it.
func failure(what, msg string, err error) error {
	if msg = strings.TrimSpace(msg); msg != "" {
		err = errors.New(msg)
	} else if err == nil {
		err = errors.New("unexpected output")
	}
	return fmt.Errorf("%s: %w", what, err)
}

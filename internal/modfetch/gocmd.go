package modfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/mod/module"
)

// Prefetcher is a Source that can obtain several module versions ahead of
// need, together; Fetch then answers from what Prefetch got. A go.mod at
// go 1.17 or later names every module its packages and tests can import, so
// fetching its missing requirements as one batch, before type-checking
// starts, replaces one download per import with one parallel download,
// at the price of the modules only tests need.
type Prefetcher interface {
	// Prefetch obtains mods as a batch. A problem with one module is not an
	// error here: it surfaces from Fetch, when and if that module is
	// needed. The error is for the batch as a whole, such as a Source that
	// cannot run at all, and is advisory, since Fetch reports it again.
	Prefetch(ctx context.Context, mods []module.Version) error
}

// GoCommand is a Source that runs the go command, which handles GOPROXY,
// GOPRIVATE, the checksum database and credentials on the tool's behalf. It
// writes only under GOMODCACHE: every command runs outside any module, with
// GOWORK=off, so the go.mod, go.sum and go.work of the repository are never
// read, let alone written.
type GoCommand struct {
	// Go is the binary to run; "go" from PATH when empty.
	Go string
	// Env is appended to the environment the commands inherit.
	Env []string
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
	out, stderr, err := g.run(ctx, "list", "-m", "-e", "-json", path+"@"+query)
	var m struct {
		Path, Version string
		Error         *struct{ Err string }
	}
	if jerr := json.Unmarshal(out, &m); jerr != nil || m.Path == "" {
		return module.Version{}, g.failure("go list -m", path, query, stderr, err)
	}
	if m.Error != nil {
		return module.Version{}, g.failure("go list -m", path, query, m.Error.Err, nil)
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

// Prefetch implements Prefetcher with one go mod download for every version
// in mods that no Fetch or Prefetch has taken on yet.
func (g *GoCommand) Prefetch(ctx context.Context, mods []module.Version) error {
	owned := g.claim(mods)
	if len(owned) == 0 {
		return nil
	}
	list := slices.SortedFunc(keys(owned), func(a, b module.Version) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Version, b.Version)
	})
	results, err := g.download(ctx, list)
	for mod, f := range owned {
		f.mod, f.err = results[mod].unpack(err)
		close(f.done)
	}
	return err
}

// keys iterates over the keys of m.
func keys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
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
	what := "go mod download"
	if len(mods) == 1 {
		what += " " + mods[0].Path + "@" + mods[0].Version
	} else {
		what += fmt.Sprintf(" (%d modules)", len(mods))
	}
	args := []string{"mod", "download", "-json"}
	for _, mod := range mods {
		if g.Stderr != nil {
			fmt.Fprintf(g.Stderr, "downloading %s %s\n", mod.Path, mod.Version)
		}
		args = append(args, mod.Path+"@"+mod.Version)
	}
	out, stderr, err := g.run(ctx, args...)

	results := map[module.Version]result{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var m struct {
			Path, Version, Error string
			GoMod, Dir, Sum      string
		}
		if derr := dec.Decode(&m); derr != nil || m.Path == "" {
			break
		}
		mod := module.Version{Path: m.Path, Version: m.Version}
		switch {
		case m.Error != "":
			results[mod] = result{err: g.failure("go mod download", m.Path, m.Version, m.Error, nil)}
		case m.Dir == "" || m.GoMod == "":
			results[mod] = result{err: fmt.Errorf("go mod download %s@%s: no directory in its report", m.Path, m.Version)}
		default:
			gomod, rerr := os.ReadFile(m.GoMod)
			if rerr != nil {
				results[mod] = result{err: fmt.Errorf("go mod download %s@%s: %w", m.Path, m.Version, rerr)}
				continue
			}
			results[mod] = result{mod: &Module{Version: mod, Dir: m.Dir, GoMod: gomod, Sum: m.Sum}}
		}
	}
	if len(results) == 0 {
		if stderr = strings.TrimSpace(stderr); stderr != "" {
			err = errors.New(stderr)
		} else if err == nil {
			err = errors.New("unexpected output")
		}
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return results, nil
}

// run executes the go command with args and returns its standard output
// and error, and the error from running it. See GoCommand for the
// environment it runs in.
func (g *GoCommand) run(ctx context.Context, args ...string) (stdout []byte, stderr string, err error) {
	bin := g.Go
	if bin == "" {
		bin = "go"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// The root of the filesystem is the one directory with no go.mod above
	// it: from there the go command cannot find, and so cannot touch, the
	// module the user is in.
	cmd.Dir = rootDir()
	cmd.Env = append(os.Environ(), "GOWORK=off", "GO111MODULE=on")
	cmd.Env = append(cmd.Env, g.Env...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if g.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&errb, g.Stderr)
	}
	err = cmd.Run()
	return out.Bytes(), errb.String(), err
}

// failure builds the error for a failed command: a NotFoundError when the
// go command's message says the module or version does not exist, else the
// message itself, or the process error when there is no message.
func (g *GoCommand) failure(what, path, version, msg string, err error) error {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		if err == nil {
			err = errors.New("unexpected output")
		}
		return fmt.Errorf("%s %s@%s: %w", what, path, version, err)
	}
	reason := errors.New(msg)
	for _, s := range []string{"not found", "unknown revision", "no matching versions", "404"} {
		if strings.Contains(msg, s) {
			return &NotFoundError{Path: path, Query: version, Err: reason}
		}
	}
	return fmt.Errorf("%s %s@%s: %w", what, path, version, reason)
}

// rootDir is the root of the filesystem holding the working directory: "/"
// on Unix, the current drive's root on Windows.
func rootDir() string {
	wd, _ := os.Getwd()
	return filepath.VolumeName(wd) + string(filepath.Separator)
}

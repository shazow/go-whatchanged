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
	"strings"
	"sync"

	"golang.org/x/mod/module"
)

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
	// Stderr receives what the go command prints on its standard error,
	// such as "go: downloading ..." progress lines; nil discards it. The
	// text is also kept for error messages.
	Stderr io.Writer

	mu      sync.Mutex
	fetches map[module.Version]*fetch
}

// fetch is one Fetch in flight or done, so that concurrent callers share a
// single go mod download per module version.
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
	g.mu.Lock()
	if g.fetches == nil {
		g.fetches = map[module.Version]*fetch{}
	}
	f, ok := g.fetches[mod]
	if !ok {
		f = &fetch{done: make(chan struct{})}
		g.fetches[mod] = f
	}
	g.mu.Unlock()
	if ok {
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return f.mod, f.err
	}
	f.mod, f.err = g.download(ctx, mod)
	close(f.done)
	return f.mod, f.err
}

func (g *GoCommand) download(ctx context.Context, mod module.Version) (*Module, error) {
	out, stderr, err := g.run(ctx, "mod", "download", "-json", mod.Path+"@"+mod.Version)
	var m struct {
		Path, Version, Error string
		GoMod, Dir, Sum      string
	}
	// go mod download reports a failed download in the JSON itself and
	// exits non-zero; the JSON is the better account when it is there.
	if jerr := json.Unmarshal(out, &m); jerr != nil || m.Path == "" {
		return nil, g.failure("go mod download", mod.Path, mod.Version, stderr, err)
	}
	if m.Error != "" {
		return nil, g.failure("go mod download", mod.Path, mod.Version, m.Error, nil)
	}
	if m.Dir == "" || m.GoMod == "" {
		return nil, fmt.Errorf("go mod download %s@%s: no directory in its report", mod.Path, mod.Version)
	}
	gomod, err := os.ReadFile(m.GoMod)
	if err != nil {
		return nil, fmt.Errorf("go mod download %s@%s: %w", mod.Path, mod.Version, err)
	}
	return &Module{
		Version: module.Version{Path: m.Path, Version: m.Version},
		Dir:     m.Dir,
		GoMod:   gomod,
		Sum:     m.Sum,
	}, nil
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

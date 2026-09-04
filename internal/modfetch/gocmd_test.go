package modfetch

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/mod/module"
)

// fakeGo is a shell script standing in for the go command: it logs each
// invocation with its working directory and GOWORK, and answers each
// module argument with the JSON the real command would print, carrying on
// past a failure as go mod download does.
const fakeGo = `#!/bin/sh
printf '%s\n' "$PWD GOWORK=$GOWORK $*" >>"$GOFAKE_LOG"
rc=0
for arg; do
	case "$arg" in
	example.com/m@latest) echo '{"Path":"example.com/m","Version":"v1.2.0"}' ;;
	example.com/m@nope) echo '{"Path":"example.com/m","Error":{"Err":"no matching versions for query \"nope\""}}' ;;
	example.com/m@v1.2.0) printf '{"Path":"example.com/m","Version":"v1.2.0","GoMod":"%s","Dir":"%s"}\n' "$GOFAKE_GOMOD" "$GOFAKE_DIR" ;;
	example.com/m@v9.9.9) echo '{"Path":"example.com/m","Version":"v9.9.9","Error":"example.com/m@v9.9.9: reading https://proxy/example.com/m/@v/v9.9.9.info: 404 Not Found"}'; rc=1 ;;
	*@*) echo "go: cannot serve $arg" >&2; exit 1 ;;
	esac
done
exit $rc
`

// newFakeGo puts the fake go command first in PATH and returns a GoCommand,
// a reader of the fake's invocation log, and the GoCommand's Stderr.
func newFakeGo(t *testing.T) (g *GoCommand, log func() []string, stderr *bytes.Buffer) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake go command is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := filepath.Join(dir, "v1.2.0.mod")
	if err := os.WriteFile(gomod, []byte("module example.com/m\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "log")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOFAKE_LOG", logFile)
	t.Setenv("GOFAKE_GOMOD", gomod)
	t.Setenv("GOFAKE_DIR", filepath.Join(dir, "m@v1.2.0"))
	stderr = &bytes.Buffer{}
	log = func() []string {
		data, _ := os.ReadFile(logFile)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return &GoCommand{Stderr: stderr}, log, stderr
}

func TestGoCommandResolve(t *testing.T) {
	g, log, _ := newFakeGo(t)
	ctx := t.Context()

	// A canonical version resolves to itself without running anything.
	v, err := g.Resolve(ctx, "example.com/m", "v1.0.0")
	if err != nil || v != (module.Version{Path: "example.com/m", Version: "v1.0.0"}) {
		t.Errorf("Resolve(v1.0.0) = %v, %v", v, err)
	}
	if lines := log(); len(lines) != 1 || lines[0] != "" {
		t.Errorf("canonical version ran the go command: %q", lines)
	}

	v, err = g.Resolve(ctx, "example.com/m", "latest")
	if err != nil || v != (module.Version{Path: "example.com/m", Version: "v1.2.0"}) {
		t.Errorf("Resolve(latest) = %v, %v", v, err)
	}
	wd, _ := os.Getwd()
	root := filepath.VolumeName(wd) + string(filepath.Separator)
	if line := log()[0]; !strings.HasPrefix(line, root+" GOWORK=off list -m -e -json example.com/m@latest") {
		t.Errorf("go ran as %q; want it at the filesystem root with GOWORK=off", line)
	}

	_, err = g.Resolve(ctx, "example.com/m", "nope")
	if err == nil || err.Error() != `go list -m example.com/m@nope: no matching versions for query "nope"` {
		t.Errorf("Resolve(nope) = %v", err)
	}
}

func TestGoCommandFetch(t *testing.T) {
	g, log, stderr := newFakeGo(t)
	ctx := t.Context()

	// Concurrent fetches of one version share a single download.
	const n = 5
	var wg sync.WaitGroup
	mods := make([]*Module, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			mods[i], errs[i] = g.Fetch(ctx, module.Version{Path: "example.com/m", Version: "v1.2.0"})
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if mods[i] != mods[0] {
			t.Errorf("fetch %d returned a different module", i)
		}
	}
	m := mods[0]
	if m.Path != "example.com/m" || m.Version.Version != "v1.2.0" || m.FS != nil ||
		filepath.Base(m.Dir) != "m@v1.2.0" || string(m.GoMod) != "module example.com/m\n\ngo 1.24\n" {
		t.Errorf("Fetch = %+v", m)
	}
	if lines := log(); len(lines) != 1 || !strings.HasSuffix(lines[0], "mod download -json example.com/m@v1.2.0") {
		t.Errorf("downloads = %q, want one", lines)
	}
	if got := strings.Count(stderr.String(), "downloading example.com/m v1.2.0\n"); got != 1 {
		t.Errorf("progress lines = %d, want 1:\n%s", got, stderr.String())
	}

	// A module the go command cannot get is reported in the go command's
	// words.
	_, err := g.Fetch(ctx, module.Version{Path: "example.com/m", Version: "v9.9.9"})
	if err == nil || !strings.HasPrefix(err.Error(), "go mod download example.com/m@v9.9.9: example.com/m@v9.9.9: reading ") {
		t.Errorf("Fetch(v9.9.9) = %v", err)
	}

	// Anything else is the go command's stderr, which also reaches Stderr.
	_, err = g.Fetch(ctx, module.Version{Path: "example.com/other", Version: "v1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "go mod download example.com/other@v1.0.0: go: cannot serve") {
		t.Errorf("Fetch(other) = %v", err)
	}
	if !strings.Contains(stderr.String(), "cannot serve") {
		t.Errorf("stderr = %q; want the go command's output", stderr.String())
	}
}

// TestGoCommandNotOnPATH checks that a missing go command is reported as
// such, with what it is needed for, rather than as an exec failure.
func TestGoCommandNotOnPATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	g := &GoCommand{}
	_, err := g.Fetch(t.Context(), module.Version{Path: "example.com/m", Version: "v1.2.0"})
	if err == nil || err.Error() != "go mod download example.com/m@v1.2.0: the go command is not on PATH; go-whatchanged runs it to download modules" {
		t.Errorf("Fetch = %v", err)
	}
	_, err = g.Resolve(t.Context(), "example.com/m", "latest")
	if err == nil || err.Error() != "go list -m example.com/m@latest: the go command is not on PATH; go-whatchanged runs it to download modules" {
		t.Errorf("Resolve = %v", err)
	}
}

func TestGoCommandPrefetch(t *testing.T) {
	g, log, stderr := newFakeGo(t)
	ctx := t.Context()
	good := module.Version{Path: "example.com/m", Version: "v1.2.0"}
	bad := module.Version{Path: "example.com/m", Version: "v9.9.9"}

	// One command for the batch; a module it cannot get is not the batch's
	// error.
	if err := g.Prefetch(ctx, []module.Version{bad, good}); err != nil {
		t.Fatal(err)
	}
	if lines := log(); len(lines) != 1 || !strings.HasSuffix(lines[0], "mod download -json example.com/m@v1.2.0 example.com/m@v9.9.9") {
		t.Errorf("commands = %q, want one for both modules", lines)
	}
	if got := strings.Count(stderr.String(), "downloading example.com/m "); got != 2 {
		t.Errorf("progress lines = %d, want 2:\n%s", got, stderr.String())
	}

	// Fetch answers from the batch without running anything more, the
	// failure included.
	m, err := g.Fetch(ctx, good)
	if err != nil || filepath.Base(m.Dir) != "m@v1.2.0" {
		t.Errorf("Fetch(good) = %+v, %v", m, err)
	}
	if _, err := g.Fetch(ctx, bad); err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("Fetch(bad) = %v, want the go command's account", err)
	}
	// Nor does a second Prefetch of the same versions.
	if err := g.Prefetch(ctx, []module.Version{good, bad}); err != nil {
		t.Fatal(err)
	}
	if lines := log(); len(lines) != 1 {
		t.Errorf("commands after the batch = %q, want no more", lines)
	}

	// A batch that produces no report at all is the batch's error, and
	// each module's.
	other := module.Version{Path: "example.com/other", Version: "v1.0.0"}
	err = g.Prefetch(ctx, []module.Version{other})
	if err == nil || !strings.Contains(err.Error(), "go mod download example.com/other@v1.0.0: go: cannot serve") {
		t.Errorf("Prefetch(other) = %v", err)
	}
	if _, ferr := g.Fetch(ctx, other); ferr == nil || ferr.Error() != err.Error() {
		t.Errorf("Fetch(other) = %v, want the batch's error", ferr)
	}
}

package modfetch

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/mod/module"
)

// fakeGo is a shell script standing in for the go command: it logs each
// invocation with its working directory and GOWORK, and answers the queries
// the tests make with the JSON the real command would print.
const fakeGo = `#!/bin/sh
printf '%s\n' "$PWD GOWORK=$GOWORK $*" >>"$GOFAKE_LOG"
for last; do :; done
case "$last" in
example.com/m@latest) echo '{"Path":"example.com/m","Version":"v1.2.0"}' ;;
example.com/m@nope) echo '{"Path":"example.com/m","Error":{"Err":"no matching versions for query \"nope\""}}' ;;
example.com/m@v1.2.0) printf '{"Path":"example.com/m","Version":"v1.2.0","GoMod":"%s","Dir":"%s","Sum":"h1:abc"}\n' "$GOFAKE_GOMOD" "$GOFAKE_DIR" ;;
example.com/m@v9.9.9) echo '{"Path":"example.com/m","Version":"v9.9.9","Error":"example.com/m@v9.9.9: reading https://proxy/example.com/m/@v/v9.9.9.info: 404 Not Found"}'; exit 1 ;;
*) echo "go: cannot serve $*" >&2; exit 1 ;;
esac
`

func newFakeGo(t *testing.T) (g *GoCommand, log func() []string, stderr *bytes.Buffer) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake go command is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "go")
	if err := os.WriteFile(bin, []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "log")
	gomod := filepath.Join(dir, "v1.2.0.mod")
	if err := os.WriteFile(gomod, []byte("module example.com/m\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr = &bytes.Buffer{}
	g = &GoCommand{
		Go:     bin,
		Env:    []string{"GOFAKE_LOG=" + logFile, "GOFAKE_GOMOD=" + gomod, "GOFAKE_DIR=" + filepath.Join(dir, "m@v1.2.0")},
		Stderr: stderr,
	}
	log = func() []string {
		data, _ := os.ReadFile(logFile)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return g, log, stderr
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
	line := log()[0]
	if !strings.HasPrefix(line, rootDir()+" GOWORK=off list -m -e -json example.com/m@latest") {
		t.Errorf("go ran as %q; want it at the filesystem root with GOWORK=off", line)
	}

	_, err = g.Resolve(ctx, "example.com/m", "nope")
	var nf *NotFoundError
	if !errors.As(err, &nf) || nf.Path != "example.com/m" || nf.Query != "nope" {
		t.Errorf("Resolve(nope) = %v, want NotFoundError", err)
	}
	if !strings.Contains(err.Error(), `no matching versions for query "nope"`) {
		t.Errorf("Resolve(nope) = %v; want the go command's account", err)
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
	if m.Path != "example.com/m" || m.Version.Version != "v1.2.0" || m.Sum != "h1:abc" || m.FS != nil ||
		filepath.Base(m.Dir) != "m@v1.2.0" || string(m.GoMod) != "module example.com/m\n\ngo 1.24\n" {
		t.Errorf("Fetch = %+v", m)
	}
	if lines := log(); len(lines) != 1 || !strings.HasSuffix(lines[0], "mod download -json example.com/m@v1.2.0") {
		t.Errorf("downloads = %q, want one", lines)
	}
	if got := strings.Count(stderr.String(), "downloading example.com/m v1.2.0\n"); got != 1 {
		t.Errorf("progress lines = %d, want 1:\n%s", got, stderr.String())
	}

	_, err := g.Fetch(ctx, module.Version{Path: "example.com/m", Version: "v9.9.9"})
	var nf *NotFoundError
	if !errors.As(err, &nf) || nf.Query != "v9.9.9" {
		t.Errorf("Fetch(v9.9.9) = %v, want NotFoundError", err)
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

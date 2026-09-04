// Package modfetch obtains module versions from outside the repository: a
// Source decides the mechanism, the go command today, callers see versions
// and trees. It is the one place go-whatchanged reaches the network or
// writes to disk, and a run without a Source does neither.
package modfetch

import (
	"context"
	"fmt"

	"golang.org/x/mod/module"

	"github.com/shazow/go-whatchanged/internal/vfs"
)

// Source obtains module versions. A nil Source keeps the tool in its
// read-only mode: a module missing from the module cache is an error rather
// than a download.
type Source interface {
	// Resolve turns a query into the version it denotes: a semantic
	// version, "latest", a branch or tag name, a commit prefix, or a
	// comparison such as "<v1.5.0". A canonical version resolves to itself,
	// so a Source that cannot evaluate queries may still serve exact ones.
	Resolve(ctx context.Context, path, query string) (module.Version, error)

	// Fetch obtains one module version and says where its tree is readable.
	// It is idempotent and safe for concurrent use: the two sides of a diff
	// load in parallel and may ask for the same dependency at once.
	Fetch(ctx context.Context, mod module.Version) (*Module, error)
}

// Module is a fetched module version.
type Module struct {
	module.Version

	// Dir is the absolute path of the module root. With FS nil it is a
	// directory on the host. Otherwise it is the synthetic path FS must be
	// mounted at, and FS serves the module root at ".".
	Dir string
	FS  vfs.FS

	// GoMod is the module's go.mod as the Source knows it: the file from
	// the tree, or the one the ecosystem synthesizes for versions that
	// predate modules. Resolution should read this rather than Dir, so that
	// a module without a go.mod of its own still resolves.
	GoMod []byte

	// Sum is the h1: hash of the tree when the Source verified it, else "".
	Sum string
}

// NotFoundError reports a module or version the Source cannot find, as
// opposed to a transport or verification failure.
type NotFoundError struct {
	Path, Query string
	Err         error // the Source's own account, if any
}

func (e *NotFoundError) Error() string {
	msg := fmt.Sprintf("%s@%s not found", e.Path, e.Query)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *NotFoundError) Unwrap() error { return e.Err }

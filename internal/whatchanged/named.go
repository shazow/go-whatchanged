package whatchanged

import (
	"go/format"
	"go/types"
	"strings"
)

// declString renders obj as it would appear in source, formatted as gofmt
// formats it: "func Open(path string) (*Client, error)", "type Options
// struct{ Timeout int }", "const Version = 1". go/types prints methods as
// "func (*T).M(...)"; this prints the conventional "func (r *T) M(...)"
// instead, adds the value of a constant, which is what a "value changed"
// message is about, and leaves the type off an untyped constant, which has
// none in source. A struct field is rendered as it is declared in its
// struct, "Timeout int", or its type alone when embedded; the struct's
// name is carried separately, see structOf.
func declString(obj types.Object, pkg *types.Package) string {
	// Qualify foreign types by package name, as source does ("apidiff.Report"
	// rather than "golang.org/x/exp/apidiff.Report").
	qual := func(p *types.Package) string {
		if p == pkg {
			return ""
		}
		return p.Name()
	}
	switch o := obj.(type) {
	case *types.Const:
		if b, ok := o.Type().(*types.Basic); ok && b.Info()&types.IsUntyped != 0 {
			return gofmt("const " + o.Name() + " = " + o.Val().String())
		}
		return gofmt(types.ObjectString(obj, qual) + " = " + o.Val().String())
	case *types.Var:
		if o.IsField() {
			t := strings.TrimPrefix(gofmt("var _ "+types.TypeString(o.Type(), qual)), "var _ ")
			if o.Embedded() {
				return t
			}
			return o.Name() + " " + t
		}
	case *types.Func:
		if sig := o.Signature(); sig.Recv() != nil {
			recv := sig.Recv()
			name := ""
			if recv.Name() != "" && recv.Name() != "_" {
				name = recv.Name() + " "
			}
			return gofmt("func (" + name + types.TypeString(recv.Type(), qual) + ") " + o.Name() +
				strings.TrimPrefix(types.TypeString(sig, qual), "func"))
		}
	}
	return gofmt(types.ObjectString(obj, qual))
}

// gofmt formats a declaration as gofmt would: "struct{ Timeout int }"
// rather than go/types's "struct{Timeout int}", and a struct or interface
// with several members on several lines, indented with tabs. A
// declaration the parser rejects is returned as it is.
func gofmt(decl string) string {
	const pkg = "package p\n\n"
	out, err := format.Source([]byte(pkg + decl + "\n"))
	if err != nil {
		return decl
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(out), pkg), "\n")
}

// structOf returns the struct a field belongs to, "Config" for the field
// Config.Timeout, and "" for any other object. A generic struct's name
// comes without its type parameters, as lookupSymbol takes it.
func structOf(obj types.Object, sym string) string {
	v, ok := obj.(*types.Var)
	if !ok || !v.IsField() {
		return ""
	}
	recv := sym[:strings.LastIndex(sym, ".")]
	recv, _, _ = strings.Cut(recv, "[")
	return recv
}

// lookupSymbol resolves the symbol forms apidiff emits: "Name", "T.M",
// "(*T).M" and "T.F" for a struct field. Package-level declarations,
// methods and fields are returned; anything else (a symbol qualified with
// another package, a "method set of" note) resolves to nil.
func lookupSymbol(pkg *types.Package, sym string) types.Object {
	var recv, name string
	ptr := false
	if rest, ok := strings.CutPrefix(sym, "(*"); ok {
		if recv, name, ok = strings.Cut(rest, ")."); !ok {
			return nil
		}
		ptr = true
	} else if i := strings.LastIndex(sym, "."); i >= 0 {
		recv, name = sym[:i], sym[i+1:]
	} else {
		name = sym
	}
	if recv == "" {
		return pkg.Scope().Lookup(name)
	}
	// Generic receivers render as "List[T]"; the type name is the part
	// before the type argument list.
	recv, _, _ = strings.Cut(recv, "[")
	tn, ok := pkg.Scope().Lookup(recv).(*types.TypeName)
	if !ok {
		return nil
	}
	t := tn.Type()
	if ptr {
		t = types.NewPointer(t)
	}
	obj, _, _ := types.LookupFieldOrMethod(t, true, pkg, name)
	switch obj.(type) {
	case *types.Func, *types.Var:
		return obj
	}
	return nil
}

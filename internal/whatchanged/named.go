package whatchanged

import (
	"go/types"
	"strings"
)

// declString renders obj as it would appear in source, for example "func
// Open(path string) (*Client, error)" or "field Point.Z int", where sym is
// the symbol as apidiff names it ("Point.Z"). go/types prints methods as
// "func (*T).M(...)"; this prints the conventional "func (r *T) M(...)"
// instead, names the struct a field belongs to, which go/types leaves out
// and the line would otherwise not say, and adds the value of a constant,
// which is what a "value changed" message is about.
func declString(obj types.Object, pkg *types.Package, sym string) string {
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
		return types.ObjectString(obj, qual) + " = " + o.Val().String()
	case *types.Var:
		if o.IsField() {
			return "field " + sym + " " + types.TypeString(o.Type(), qual)
		}
		return types.ObjectString(obj, qual)
	}
	f, ok := obj.(*types.Func)
	if !ok {
		return types.ObjectString(obj, qual)
	}
	sig, ok := f.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return types.ObjectString(obj, qual)
	}
	recv := sig.Recv()
	var b strings.Builder
	b.WriteString("func (")
	if recv.Name() != "" && recv.Name() != "_" {
		b.WriteString(recv.Name())
		b.WriteString(" ")
	}
	b.WriteString(types.TypeString(recv.Type(), qual))
	b.WriteString(") ")
	b.WriteString(f.Name())
	b.WriteString(strings.TrimPrefix(types.TypeString(sig, qual), "func"))
	return b.String()
}

// lookupSymbol resolves the symbol forms apidiff emits: "Name", "T.M",
// "(*T).M" and "T.F" for a struct field. Package-level declarations,
// methods and fields are returned; anything else (a symbol qualified with
// another package, a "method set of" note) resolves to nil.
func lookupSymbol(pkg *types.Package, sym string) types.Object {
	var recv, name string
	ptr := false
	switch {
	case strings.HasPrefix(sym, "(*"):
		i := strings.Index(sym, ").")
		if i < 0 {
			return nil
		}
		recv, name, ptr = sym[2:i], sym[i+2:], true
	default:
		if i := strings.LastIndex(sym, "."); i >= 0 {
			recv, name = sym[:i], sym[i+1:]
		} else {
			name = sym
		}
	}
	if recv == "" {
		return pkg.Scope().Lookup(name)
	}
	// Generic receivers render as "List[T]"; the type name is the part
	// before the type argument list.
	if i := strings.Index(recv, "["); i >= 0 {
		recv = recv[:i]
	}
	tn, ok := pkg.Scope().Lookup(recv).(*types.TypeName)
	if !ok {
		return nil
	}
	var t types.Type = tn.Type()
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

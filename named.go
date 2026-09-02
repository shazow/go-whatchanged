package whatchanged

import (
	"go/types"
	"strings"
)

// namedForms returns the declaration-style strings of the symbol a
// "changed from X to Y" message refers to, on both sides, for example
// "func Open(path string) (*Client, error)". It returns empty strings when
// the message is not about a whole object (a struct field, say), so the
// renderer falls back to the anonymous type in the message.
func namedForms(old, nw *types.Package, msg string) (before, after string) {
	if !strings.Contains(msg, "changed from ") {
		return "", ""
	}
	sym, _, ok := strings.Cut(msg, ": ")
	if !ok {
		return "", ""
	}
	oldObj := lookupSymbol(old, sym)
	newObj := lookupSymbol(nw, sym)
	if oldObj == nil || newObj == nil {
		return "", ""
	}
	return types.ObjectString(oldObj, types.RelativeTo(old)), types.ObjectString(newObj, types.RelativeTo(nw))
}

// lookupSymbol resolves the symbol forms apidiff emits: "Name", "T.M" and
// "(*T).M". Only whole objects are returned: package-level declarations and
// methods. Struct fields resolve to nil.
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
	if f, ok := obj.(*types.Func); ok {
		return f
	}
	return nil
}

// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements printing of types.

package types2

import (
	"bytes"
	"strconv"
	"unicode/utf8"
)

// A Qualifier controls how named package-level objects are printed in
// calls to TypeString, ObjectString, and SelectionString.
//
// These three formatting routines call the Qualifier for each
// package-level object O, and if the Qualifier returns a non-empty
// string p, the object is printed in the form p.O.
// If it returns an empty string, only the object name O is printed.
//
// Using a nil Qualifier is equivalent to using (*Package).Path: the
// object is qualified by the import path, e.g., "encoding/json.Marshal".
//
type Qualifier func(*Package) string

// RelativeTo returns a Qualifier that fully qualifies members of
// all packages other than pkg.
func RelativeTo(pkg *Package) Qualifier {
	if pkg == nil {
		return nil
	}
	return func(other *Package) string {
		if pkg == other {
			return "" // same package; unqualified
		}
		return other.Path()
	}
}

// TypeString returns the string representation of typ.
// The Qualifier controls the printing of
// package-level objects, and may be nil.
func TypeString(typ Type, qf Qualifier) string {
	var buf bytes.Buffer
	WriteType(&buf, typ, qf)
	return buf.String()
}

// WriteType writes the string representation of typ to buf.
// The Qualifier controls the printing of
// package-level objects, and may be nil.
func WriteType(buf *bytes.Buffer, typ Type, qf Qualifier) {
	newTypeWriter(buf, qf).typ(typ)
}

// WriteSignature writes the representation of the signature sig to buf,
// without a leading "func" keyword.
// The Qualifier controls the printing of
// package-level objects, and may be nil.
func WriteSignature(buf *bytes.Buffer, sig *Signature, qf Qualifier) {
	newTypeWriter(buf, qf).signature(sig)
}

type typeWriter struct {
	buf  *bytes.Buffer
	seen map[Type]bool
	qf   Qualifier
	env  *Environment // if non-nil, we are type hashing
}

func newTypeWriter(buf *bytes.Buffer, qf Qualifier) *typeWriter {
	return &typeWriter{buf, make(map[Type]bool), qf, nil}
}

func newTypeHasher(buf *bytes.Buffer, env *Environment) *typeWriter {
	assert(env != nil)
	return &typeWriter{buf, make(map[Type]bool), nil, env}
}

func (w *typeWriter) byte(b byte) {
	if w.env != nil {
		if b == ' ' {
			b = '#'
		}
		w.buf.WriteByte(b)
		return
	}
	w.buf.WriteByte(b)
	if b == ',' || b == ';' {
		w.buf.WriteByte(' ')
	}
}

func (w *typeWriter) string(s string) {
	w.buf.WriteString(s)
}

func (w *typeWriter) error(msg string) {
	if w.env != nil {
		panic(msg)
	}
	w.buf.WriteString("<" + msg + ">")
}

func (w *typeWriter) typ(typ Type) {
	if w.seen[typ] {
		w.error("cycle to " + goTypeName(typ))
		return
	}
	w.seen[typ] = true
	defer delete(w.seen, typ)

	switch t := typ.(type) {
	case nil:
		w.error("nil")

	case *Basic:
		// exported basic types go into package unsafe
		// (currently this is just unsafe.Pointer)
		if isExported(t.name) {
			if obj, _ := Unsafe.scope.Lookup(t.name).(*TypeName); obj != nil {
				w.typeName(obj)
				break
			}
		}
		w.string(t.name)

	case *Array:
		w.byte('[')
		w.string(strconv.FormatInt(t.len, 10))
		w.byte(']')
		w.typ(t.elem)

	case *Slice:
		w.string("[]")
		w.typ(t.elem)

	case *Struct:
		w.string("struct{")
		for i, f := range t.fields {
			if i > 0 {
				w.byte(';')
			}
			// This doesn't do the right thing for embedded type
			// aliases where we should print the alias name, not
			// the aliased type (see issue #44410).
			if !f.embedded {
				w.string(f.name)
				w.byte(' ')
			}
			w.typ(f.typ)
			if tag := t.Tag(i); tag != "" {
				w.byte(' ')
				// TODO(gri) If tag contains blanks, replacing them with '#'
				//           in Environment.TypeHash may produce another tag
				//           accidentally.
				w.string(strconv.Quote(tag))
			}
		}
		w.byte('}')

	case *Pointer:
		w.byte('*')
		w.typ(t.base)

	case *Tuple:
		w.tuple(t, false)

	case *Signature:
		w.string("func")
		w.signature(t)

	case *Union:
		// Unions only appear as (syntactic) embedded elements
		// in interfaces and syntactically cannot be empty.
		if t.Len() == 0 {
			w.error("empty union")
			break
		}
		for i, t := range t.terms {
			if i > 0 {
				w.byte('|')
			}
			if t.tilde {
				w.byte('~')
			}
			w.typ(t.typ)
		}

	case *Interface:
		w.string("interface{")
		first := true
		for _, m := range t.methods {
			if !first {
				w.byte(';')
			}
			first = false
			w.string(m.name)
			w.signature(m.typ.(*Signature))
		}
		for _, typ := range t.embeddeds {
			if !first {
				w.byte(';')
			}
			first = false
			w.typ(typ)
		}
		w.byte('}')

	case *Map:
		w.string("map[")
		w.typ(t.key)
		w.byte(']')
		w.typ(t.elem)

	case *Chan:
		var s string
		var parens bool
		switch t.dir {
		case SendRecv:
			s = "chan "
			// chan (<-chan T) requires parentheses
			if c, _ := t.elem.(*Chan); c != nil && c.dir == RecvOnly {
				parens = true
			}
		case SendOnly:
			s = "chan<- "
		case RecvOnly:
			s = "<-chan "
		default:
			w.error("unknown channel direction")
		}
		w.string(s)
		if parens {
			w.byte('(')
		}
		w.typ(t.elem)
		if parens {
			w.byte(')')
		}

	case *Named:
		w.typePrefix(t)
		w.typeName(t.obj)
		if t.targs != nil {
			// instantiated type
			w.typeList(t.targs.list())
		} else if w.env == nil && t.TypeParams().Len() != 0 { // For type hashing, don't need to format the TParams
			// parameterized type
			w.tParamList(t.TypeParams().list())
		}

	case *TypeParam:
		if t.obj == nil {
			w.error("unnamed type parameter")
			break
		}
		// Optionally write out package for typeparams (like Named).
		// TODO(danscales): this is required for import/export, so
		// we maybe need a separate function that won't be changed
		// for debugging purposes.
		if t.obj.pkg != nil {
			writePackage(w.buf, t.obj.pkg, w.qf)
		}
		w.string(t.obj.name + subscript(t.id))

	case *top:
		w.error("⊤")

	default:
		// For externally defined implementations of Type.
		// Note: In this case cycles won't be caught.
		w.string(t.String())
	}
}

// If w.env is non-nil, typePrefix writes a unique prefix for the named type t
// based on the types already observed by w.env. If w.env is nil, it does
// nothing.
func (w *typeWriter) typePrefix(t *Named) {
	if w.env != nil {
		w.string(strconv.Itoa(w.env.idForType(t)))
	}
}

func (w *typeWriter) typeList(list []Type) {
	w.byte('[')
	for i, typ := range list {
		if i > 0 {
			w.byte(',')
		}
		w.typ(typ)
	}
	w.byte(']')
}

func (w *typeWriter) tParamList(list []*TypeParam) {
	w.byte('[')
	var prev Type
	for i, tpar := range list {
		// Determine the type parameter and its constraint.
		// list is expected to hold type parameter names,
		// but don't crash if that's not the case.
		if tpar == nil {
			w.error("nil type parameter")
			continue
		}
		if i > 0 {
			if tpar.bound != prev {
				// bound changed - write previous one before advancing
				w.byte(' ')
				w.typ(prev)
			}
			w.byte(',')
		}
		prev = tpar.bound
		w.typ(tpar)
	}
	if prev != nil {
		w.byte(' ')
		w.typ(prev)
	}
	w.byte(']')
}

func (w *typeWriter) typeName(obj *TypeName) {
	if obj.pkg != nil {
		writePackage(w.buf, obj.pkg, w.qf)
	}
	w.string(obj.name)
}

func (w *typeWriter) tuple(tup *Tuple, variadic bool) {
	w.byte('(')
	if tup != nil {
		for i, v := range tup.vars {
			if i > 0 {
				w.byte(',')
			}
			// parameter names are ignored for type identity and thus type hashes
			if w.env == nil && v.name != "" {
				w.string(v.name)
				w.byte(' ')
			}
			typ := v.typ
			if variadic && i == len(tup.vars)-1 {
				if s, ok := typ.(*Slice); ok {
					w.string("...")
					typ = s.elem
				} else {
					// special case:
					// append(s, "foo"...) leads to signature func([]byte, string...)
					if t := asBasic(typ); t == nil || t.kind != String {
						w.error("expected string type")
						continue
					}
					w.typ(typ)
					w.string("...")
					continue
				}
			}
			w.typ(typ)
		}
	}
	w.byte(')')
}

func (w *typeWriter) signature(sig *Signature) {
	if sig.TypeParams().Len() != 0 {
		w.tParamList(sig.TypeParams().list())
	}

	w.tuple(sig.params, sig.variadic)

	n := sig.results.Len()
	if n == 0 {
		// no result
		return
	}

	w.byte(' ')
	if n == 1 && (w.env != nil || sig.results.vars[0].name == "") {
		// single unnamed result (if type hashing, name must be ignored)
		w.typ(sig.results.vars[0].typ)
		return
	}

	// multiple or named result(s)
	w.tuple(sig.results, false)
}

// subscript returns the decimal (utf8) representation of x using subscript digits.
func subscript(x uint64) string {
	const w = len("₀") // all digits 0...9 have the same utf8 width
	var buf [32 * w]byte
	i := len(buf)
	for {
		i -= w
		utf8.EncodeRune(buf[i:], '₀'+rune(x%10)) // '₀' == U+2080
		x /= 10
		if x == 0 {
			break
		}
	}
	return string(buf[i:])
}

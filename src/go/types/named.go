// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"go/token"
	"sync"
)

// A Named represents a named (defined) type.
type Named struct {
	check      *Checker
	info       typeInfo       // for cycle detection
	obj        *TypeName      // corresponding declared object for declared types; placeholder for instantiated types
	orig       *Named         // original, uninstantiated type
	fromRHS    Type           // type (on RHS of declaration) this *Named type is derived of (for cycle reporting)
	underlying Type           // possibly a *Named during setup; never a *Named once set up completely
	tparams    *TypeParamList // type parameters, or nil
	targs      *TypeList      // type arguments (after instantiation), or nil
	methods    []*Func        // methods declared for this type (not the method set of this type); signatures are type-checked lazily

	// resolver may be provided to lazily resolve type parameters, underlying, and methods.
	resolver func(*Environment, *Named) (tparams *TypeParamList, underlying Type, methods []*Func)
	once     sync.Once // ensures that tparams, underlying, and methods are resolved before accessing
}

// NewNamed returns a new named type for the given type name, underlying type, and associated methods.
// If the given type name obj doesn't have a type yet, its type is set to the returned named type.
// The underlying type must not be a *Named.
func NewNamed(obj *TypeName, underlying Type, methods []*Func) *Named {
	if _, ok := underlying.(*Named); ok {
		panic("underlying type must not be *Named")
	}
	return (*Checker)(nil).newNamed(obj, nil, underlying, nil, methods)
}

func (t *Named) resolve(env *Environment) *Named {
	if t.resolver == nil {
		return t
	}

	t.once.Do(func() {
		// TODO(mdempsky): Since we're passing t to the resolver anyway
		// (necessary because types2 expects the receiver type for methods
		// on defined interface types to be the Named rather than the
		// underlying Interface), maybe it should just handle calling
		// SetTypeParams, SetUnderlying, and AddMethod instead?  Those
		// methods would need to support reentrant calls though. It would
		// also make the API more future-proof towards further extensions
		// (like SetTypeParams).
		t.tparams, t.underlying, t.methods = t.resolver(env, t)
		t.fromRHS = t.underlying // for cycle detection
	})
	return t
}

// newNamed is like NewNamed but with a *Checker receiver and additional orig argument.
func (check *Checker) newNamed(obj *TypeName, orig *Named, underlying Type, tparams *TypeParamList, methods []*Func) *Named {
	typ := &Named{check: check, obj: obj, orig: orig, fromRHS: underlying, underlying: underlying, tparams: tparams, methods: methods}
	if typ.orig == nil {
		typ.orig = typ
	}
	if obj.typ == nil {
		obj.typ = typ
	}
	// Ensure that typ is always expanded, at which point the check field can be
	// nilled out.
	//
	// Note that currently we cannot nil out check inside typ.under(), because
	// it's possible that typ is expanded multiple times.
	//
	// TODO(rFindley): clean this up so that under is the only function mutating
	//                 named types.
	if check != nil {
		check.later(func() {
			switch typ.under().(type) {
			case *Named:
				panic("unexpanded underlying type")
			}
			typ.check = nil
		})
	}
	return typ
}

// Obj returns the type name for the declaration defining the named type t. For
// instantiated types, this is the type name of the base type.
func (t *Named) Obj() *TypeName {
	return t.orig.obj // for non-instances this is the same as t.obj
}

// _Orig returns the original generic type an instantiated type is derived from.
// If t is not an instantiated type, the result is t.
func (t *Named) _Orig() *Named { return t.orig }

// TODO(gri) Come up with a better representation and API to distinguish
//           between parameterized instantiated and non-instantiated types.

// TypeParams returns the type parameters of the named type t, or nil.
// The result is non-nil for an (originally) parameterized type even if it is instantiated.
func (t *Named) TypeParams() *TypeParamList { return t.resolve(nil).tparams }

// SetTypeParams sets the type parameters of the named type t.
func (t *Named) SetTypeParams(tparams []*TypeParam) { t.resolve(nil).tparams = bindTParams(tparams) }

// TypeArgs returns the type arguments used to instantiate the named type t.
func (t *Named) TypeArgs() *TypeList { return t.targs }

// NumMethods returns the number of explicit methods whose receiver is named type t.
func (t *Named) NumMethods() int { return len(t.resolve(nil).methods) }

// Method returns the i'th method of named type t for 0 <= i < t.NumMethods().
func (t *Named) Method(i int) *Func { return t.resolve(nil).methods[i] }

// SetUnderlying sets the underlying type and marks t as complete.
func (t *Named) SetUnderlying(underlying Type) {
	if underlying == nil {
		panic("underlying type must not be nil")
	}
	if _, ok := underlying.(*Named); ok {
		panic("underlying type must not be *Named")
	}
	t.resolve(nil).underlying = underlying
}

// AddMethod adds method m unless it is already in the method list.
func (t *Named) AddMethod(m *Func) {
	t.resolve(nil)
	if i, _ := lookupMethod(t.methods, m.pkg, m.name); i < 0 {
		t.methods = append(t.methods, m)
	}
}

func (t *Named) Underlying() Type { return t.resolve(nil).underlying }
func (t *Named) String() string   { return TypeString(t, nil) }

// ----------------------------------------------------------------------------
// Implementation

// under returns the expanded underlying type of n0; possibly by following
// forward chains of named types. If an underlying type is found, resolve
// the chain by setting the underlying type for each defined type in the
// chain before returning it. If no underlying type is found or a cycle
// is detected, the result is Typ[Invalid]. If a cycle is detected and
// n0.check != nil, the cycle is reported.
func (n0 *Named) under() Type {
	u := n0.Underlying()

	// If the underlying type of a defined type is not a defined
	// (incl. instance) type, then that is the desired underlying
	// type.
	var n1 *Named
	switch u1 := u.(type) {
	case nil:
		return Typ[Invalid]
	default:
		// common case
		return u
	case *Named:
		// handled below
		n1 = u1
	}

	if n0.check == nil {
		panic("Named.check == nil but type is incomplete")
	}

	// Invariant: after this point n0 as well as any named types in its
	// underlying chain should be set up when this function exits.
	check := n0.check
	n := n0

	seen := make(map[*Named]int) // types that need their underlying resolved
	var path []Object            // objects encountered, for cycle reporting

loop:
	for {
		seen[n] = len(seen)
		path = append(path, n.obj)
		n = n1
		if i, ok := seen[n]; ok {
			// cycle
			check.cycleError(path[i:])
			u = Typ[Invalid]
			break
		}
		u = n.Underlying()
		switch u1 := u.(type) {
		case nil:
			u = Typ[Invalid]
			break loop
		default:
			break loop
		case *Named:
			// Continue collecting *Named types in the chain.
			n1 = u1
		}
	}

	for n := range seen {
		// We should never have to update the underlying type of an imported type;
		// those underlying types should have been resolved during the import.
		// Also, doing so would lead to a race condition (was issue #31749).
		// Do this check always, not just in debug mode (it's cheap).
		if n.obj.pkg != check.pkg {
			panic("imported type with unresolved underlying type")
		}
		n.underlying = u
	}

	return u
}

func (n *Named) setUnderlying(typ Type) {
	if n != nil {
		n.underlying = typ
	}
}

// bestEnv returns the best available environment. In order of preference:
// - the given env, if non-nil
// - the Checker env, if check is non-nil
// - a new environment
func (check *Checker) bestEnv(env *Environment) *Environment {
	if env != nil {
		return env
	}
	if check != nil {
		assert(check.conf.Environment != nil)
		return check.conf.Environment
	}
	return NewEnvironment()
}

// expandNamed ensures that the underlying type of n is instantiated.
// The underlying type will be Typ[Invalid] if there was an error.
func expandNamed(env *Environment, n *Named, instPos token.Pos) (tparams *TypeParamList, underlying Type, methods []*Func) {
	n.orig.resolve(env)

	check := n.check

	if check.validateTArgLen(instPos, n.orig.tparams.Len(), n.targs.Len()) {
		// We must always have an env, to avoid infinite recursion.
		env = check.bestEnv(env)
		h := env.typeHash(n.orig, n.targs.list())
		// ensure that an instance is recorded for h to avoid infinite recursion.
		env.typeForHash(h, n)

		smap := makeSubstMap(n.orig.tparams.list(), n.targs.list())
		underlying = n.check.subst(instPos, n.orig.underlying, smap, env)

		for i := 0; i < n.orig.NumMethods(); i++ {
			origm := n.orig.Method(i)

			// During type checking origm may not have a fully set up type, so defer
			// instantiation of its signature until later.
			m := NewFunc(origm.pos, origm.pkg, origm.name, nil)
			m.hasPtrRecv = origm.hasPtrRecv
			// Setting instRecv here allows us to complete later (we need the
			// instRecv to get targs and the original method).
			m.instRecv = n

			methods = append(methods, m)
		}
	} else {
		underlying = Typ[Invalid]
	}

	// Methods should not escape the type checker API without being completed. If
	// we're in the context of a type checking pass, we need to defer this until
	// later (not all methods may have types).
	completeMethods := func() {
		for _, m := range methods {
			if m.instRecv != nil {
				check.completeMethod(env, m)
			}
		}
	}
	if check != nil {
		check.later(completeMethods)
	} else {
		completeMethods()
	}

	return n.orig.tparams, underlying, methods
}

func (check *Checker) completeMethod(env *Environment, m *Func) {
	assert(m.instRecv != nil)
	rtyp := m.instRecv
	m.instRecv = nil
	m.setColor(black)

	assert(rtyp.TypeArgs().Len() > 0)

	// Look up the original method.
	_, orig := lookupMethod(rtyp.orig.methods, rtyp.obj.pkg, m.name)
	assert(orig != nil)
	if check != nil {
		check.objDecl(orig, nil)
	}
	origSig := orig.typ.(*Signature)
	if origSig.RecvTypeParams().Len() != rtyp.targs.Len() {
		m.typ = origSig // or new(Signature), but we can't use Typ[Invalid]: Funcs must have Signature type
		return          // error reported elsewhere
	}

	smap := makeSubstMap(origSig.RecvTypeParams().list(), rtyp.targs.list())
	sig := check.subst(orig.pos, origSig, smap, env).(*Signature)
	if sig == origSig {
		// No substitution occurred, but we still need to create a copy to hold the
		// instantiated receiver.
		copy := *origSig
		sig = &copy
	}
	sig.recv = NewParam(origSig.recv.pos, origSig.recv.pkg, origSig.recv.name, rtyp)

	m.typ = sig
}

// safeUnderlying returns the underlying of typ without expanding instances, to
// avoid infinite recursion.
//
// TODO(rfindley): eliminate this function or give it a better name.
func safeUnderlying(typ Type) Type {
	if t, _ := typ.(*Named); t != nil {
		return t.resolve(nil).underlying
	}
	return typ.Underlying()
}

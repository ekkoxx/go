// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"fmt"
	"os"

	"cmd/compile/internal/base"
	"cmd/compile/internal/dwarfgen"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/compile/internal/types2"
	"cmd/internal/src"
)

// checkFiles configures and runs the types2 checker on the given
// parsed source files and then returns the result.
func checkFiles(noders []*noder) (posMap, *types2.Package, *types2.Info) {
	if base.SyntaxErrors() != 0 {
		base.ErrorExit()
	}

	// setup and syntax error reporting
	var m posMap
	files := make([]*syntax.File, len(noders))
	for i, p := range noders {
		m.join(&p.posMap)
		files[i] = p.file
	}

	// typechecking
	env := types2.NewEnvironment()
	importer := gcimports{
		env:      env,
		packages: map[string]*types2.Package{"unsafe": types2.Unsafe},
	}
	conf := types2.Config{
		Environment:           env,
		GoVersion:             base.Flag.Lang,
		IgnoreLabels:          true, // parser already checked via syntax.CheckBranches mode
		CompilerErrorMessages: true, // use error strings matching existing compiler errors
		AllowTypeLists:        true, // remove this line once all tests use type set syntax
		Error: func(err error) {
			terr := err.(types2.Error)
			base.ErrorfAt(m.makeXPos(terr.Pos), "%s", terr.Msg)
		},
		Importer: &importer,
		Sizes:    &gcSizes{},
	}
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Implicits:  make(map[syntax.Node]types2.Object),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Inferred:   make(map[syntax.Expr]types2.Inferred),
		// expand as needed
	}

	pkg, err := conf.Check(base.Ctxt.Pkgpath, files, info)

	base.ExitIfErrors()
	if err != nil {
		base.FatalfAt(src.NoXPos, "conf.Check error: %v", err)
	}

	return m, pkg, info
}

// check2 type checks a Go package using types2, and then generates IR
// using the results.
func check2(noders []*noder) {
	m, pkg, info := checkFiles(noders)

	if base.Flag.G < 2 {
		os.Exit(0)
	}

	g := irgen{
		target: typecheck.Target,
		self:   pkg,
		info:   info,
		posMap: m,
		objs:   make(map[types2.Object]*ir.Name),
		typs:   make(map[types2.Type]*types.Type),
	}
	g.generate(noders)

	if base.Flag.G < 3 {
		os.Exit(0)
	}
}

// dictInfo is the dictionary format for an instantiation of a generic function with
// particular shapes. shapeParams, derivedTypes, subDictCalls, and itabConvs describe
// the actual dictionary entries in order, and the remaining fields are other info
// needed in doing dictionary processing during compilation.
type dictInfo struct {
	// Types substituted for the type parameters, which are shape types.
	shapeParams []*types.Type
	// All types derived from those typeparams used in the instantiation.
	derivedTypes []*types.Type
	// Nodes in the instantiation that requires a subdictionary. Includes
	// method and function calls (OCALL), function values (OFUNCINST), method
	// values/expressions (OXDOT).
	subDictCalls []ir.Node
	// Nodes in the instantiation that are a conversion from a typeparam/derived
	// type to a specific interface.
	itabConvs []ir.Node

	// Mapping from each shape type that substitutes a type param, to its
	// type bound (which is also substitued with shapes if it is parameterized)
	shapeToBound map[*types.Type]*types.Type

	// For type switches on nonempty interfaces, a map from OTYPE entries of
	// HasShape type, to the interface type we're switching from.
	type2switchType map[ir.Node]*types.Type

	startSubDict  int // Start of dict entries for subdictionaries
	startItabConv int // Start of dict entries for itab conversions
	dictLen       int // Total number of entries in dictionary
}

// instInfo is information gathered on an shape instantiation of a function.
type instInfo struct {
	fun       *ir.Func // The instantiated function (with body)
	dictParam *ir.Name // The node inside fun that refers to the dictionary param

	dictInfo *dictInfo
}

type irgen struct {
	target *ir.Package
	self   *types2.Package
	info   *types2.Info

	posMap
	objs   map[types2.Object]*ir.Name
	typs   map[types2.Type]*types.Type
	marker dwarfgen.ScopeMarker

	// laterFuncs records tasks that need to run after all declarations
	// are processed.
	laterFuncs []func()

	// exprStmtOK indicates whether it's safe to generate expressions or
	// statements yet.
	exprStmtOK bool

	// types which we need to finish, by doing g.fillinMethods.
	typesToFinalize []*typeDelayInfo

	dnum int // for generating unique dictionary variables

	// Map from a name of function that been instantiated to information about
	// its instantiated function (including dictionary format).
	instInfoMap map[*types.Sym]*instInfo

	// dictionary syms which we need to finish, by writing out any itabconv
	// entries.
	dictSymsToFinalize []*delayInfo

	// True when we are compiling a top-level generic function or method. Use to
	// avoid adding closures of generic functions/methods to the target.Decls
	// list.
	topFuncIsGeneric bool
}

func (g *irgen) later(fn func()) {
	g.laterFuncs = append(g.laterFuncs, fn)
}

type delayInfo struct {
	gf     *ir.Name
	targs  []*types.Type
	sym    *types.Sym
	off    int
	isMeth bool
}

type typeDelayInfo struct {
	typ  *types2.Named
	ntyp *types.Type
}

func (g *irgen) generate(noders []*noder) {
	types.LocalPkg.Name = g.self.Name()
	types.LocalPkg.Height = g.self.Height()
	typecheck.TypecheckAllowed = true

	// Prevent size calculations until we set the underlying type
	// for all package-block defined types.
	types.DeferCheckSize()

	// At this point, types2 has already handled name resolution and
	// type checking. We just need to map from its object and type
	// representations to those currently used by the rest of the
	// compiler. This happens in a few passes.

	// 1. Process all import declarations. We use the compiler's own
	// importer for this, rather than types2's gcimporter-derived one,
	// to handle extensions and inline function bodies correctly.
	//
	// Also, we need to do this in a separate pass, because mappings are
	// instantiated on demand. If we interleaved processing import
	// declarations with other declarations, it's likely we'd end up
	// wanting to map an object/type from another source file, but not
	// yet have the import data it relies on.
	declLists := make([][]syntax.Decl, len(noders))
Outer:
	for i, p := range noders {
		g.pragmaFlags(p.file.Pragma, ir.GoBuildPragma)
		for j, decl := range p.file.DeclList {
			switch decl := decl.(type) {
			case *syntax.ImportDecl:
				g.importDecl(p, decl)
			default:
				declLists[i] = p.file.DeclList[j:]
				continue Outer // no more ImportDecls
			}
		}
	}

	// 2. Process all package-block type declarations. As with imports,
	// we need to make sure all types are properly instantiated before
	// trying to map any expressions that utilize them. In particular,
	// we need to make sure type pragmas are already known (see comment
	// in irgen.typeDecl).
	//
	// We could perhaps instead defer processing of package-block
	// variable initializers and function bodies, like noder does, but
	// special-casing just package-block type declarations minimizes the
	// differences between processing package-block and function-scoped
	// declarations.
	for _, declList := range declLists {
		for _, decl := range declList {
			switch decl := decl.(type) {
			case *syntax.TypeDecl:
				g.typeDecl((*ir.Nodes)(&g.target.Decls), decl)
			}
		}
	}
	types.ResumeCheckSize()

	// 3. Process all remaining declarations.
	for _, declList := range declLists {
		g.decls((*ir.Nodes)(&g.target.Decls), declList)
	}
	g.exprStmtOK = true

	// 4. Run any "later" tasks. Avoid using 'range' so that tasks can
	// recursively queue further tasks. (Not currently utilized though.)
	for len(g.laterFuncs) > 0 {
		fn := g.laterFuncs[0]
		g.laterFuncs = g.laterFuncs[1:]
		fn()
	}

	if base.Flag.W > 1 {
		for _, n := range g.target.Decls {
			s := fmt.Sprintf("\nafter noder2 %v", n)
			ir.Dump(s, n)
		}
	}

	for _, p := range noders {
		// Process linkname and cgo pragmas.
		p.processPragmas()

		// Double check for any type-checking inconsistencies. This can be
		// removed once we're confident in IR generation results.
		syntax.Crawl(p.file, func(n syntax.Node) bool {
			g.validate(n)
			return false
		})
	}

	if base.Flag.Complete {
		for _, n := range g.target.Decls {
			if fn, ok := n.(*ir.Func); ok {
				if fn.Body == nil && fn.Nname.Sym().Linkname == "" {
					base.ErrorfAt(fn.Pos(), "missing function body")
				}
			}
		}
	}

	// Check for unusual case where noder2 encounters a type error that types2
	// doesn't check for (e.g. notinheap incompatibility).
	base.ExitIfErrors()

	typecheck.DeclareUniverse()

	// Create any needed stencils of generic functions
	g.stencil()

	// Remove all generic functions from g.target.Decl, since they have been
	// used for stenciling, but don't compile. Generic functions will already
	// have been marked for export as appropriate.
	j := 0
	for i, decl := range g.target.Decls {
		if decl.Op() != ir.ODCLFUNC || !decl.Type().HasTParam() {
			g.target.Decls[j] = g.target.Decls[i]
			j++
		}
	}
	g.target.Decls = g.target.Decls[:j]

	base.Assertf(len(g.laterFuncs) == 0, "still have %d later funcs", len(g.laterFuncs))
}

func (g *irgen) unhandled(what string, p poser) {
	base.FatalfAt(g.pos(p), "unhandled %s: %T", what, p)
	panic("unreachable")
}

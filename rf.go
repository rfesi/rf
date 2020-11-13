// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"log"
	"os"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"rsc.io/rf/refactor"
)

var showDiff = flag.Bool("diff", false, "show diff instead of writing files")

func usage() {
	fmt.Fprintf(os.Stderr, "usage: rf [-diff] 'old->new'...\n")
	os.Exit(2)
}

func main() {
	log.SetPrefix("rf: ")
	log.SetFlags(0)

	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
	}

	if err := run(".", ".", *showDiff, os.Stdout, os.Stderr, args); err != nil {
		log.Fatal(err)
	}
}

func run(dir, pkg string, showDiff bool, stdout, stderr io.Writer, args []string) error {
	cmd := cmds[args[0]]
	if cmd == nil {
		return fmt.Errorf("unknown command %s", args[0])
	}

	rf, err := refactor.New(dir, pkg)
	if err != nil {
		return err
	}

	snap, err := rf.LoadTyped()
	if err != nil {
		return err
	}
	if refactor.PrintErrors(stderr, snap) {
		return fmt.Errorf("errors loading packages")
	}
	pkgs := snap.Packages()

	rewrite, needImporters, err := cmd(pkgs[0], args[1:])
	if err != nil {
		return err
	}

	if needImporters {
		importers, err := rf.Importers()
		if err != nil {
			return err
		}

		snap, err = rf.LoadTyped(importers...)
		if err != nil {
			return err
		}
		if refactor.PrintErrors(stderr, snap) {
			return fmt.Errorf("errors loading importer packages")
		}
		pkgs = snap.Packages()

		var paths []string
		for _, p := range pkgs {
			paths = append(paths, p.Pkg.PkgPath)
		}

		// Reloaded the packages, so have to re-evaluate the importers.
		// This one should be guaranteed to succeed.
		rewrite, needImporters, err = cmd(pkgs[0], args[1:])
		if err != nil {
			return err
		}
	}

	ep := refactor.NewErrorPrinter(pkgs[0], stderr) // TODO: pkgs instead of pkgs[0]?
	refactor.Apply(rewrite, pkgs, ep)

	if ep.NumErrors() > 0 {
		return fmt.Errorf("errors applying rewrite")
	}

	snap.Gofmt()

	if showDiff {
		d, err := snap.Diff()
		if err != nil {
			return err
		}
		stdout.Write(d)
		return nil
	}

	snap1, err := rf.LoadTyped(snap.Modified()...)
	if err != nil {
		return err
	}
	if refactor.PrintErrors(stderr, snap1) {
		return fmt.Errorf("errors reloading modified packages")
	}

	return snap.Write(stderr)
}

var cmds = map[string]func(*refactor.Package, []string) (refactor.Rewriter, bool, error){
	"mv": cmdMv,
}

var isGoIdent = regexp.MustCompile(`^[\p{L}_][\p{L}\p{Nd}_]*$`)

func topItem(item *refactor.Item) *refactor.Item {
	for item != nil && item.Outer != nil {
		item = item.Outer
	}
	return item
}

func cmdMv(p *refactor.Package, args []string) (rw refactor.Rewriter, needImported bool, err error) {
	if len(args) < 2 {
		return nil, false, fmt.Errorf("usage: mv old... new")
	}

	var items []*refactor.Item
	for _, arg := range args {
		items = append(items, p.Lookup(arg))
	}
	for i, item := range items[:len(items)-1] {
		if item == nil {
			return nil, false, fmt.Errorf("cannot find %s", args[i])
		}
	}

	srcs, dst := items[:len(items)-1], items[len(items)-1]
	if dst != nil && (dst.Kind == refactor.ItemDir || dst.Kind == refactor.ItemFile) {
		for _, item := range srcs {
			if item.Outer == nil && item.Kind != refactor.ItemDir {
				// ok
				continue
			}
			if item.Kind == refactor.ItemMethod && item.Outer.Kind == refactor.ItemType {
				// ok
				continue
			}
			return nil, false, fmt.Errorf("cannot move %s to directory %s", item.Kind, dst.Name)
		}

		// TODO implement
	}

	// Otherwise, renaming to program identifier, which must not exist.
	if len(items) != 2 {
		return nil, false, fmt.Errorf("cannot move multiple items to %s", args[len(args)-1])
	}

	oldItem, newItem := items[0], items[1]
	oldPath, newPath := args[0], args[1]
	if newItem != nil {
		return nil, false, fmt.Errorf("already have %s", newPath)
	}

	var newBase *refactor.Item
	newPrefix, newName, ok := cutLast(newPath, ".")
	if ok {
		newBase = p.Lookup(newPrefix)
		if newBase == nil {
			return nil, false, fmt.Errorf("cannot find %s", newPrefix)
		}
	} else {
		newName = newPath
	}

	// Check that newName is a valid identifier.
	// (Arbitrary syntax would make simple string search for dots invalid, among other problems.)
	if !isGoIdent.MatchString(newName) {
		return nil, false, fmt.Errorf("malformed replacement: not a valid Go identifier: %s", newName)
	}

	r, _ := utf8.DecodeRuneInString(oldPath)
	exported := unicode.IsUpper(r)

	// Straight rename.
	oldBase := oldItem.Outer
	if oldBase == nil && newBase == nil || oldBase != nil && newBase != nil && oldBase.Obj == newBase.Obj {
		lookup := ""
		if newBase == nil {
			lookup = newName
		}
		return &renameIdent{self: p, old: oldItem, lookup: lookup, new: newName}, exported, nil
	}

	// Rename plain global var to field in global struct var.
	newTop := topItem(newBase)
	if oldBase == nil && oldItem.Kind == refactor.ItemVar &&
		newBase != nil && newTop.Kind == refactor.ItemVar && (newBase.Kind == refactor.ItemVar || newBase.Kind == refactor.ItemField) {
		if _, ok := newBase.Obj.(*types.Var).Type().Underlying().(*types.Struct); ok {
			// Finding struct type is a little tricky.
			var structPos token.Pos
			tvar := newBase.Obj.(*types.Var)
			switch typ := tvar.Type().(type) {
			case *types.Struct:
				structPos = tvar.Pos()
			case *types.Named:
				structPos = typ.Obj().Pos()
			}
			// Need to add struct field, and need to replace references.
			rw := MultiRewriter(
				&addStructField{structPos: structPos, name: newName, typ: oldItem.Obj.Type()},
				&removeDecl{target: oldItem.Obj, reportInit: true},
				&renameIdent{self: p, skipDefn: true, old: oldItem, lookup: newTop.Name, lookupOK: newTop.Obj, new: newPath},
			)
			return rw, exported, nil
		}
	}

	// TODO: Rename field in global struct var to plain global var.

	// TODO: Rename global function to method.
	if oldBase == nil && oldItem.Kind == refactor.ItemFunc &&
		newBase != nil && newBase.Kind == refactor.ItemType {
		if _, ok := newBase.Obj.(*types.TypeName); ok {
			// TODO check method set for newName
			// TODO check that first argument of old function is receiver
			// TODO finish
		}
	}

	// Rename method to global function.
	if oldBase.Outer == nil && oldBase.Kind == refactor.ItemType && oldItem.Kind == refactor.ItemMethod && newBase == nil {
		rw := &methodToFunc{target: oldItem.Obj.(*types.Func), name: newName}
		return rw, exported, nil
	}

	// TODO: Rename variable in function to global.

	// TODO: Rename global to variable in function.

	return nil, false, fmt.Errorf("unimplemented replacement: %v -> %v . %v", oldItem, newBase, newName)
}

func pkgFindAll(pkg *refactor.Package, pos token.Pos) []ast.Node {
	for _, file := range pkg.Pkg.Syntax {
		if file.Pos() <= pos && pos < file.End() {
			return findAll(file, pos)
		}
	}
	return nil
}

func pkgFind(pkg *refactor.Package, pos token.Pos, typ interface{}) ast.Node {
	for _, file := range pkg.Pkg.Syntax {
		if file.Pos() <= pos && pos < file.End() {
			return find(file, pos, typ)
		}
	}
	return nil
}

func findAll(n ast.Node, pos token.Pos) []ast.Node {
	var list []ast.Node
	ast.Inspect(n, func(n ast.Node) bool {
		if n != nil && n.Pos() <= pos && pos < n.End() {
			list = append(list, n)
			return true
		}
		return false
	})
	return list
}

func find(n ast.Node, pos token.Pos, typ interface{}) ast.Node {
	var rtyp reflect.Type
	if typ != nil {
		rtyp = reflect.TypeOf(typ)
	}
	var found ast.Node
	ast.Inspect(n, func(n ast.Node) bool {
		if n != nil && n.Pos() <= pos && pos < n.End() {
			if typ != nil && reflect.TypeOf(n) == rtyp {
				found = n
			}
			return true
		}
		return false
	})
	return found
}

type removeDecl struct {
	target     types.Object
	reportInit bool
}

func (r *removeDecl) Rewrite(pkg *refactor.Package, file *ast.File, ep refactor.ErrorPrinter) {
	want := r.target.Pos()
	path := findAll(file, want)
	if path == nil {
		return
	}
	var spec *ast.ValueSpec
	for len(path) > 0 {
		s, ok := path[len(path)-1].(*ast.ValueSpec)
		if ok {
			spec = s
			break
		}
		path = path[:len(path)-1]
	}
	var decl *ast.GenDecl
	for len(path) > 0 {
		d, ok := path[len(path)-1].(*ast.GenDecl)
		if ok {
			decl = d
			break
		}
		path = path[:len(path)-1]
	}
	if spec == nil || decl == nil {
		ep.ErrorAt(want, "internal error - lost track of declaration")
		return
	}

	if len(decl.Specs) == 1 && len(spec.Names) == 1 {
		// Delete entire declaration.
		// TODO: Doc comments too.
		// TODO: Newline too.
		replace(pkg, decl.Pos(), decl.End(), "")
		return
	}

	if len(spec.Names) == 1 {
		replace(pkg, spec.Pos(), spec.End(), "")
		return
	}

	for i, id := range spec.Names {
		if id.Pos() == want {
			if i == 0 {
				replace(pkg, id.Pos(), spec.Names[i+1].Pos(), "")
			} else {
				replace(pkg, spec.Names[i-1].End(), id.End(), "")
			}
			if len(spec.Values) > 0 && r.reportInit {
				ep.ErrorAt(spec.Values[i].Pos(), "cannot move initialization")
			}
			return
		}
	}
	ep.ErrorAt(want, "internal error - lost track of declaration spec")
}

func replace(pkg *refactor.Package, start, end token.Pos, repl string) {
	p1 := pkg.Position(start)
	p2 := pkg.Position(end)
	pkg.Edit(p1.Filename).Replace(p1.Offset, p2.Offset, repl)
}

type addStructField struct {
	structPos token.Pos // var or field of struct type that needs new field
	name      string
	typ       types.Type
}

func (r *addStructField) Rewrite(pkg *refactor.Package, file *ast.File, ep refactor.ErrorPrinter) {
	want := r.structPos
	if want < file.Pos() || file.End() <= want {
		return
	}

	path := findAll(file, want)
	var typ ast.Expr
Loop:
	for i := len(path) - 1; i >= 0; i-- {
		switch p := path[i].(type) {
		case *ast.Field:
			if want >= p.Type.Pos() {
				ep.ErrorAt(want, "internal error - lost track of where to insert struct field 1")
				return
			}
			typ = p.Type
			break Loop
		case *ast.ValueSpec:
			if want >= p.Type.Pos() {
				ep.ErrorAt(want, "internal error - lost track of where to insert struct field 2")
				return
			}
			typ = p.Type
			break Loop
		case *ast.TypeSpec:
			if want >= p.Type.Pos() {
				ep.ErrorAt(want, "internal error - lost track of where to insert struct field 2")
				return
			}
			typ = p.Type
			break Loop
		}
	}
	if typ == nil {
		ep.ErrorAt(want, "internal error - lost track of where to insert struct field 3")
		return
	}
	t, ok := typ.(*ast.StructType)
	if !ok {
		// Failed to find struct.
		ep.ErrorAt(typ.Pos(), "internal error - lost track of where to insert struct field 4")
		return
	}

	fields := t.Fields
	// Do we need a newline before the brace?
	line := func(pos token.Pos) int {
		return pkg.Position(pos).Line
	}
	last := fields.Opening
	if len(fields.List) > 0 {
		last = fields.List[len(fields.List)-1].End()
	}
	nl := ""
	if line(last) == line(fields.Closing) {
		nl = "\n"
	}

	// Copy type from the original global.
	replace(pkg, fields.Closing, fields.Closing, nl+r.name+" "+r.typ.String()+"\n")
}

func MultiRewriter(rewriters ...refactor.Rewriter) refactor.Rewriter {
	return &multiRewriter{append([]refactor.Rewriter{}, rewriters...)}
}

type multiRewriter struct {
	list []refactor.Rewriter
}

func (m *multiRewriter) Rewrite(pkg *refactor.Package, file *ast.File, ep refactor.ErrorPrinter) {
	for _, r := range m.list {
		r.Rewrite(pkg, file, ep)
	}
}

type renameIdent struct {
	self     *refactor.Package
	old      *refactor.Item
	lookup   string
	lookupOK types.Object
	skipDefn bool
	new      string
}

func (r *renameIdent) Rewrite(pkg *refactor.Package, file *ast.File, ep refactor.ErrorPrinter) {
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || (pkg.Pkg.TypesInfo.Defs[id] != r.old.Obj && pkg.Pkg.TypesInfo.Uses[id] != r.old.Obj) {
			return true
		}
		if r.skipDefn && pkg.Pkg.TypesInfo.Defs[id] == r.old.Obj {
			return true
		}

		p1 := pkg.Position(id.Pos())
		p2 := pkg.Position(id.End())
		if pkg == r.self && r.lookup != "" {
			// Double-check that the name is not in scope.
			if found := pkg.LookupAt(r.lookup, id.Pos()); found != nil && found != r.lookupOK {
				ep.ErrorAt(id.Pos(), "cannot rename %s: %s already in scope", r.old.Name, r.lookup)
			}
		}
		pkg.Edit(pkg.File(id.Pos())).Replace(p1.Offset, p2.Offset, r.new)
		return true
	})
}

type methodToFunc struct {
	target *types.Func
	name   string
}

func (r *methodToFunc) Rewrite(pkg *refactor.Package, file *ast.File, ep refactor.ErrorPrinter) {
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if id, ok := n.(*ast.Ident); ok {
			if pkg.Pkg.TypesInfo.Defs[id] == r.target {
				decl := stack[len(stack)-2].(*ast.FuncDecl)
				replace(pkg, decl.Recv.Opening, decl.Recv.Opening, r.name)
				sep := ""
				if len(decl.Type.Params.List) > 0 {
					sep = ", "
				}
				replace(pkg, decl.Recv.Closing, decl.Type.Params.Opening+1, sep)
				// TODO add param names if needed
				return true
			}
			if pkg.Pkg.TypesInfo.Uses[id] == r.target {
				// Is the actual receiver a pointer?
				sig := r.target.Type().(*types.Signature)
				recvType := sig.Recv().Type()
				_, recvPtr := recvType.(*types.Pointer)

				// Is the selector a receiver variable or a type?
				// And is it a pointer?
				sel := stack[len(stack)-2].(*ast.SelectorExpr)
				tv, ok := pkg.Pkg.TypesInfo.Types[sel.X]
				if !ok {
					println("MISSING TYPEI NFO")
				}
				selType := tv.IsType()
				_, selPtr := tv.Type.(*types.Pointer)

				// Is the selector being called?
				call, _ := stack[len(stack)-3].(*ast.CallExpr)

				if call == nil {
					// T.F or x.F or (*T).F or (&x).F
					if !selType {
						ep.ErrorAt(id.Pos(), "cannot rewrite method value using function")
						return true
					}
					if selPtr && !recvPtr {
						ep.ErrorAt(id.Pos(), "cannot use pointer method value with function using non-pointer receiver")
						return true
					}

					// T.F or (*T).F, matching the actual function.
					replace(pkg, sel.Pos(), sel.End(), r.name)
					return true
				}

				// Rewrite call.
				// May need implicit address or star on receiver.
				// TODO: check scope for r.name
				if selType {
					replace(pkg, call.Pos(), call.Lparen, r.name)
					if selPtr && !recvPtr {
						// TODO parens for star
						replace(pkg, call.Lparen+1, call.Lparen+1, "*")
					}
					return true
				}
				replace(pkg, sel.X.Pos(), sel.X.Pos(), r.name+"(")
				if recvPtr && !selPtr {
					replace(pkg, sel.X.Pos(), sel.X.Pos(), "&")
				}
				if selPtr && !recvPtr {
					replace(pkg, sel.X.Pos(), sel.X.Pos(), "*")
				}
				// TODO maybe parens for *
				replace(pkg, sel.X.End(), call.Lparen+1, "")
				if len(call.Args) > 0 {
					replace(pkg, call.Lparen+1, call.Lparen+1, ", ")
				}
				return true
			}
		}
		return true
	})
}

func cut(s, sep string) (before, after string, ok bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

func cutLast(s, sep string) (before, after string, ok bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

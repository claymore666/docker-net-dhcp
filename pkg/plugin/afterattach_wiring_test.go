// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestAfterAttachReadsWhatTheLookupFilled is the drift check the
// behaviour tests cannot make.
//
// WHY IT NEEDS A WIRING TEST. afterAttach's own tests call it directly
// and prove it reads the container name the late lookup writes. Nothing
// in this package executes Start, which is where the argument is
// chosen, so the defect that reached the field lives in a line no test
// runs: MEASURED, restoring the copy at the call site leaves the whole
// of ./pkg/... green while every attach on the sandbox key route
// renames the host link to the empty string again (#1051).
//
// WHAT IT ASSERTS. Every value afterAttach takes by pointer is passed
// as the address of a variable the lookup closure itself assigns. A
// copy taken at the call, or the address of a copy, is the bug: the
// lookup runs inside afterAttach on this route, so anything captured
// before the call still holds what the attach started with.
//
// THE BOUND. Keyed on SPELLING, like the other wiring checks here. A
// variable filled through a helper the closure calls reads as a
// violation though it is correct.
func TestAfterAttachReadsWhatTheLookupFilled(t *testing.T) {
	fset := token.NewFileSet()

	var decl, start *ast.FuncDecl
	for _, file := range []string{"dhcp_manager.go", "host_ifname.go"} {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, d := range parsed.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			switch fn.Name.Name {
			case "afterAttach":
				decl = fn
			case "Start":
				start = fn
			}
		}
	}
	if decl == nil || start == nil {
		t.Fatal("the attach no longer declares both Start and afterAttach: this check has lost its subject")
	}

	// The positions afterAttach takes by pointer, read from its own
	// signature so the check follows a reordered parameter list.
	byPointer := map[int]bool{}
	pos := 0
	for _, field := range decl.Type.Params.List {
		names := len(field.Names)
		if names == 0 {
			names = 1
		}
		star, ok := field.Type.(*ast.StarExpr)
		for i := 0; i < names; i++ {
			if ok {
				if ident, isIdent := star.X.(*ast.Ident); isIdent && ident.Name == "string" {
					byPointer[pos] = true
				}
			}
			pos++
		}
	}
	if len(byPointer) == 0 {
		t.Fatal("afterAttach takes nothing by pointer any more: either the lookup fills nothing it " +
			"reads, or this check is measuring the wrong function")
	}

	// The variables the lookup closure writes. Assignment through the
	// closure is what makes a value late; everything else is known
	// before the attach and could travel by value.
	filled := map[string]bool{}
	var lookup *ast.FuncLit
	ast.Inspect(start, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		name, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || name.Name != "inspect" {
			return true
		}
		if lit, ok := assign.Rhs[0].(*ast.FuncLit); ok {
			lookup = lit
		}
		return true
	})
	if lookup == nil {
		t.Fatal("Start no longer builds the inspect closure: this check has lost the thing it calls late")
	}
	ast.Inspect(lookup, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			if ident, ok := lhs.(*ast.Ident); ok {
				filled[ident.Name] = true
			}
		}
		return true
	})

	var calls []*ast.CallExpr
	ast.Inspect(start, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "afterAttach" {
			calls = append(calls, call)
		}
		return true
	})
	if len(calls) != 1 {
		t.Fatalf("Start calls afterAttach %d times, want exactly one: the attach has one end", len(calls))
	}

	for i, arg := range calls[0].Args {
		if !byPointer[i] {
			continue
		}
		unary, ok := arg.(*ast.UnaryExpr)
		if !ok || unary.Op != token.AND {
			t.Errorf("argument %d is not the address of anything: afterAttach takes it by pointer "+
				"because the lookup fills it after the call", i)
			continue
		}
		ident, ok := unary.X.(*ast.Ident)
		if !ok {
			t.Errorf("argument %d is the address of an expression and not of a variable the lookup "+
				"writes", i)
			continue
		}
		if !filled[ident.Name] {
			t.Errorf("argument %d passes &%s, and the inspect closure never assigns %s: the rename "+
				"would read what the attach started with, which is how every host link kept its "+
				"generated name (#1051)", i, ident.Name, ident.Name)
		}
	}
}

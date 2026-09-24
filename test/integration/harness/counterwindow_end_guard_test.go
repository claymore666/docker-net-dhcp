// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: this guard reads source and runs in the unit job (#405).

package harness

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// windowOpened reports a harness.BeginCounterWindow call, alone or with ExpectRecycle chained on it (#405).
func windowOpened(e ast.Expr) bool {
	for {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		s, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if x, ok := s.X.(*ast.Ident); ok && x.Name == "harness" && s.Sel.Name == "BeginCounterWindow" {
			return true
		}
		e = s.X
	}
}

// endsParam names the suite helpers that call End on a *harness.CounterWindow parameter, so a window handed to one
// counts as closed (#405).
func endsParam(fd *ast.FuncDecl) bool {
	params := map[string]bool{}
	for _, f := range fd.Type.Params.List {
		if st, ok := f.Type.(*ast.StarExpr); ok {
			if sel, ok := st.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "CounterWindow" {
				for _, n := range f.Names {
					params[n.Name] = true
				}
			}
		}
	}
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "End" {
				if id, ok := s.X.(*ast.Ident); ok && params[id.Name] {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// unendedWindows lists every window a function opens and neither closes nor hands to a closing helper. A window is
// keyed by where it opens: End or a helper call closes the latest window opened under that name before it, so a
// reused name still needs one End per window. The window's own cleanup reports this only when the test otherwise
// passed, so a new test shows it on its first green run (#405, #905).
func unendedWindows(fs *token.FileSet, files []*ast.File) []string {
	enders := map[string]bool{}
	for _, af := range files {
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && endsParam(fd) {
				enders[fd.Name.Name] = true
			}
		}
	}
	type window struct {
		name   string
		pos    token.Pos
		closed bool
	}
	var out []string
	for _, af := range files {
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var opened []*window
			latest := map[string]*window{}
			closeLatest := func(e ast.Expr) {
				if id, ok := e.(*ast.Ident); ok && latest[id.Name] != nil {
					latest[id.Name].closed = true
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for i, r := range n.Rhs {
						if i >= len(n.Lhs) || !windowOpened(r) {
							continue
						}
						if id, ok := n.Lhs[i].(*ast.Ident); ok {
							w := &window{name: id.Name, pos: n.Pos()}
							opened = append(opened, w)
							latest[id.Name] = w
						}
					}
				case *ast.CallExpr:
					if s, ok := n.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "End" {
						closeLatest(s.X)
					}
					if f, ok := n.Fun.(*ast.Ident); ok && enders[f.Name] {
						for _, a := range n.Args {
							closeLatest(a)
						}
					}
				}
				return true
			})
			for _, w := range opened {
				if !w.closed {
					p := fs.Position(w.pos)
					out = append(out, fmt.Sprintf("%s:%d %s: window %s", filepath.Base(p.Filename), p.Line, fd.Name.Name, w.name))
				}
			}
		}
	}
	return out
}

func TestCounterWindow_EveryWindowIsEnded(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "*_test.go"))
	if err != nil {
		t.Fatalf("glob suite sources: %v", err)
	}
	fs := token.NewFileSet()
	var files []*ast.File
	opened := 0
	for _, p := range paths {
		af, err := parser.ParseFile(fs, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && windowOpened(c) {
				opened++
			}
			return true
		})
		files = append(files, af)
	}
	if opened == 0 {
		t.Fatal("no harness.BeginCounterWindow call found under ../; this guard would pass vacuously")
	}
	for _, u := range unendedWindows(fs, files) {
		t.Errorf("%s is never closed with End(), directly or through a helper that ends it. "+
			"The window's cleanup fails the test only when everything else in it passed, so a new test "+
			"shows this on its first otherwise green run (#405, #905).", u)
	}
}

func TestCounterWindow_EndGuardWouldCatchAnUnendedWindow(t *testing.T) {
	const src = `package integration

func TestUnended(t *testing.T) {
	w := harness.BeginCounterWindow(t, ctx, cli, "recovered_ok").ExpectRecycle()
	harness.AwaitRecoveryRebuildWindow(w, "x", nil)
}

func TestEndedDirectly(t *testing.T) {
	w := harness.BeginCounterWindow(t, ctx, cli, "recovered_ok")
	w.End()
}

func TestEndedByHelper(t *testing.T) {
	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")
	closeIt(t, bindW)
}

func TestReopenedEndedOnce(t *testing.T) {
	w := harness.BeginCounterWindow(t, ctx, cli, "recovered_ok")
	w = harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")
	w.End()
}

func TestReopenedEndedTwice(t *testing.T) {
	w := harness.BeginCounterWindow(t, ctx, cli, "recovered_ok")
	w.End()
	w = harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")
	w.End()
}

func closeIt(t *testing.T, w *harness.CounterWindow) { w.End() }
`
	fs := token.NewFileSet()
	af, err := parser.ParseFile(fs, "fixture_test.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got := unendedWindows(fs, []*ast.File{af})
	want := []string{"fixture_test.go:4 TestUnended: window w", "fixture_test.go:19 TestReopenedEndedOnce: window w"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("unendedWindows = %q, want %q", got, want)
	}
}

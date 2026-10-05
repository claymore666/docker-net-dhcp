// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// netipEmptyTextCompares lists each `x.String() == ""` or `!= ""` whose x is a netip value: a zero one prints
// "invalid IP" or "invalid Prefix", so the comparison never sees the absence (#178).
func netipEmptyTextCompares(fset *token.FileSet, files []*ast.File, info *types.Info) []string {
	var out []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			be, ok := n.(*ast.BinaryExpr)
			if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
				return true
			}
			if (isEmptyText(be.Y, info) && isNetipText(be.X, info)) || (isEmptyText(be.X, info) && isNetipText(be.Y, info)) {
				out = append(out, fset.Position(be.Pos()).String())
			}
			return true
		})
	}
	return out
}

func isEmptyText(e ast.Expr, info *types.Info) bool {
	tv, ok := info.Types[e]
	return ok && tv.Value != nil && tv.Value.Kind() == constant.String && constant.StringVal(tv.Value) == ""
}

func isNetipText(e ast.Expr, info *types.Info) bool {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "String" {
		return false
	}
	t := info.TypeOf(sel.X)
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "net/netip"
}

type listedPackage struct {
	ImportPath, Dir, Export, Name      string
	GoFiles, TestGoFiles, XTestGoFiles []string
	Module                             *struct{ Main bool }
}

// moduleSources lists this module's packages under the integration tag, a superset of the default view, with the
// export data their imports type-check against (#178).
func moduleSources(t *testing.T) ([]listedPackage, func(string) (io.ReadCloser, error)) {
	t.Helper()
	cmd := exec.Command("go", "list", "-tags", "integration", "-export", "-deps", "-test",
		"-json=ImportPath,Dir,Export,Name,GoFiles,TestGoFiles,XTestGoFiles,Module", "./...")
	cmd.Dir = filepath.Join("..", "..")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, stderr.String())
	}
	exports := map[string]string{}
	var own []listedPackage
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
		if p.Module != nil && p.Module.Main && !strings.Contains(p.ImportPath, " [") && !strings.HasSuffix(p.ImportPath, ".test") {
			own = append(own, p)
		}
	}
	return own, func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(file)
	}
}

func typeCheck(t *testing.T, fset *token.FileSet, path string, files []*ast.File, lookup importer.Lookup) *types.Info {
	t.Helper()
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	if _, err := conf.Check(path, fset, files, info); err != nil {
		t.Fatalf("type-check %s: %v; the gate cannot judge what it cannot type", path, err)
	}
	return info
}

func parseAll(t *testing.T, fset *token.FileSet, dir string, names []string) []*ast.File {
	t.Helper()
	var files []*ast.File
	for _, n := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		files = append(files, f)
	}
	return files
}

func TestNetipText_NoZeroValueIsReadAsEmptyText(t *testing.T) {
	pkgs, lookup := moduleSources(t)
	if len(pkgs) < 5 {
		t.Fatalf("go list named %d packages of this module; the gate is not looking at the tree", len(pkgs))
	}
	var found []string
	for _, p := range pkgs {
		if len(p.XTestGoFiles) > 0 {
			t.Fatalf("%s has external test files, which this gate does not type-check yet", p.ImportPath)
		}
		fset := token.NewFileSet()
		files := parseAll(t, fset, p.Dir, append(append([]string{}, p.GoFiles...), p.TestGoFiles...))
		if len(files) == 0 {
			continue
		}
		testVariant := func(path string) (io.ReadCloser, error) {
			if rc, err := lookup(path + " [" + p.ImportPath + ".test]"); err == nil {
				return rc, nil
			}
			return lookup(path)
		}
		found = append(found, netipEmptyTextCompares(fset, files, typeCheck(t, fset, p.ImportPath, files, testVariant))...)
	}
	for _, at := range found {
		t.Errorf("%s compares a netip value's text with \"\"; a zero value prints \"invalid ...\", use IsValid", at)
	}
}

// The gate finds both operand orders on a value and a pointer, and passes IsValid, another type's String and a
// comparison with other text (#178).
func TestNetipText_TheGateRefusesTheShapeAndPassesItsTwin(t *testing.T) {
	_, lookup := moduleSources(t)
	const refused = `package p
import "net/netip"
func a(x netip.Addr) bool    { return x.String() == "" }
func b(x *netip.Prefix) bool { return "" != (x.String()) }
`
	const twin = `package p
import ("fmt"; "net/netip")
func a(x netip.Addr) bool   { return !x.IsValid() }
func b(x fmt.Stringer) bool { return x.String() == "" }
func c(x netip.Addr) bool   { return x.String() == "invalid IP" }
`
	for _, tc := range []struct {
		name, src string
		want      int
	}{{"refused", refused, 2}, {"twin", twin, 0}} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, tc.name+".go", tc.src, 0)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		files := []*ast.File{f}
		if got := netipEmptyTextCompares(fset, files, typeCheck(t, fset, "p", files, lookup)); len(got) != tc.want {
			t.Errorf("%s: the gate found %d (%v), want %d", tc.name, len(got), got, tc.want)
		}
	}
}

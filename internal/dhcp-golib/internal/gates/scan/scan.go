// Package scan is the shared source-scanning half of the gates. It parses Go
// files with go/parser rather than matching text, because a gate that greps
// for "time.Sleep" is defeated by an import alias, and one that greps for
// `"time"` is defeated by a comment.
package scan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SkipDirs are directory names never walked. testdata holds the gates' own
// deliberate violations and is not compiled by the go tool, so a file inside
// it is not part of any build or any test binary. vendor and .git are not
// this module's source.
var SkipDirs = map[string]bool{
	".git":     true,
	"testdata": true,
	"vendor":   true,
}

// File is one parsed Go source file.
type File struct {
	Path   string
	Fset   *token.FileSet
	Syntax *ast.File
}

// Import is one import declaration, with the local name it binds.
type Import struct {
	Path  string // e.g. "net/netip"
	Local string // the identifier it binds: alias, or the package's own name
	Dot   bool   // imported as `. "path"` — binds every exported identifier
	Blank bool   // imported as `_ "path"` — binds nothing
	Pos   token.Position
}

// GoFiles walks root and returns every path ending in .go, skipping SkipDirs.
//
// It walks the filesystem rather than asking the go tool for the package's
// file list, and that is the point: `go list` honours build constraints, so a
// file carrying a false //go:build tag is invisible to it. Such a file is
// still source in the tree, still gets compiled the day the tag flips, and is
// exactly where a violation would hide from a build-aware check.
func GoFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && SkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Parse reads and parses one file, naming it RELATIVE to root.
//
// The relative name is not cosmetic. Every position this gate prints comes
// from the FileSet, so naming the file by its absolute path puts the caller's
// directory into every diagnostic — and when the caller is a test fixture
// under t.TempDir(), that directory is named after the subtest. An assertion
// checking the gate said the right thing then matches the SUBTEST NAME instead
// of the diagnosis. That defect was real here and is why this takes a root.
func Parse(root, path string) (*File, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	name := Rel(root, path)
	fset := token.NewFileSet()
	syn, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return &File{Path: name, Fset: fset, Syntax: syn}, nil
}

// Rel renders path relative to root, falling back to the base name rather than
// to the absolute path: a diagnostic must never carry the caller's directory,
// and a fallback that does would reintroduce exactly the defect Parse guards.
func Rel(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return filepath.Base(path)
}

// Imports returns every import in the file with the local name it binds.
//
// An unaliased import binds the package's declared name, which is not always
// the last path element (math/rand/v2 binds "rand"). Guessing it from the path
// would be wrong for exactly the packages where being wrong is quiet, so the
// last element is used only as a fallback and the special cases that matter
// here are handled explicitly.
func (f *File) Imports() []Import {
	var out []Import
	for _, spec := range f.Syntax.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		imp := Import{Path: path, Pos: f.Fset.Position(spec.Pos())}
		switch {
		case spec.Name == nil:
			imp.Local = defaultLocal(path)
		case spec.Name.Name == ".":
			imp.Dot = true
		case spec.Name.Name == "_":
			imp.Blank = true
		default:
			imp.Local = spec.Name.Name
		}
		out = append(out, imp)
	}
	return out
}

func defaultLocal(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	// A trailing major-version element is not the package name.
	// len(path) > len(base) is not a formality: without it, the single-element
	// path "v2" slices path[:-1] and panics the gate. Found by mutating alias
	// resolution and then asking what defaultLocal does at its edges.
	if len(base) > 1 && base[0] == 'v' && len(path) > len(base) {
		if _, err := strconv.Atoi(base[1:]); err == nil {
			rest := path[:len(path)-len(base)-1]
			if i := strings.LastIndex(rest, "/"); i >= 0 {
				base = rest[i+1:]
			} else {
				base = rest
			}
		}
	}
	return base
}

// Sel is one qualified reference, e.g. the "Sleep" in time.Sleep.
type Sel struct {
	Local string
	Name  string
	Pos   token.Position
}

// Selectors returns every x.Y in the file where x is one of the given local
// names. Shadowing is not resolved: a local variable named time would make
// this report a false positive. That direction is deliberate — a gate that
// refuses something innocent is loud, and a gate that misses something is not.
func (f *File) Selectors(locals map[string]bool) []Sel {
	var out []Sel
	ast.Inspect(f.Syntax, func(n ast.Node) bool {
		se, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := se.X.(*ast.Ident)
		if !ok || !locals[id.Name] {
			return true
		}
		out = append(out, Sel{
			Local: id.Name,
			Name:  se.Sel.Name,
			Pos:   f.Fset.Position(se.Sel.Pos()),
		})
		return true
	})
	return out
}

// IsStdlib reports whether an import path names a standard-library package.
// The go tool's own rule: the first path element of a non-stdlib path contains
// a dot (a domain), and stdlib paths never do.
func IsStdlib(path string) bool {
	first := path
	if i := strings.Index(first, "/"); i >= 0 {
		first = first[:i]
	}
	return !strings.Contains(first, ".")
}

// RelErr renders err with the tree root stripped out of it.
//
// The gates report positions relative to the root so that no diagnostic
// carries the caller's directory (review round 1, finding 2). MEASURED
// 2026-08-29 by review round 2: the first fix for that dropped the underlying
// error entirely rather than relativising it, so "ring root %q is not
// readable" stopped saying whether that was a permission error or something
// else. Both properties are available at once; losing the cause was not
// required by keeping the path out.
//
// The stripping is textual because that is what an os.PathError carries — an
// absolute path inside a message, not a structured field this can reach.
func RelErr(root string, err error) string {
	if err == nil {
		return "<nil>"
	}
	msg := err.Error()
	if root == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, root+string(filepath.Separator), "")
	return strings.ReplaceAll(msg, root, ".")
}

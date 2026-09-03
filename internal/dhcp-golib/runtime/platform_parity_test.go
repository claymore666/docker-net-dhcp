package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// transportStatsFields renders the TransportStats field list of one source
// file as "name type" strings, in declaration order.
//
// It reads the file with go/parser, which does not apply build constraints, so
// one test run sees both platforms' declarations. That is the whole point: the
// fields' only readers are Linux-only test files, so a build on either
// platform cannot observe a field going missing from the other.
func transportStatsFields(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range gd.Specs {
			ts, ok := s.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "TransportStats" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("%s: TransportStats is not a struct", path)
			}
			for _, fl := range st.Fields.List {
				typ := typeText(t, fset, src, fl.Type)
				for _, n := range fl.Names {
					out = append(out, n.Name+" "+typ)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no TransportStats fields; the check would compare two empty lists", path)
	}
	return out
}

// typeText is the field's type as it is WRITTEN, taken from the source bytes.
//
// It is deliberately not a renderer over ast.Expr. A renderer has to enumerate
// the node kinds it understands, and the arm it reaches for the ones it does
// not is the arm that makes two different types compare equal. MEASURED
// 2026-08-30 on the hand-rolled version this replaces: `[4]byte` and
// `[16]byte` both rendered as `[]byte`, and `map[string]int` and
// `map[int]string` both as `*ast.MapType`, so the very substitution this test
// exists to catch survived it.
//
// A source slice has no such arm: it never inspects the node, so there is no
// kind it can fall through on.
//
// It refuses rather than returning something, in the two cases where it cannot
// do its job: offsets outside the file, and an empty slice.
//
// The comparison is therefore TEXTUAL, and that is a bound in a known
// direction. Two spellings of one type (a named alias, a comment written
// inside the type expression) compare as different and fail the test. That is
// a false positive, which is loud. What cannot happen is two different types
// comparing as the same, which is what B4 was.
func typeText(t *testing.T, fset *token.FileSet, src []byte, e ast.Expr) string {
	t.Helper()
	lo := fset.Position(e.Pos()).Offset
	hi := fset.Position(e.End()).Offset
	if lo < 0 || hi > len(src) || lo >= hi {
		t.Fatalf("type expression at offsets [%d,%d) is outside the %d-byte source; the type was not read", lo, hi, len(src))
	}
	txt := strings.Join(strings.Fields(string(src[lo:hi])), " ")
	if txt == "" {
		t.Fatalf("type expression at offsets [%d,%d) read as empty text", lo, hi)
	}
	return txt
}

// TestTypeTextDistinguishesWhatARendererCollapses drives typeText over the
// pairs that defeated the renderer it replaces, plus kinds that renderer had
// no arm for at all.
//
// It is here rather than left to the parity test because the parity test can
// only compare two files that currently agree: an extractor that returned the
// same constant for everything would pass it.
func TestTypeTextDistinguishesWhatARendererCollapses(t *testing.T) {
	const src = `package p

type s struct {
	A [4]byte
	B [16]byte
	C []byte
	D map[string]int
	E map[int]string
	F chan<- int
	G func(int) error
	H *uint64
	I uint64
	J struct{ X int }
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "s.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the fixture: %v", err)
	}
	st := f.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType)

	seen := map[string]string{}
	for _, fl := range st.Fields.List {
		got := typeText(t, fset, []byte(src), fl.Type)
		name := fl.Names[0].Name
		if prev, dup := seen[got]; dup {
			t.Errorf("fields %s and %s both read as %q; the extractor collapses distinct types, which is the defect it replaces", prev, name, got)
			continue
		}
		seen[got] = name
	}
	if want := len(st.Fields.List); len(seen) != want {
		t.Fatalf("%d distinct type texts from %d distinct field types", len(seen), want)
	}
	for _, want := range []string{"[4]byte", "[16]byte", "map[string]int", "map[int]string"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("no field read as %q; the extractor is not reporting the type as written", want)
		}
	}
}

// TestTransportStatsDeclarationsAgree is the check the non-Linux file's comment
// names.
//
// BOUND, stated rather than a completeness claim, because the sentence it
// replaces made one and the reviewer falsified it: this compares the
// TransportStats field lists of exactly the two files named below, by field
// name and by the type AS WRITTEN. It says nothing about the two
// PacketTransport method sets; it does not follow a type name to its
// definition, so two fields whose written types differ while denoting the same
// type fail it; and it cannot see a third platform file, which if one is added
// must be added here too, with nothing but this sentence saying so.
func TestTransportStatsDeclarationsAgree(t *testing.T) {
	linux := transportStatsFields(t, "transport_packet_linux.go")
	other := transportStatsFields(t, "transport_packet_other.go")
	if len(linux) != len(other) {
		t.Fatalf("field count differs: linux %d %v, other %d %v", len(linux), linux, len(other), other)
	}
	for i := range linux {
		if linux[i] != other[i] {
			t.Errorf("field %d differs: linux %q, other %q", i, linux[i], other[i])
		}
	}
}

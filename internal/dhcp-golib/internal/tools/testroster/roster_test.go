package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsTestFuncNameFollowsGosRule drives the naming rule in both directions,
// including the three shapes that made a grep the wrong instrument.
func TestIsTestFuncNameFollowsGosRule(t *testing.T) {
	for _, c := range []string{"Test", "TestFoo", "Test_lowercase", "TestX", "Benchmark", "BenchmarkFoo", "FuzzFoo", "ExampleFoo", "Test9"} {
		if !isTestFuncName(c) {
			t.Errorf("%q is a name go test would report, and was rejected", c)
		}
	}
	for _, c := range []string{"TestMain", "Testing", "TestingHelper", "Benchmarking", "Fuzzing", "Examples", "helper", "NotATest"} {
		if isTestFuncName(c) {
			t.Errorf("%q is not a name go test reports, and was accepted", c)
		}
	}
}

// TestDeclaredTestsIgnoresStringLiteralsAndMethods is the reason this walks the
// AST rather than grepping. internal/gates/t2 embeds whole test bodies inside
// raw string literals; a grep reports those as declarations that go test can
// never list, and the comparison then fails for a reason that is not true.
func TestDeclaredTestsIgnoresStringLiteralsAndMethods(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\n" +
		"import \"testing\"\n\n" +
		"const embedded = `\nfunc TestInsideAStringLiteral(t *testing.T) {}\n`\n\n" +
		"type helper struct{}\n\n" +
		"func (helper) TestAMethodIsNotATest() {}\n\n" +
		"func TestRealOne(t *testing.T) {}\n\n" +
		"func TestMain(m *testing.M) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := declaredTests(dir)
	if err != nil {
		t.Fatalf("declaredTests: %v", err)
	}
	if len(got) != 1 || got[0] != "TestRealOne" {
		t.Fatalf("got %v, want exactly [TestRealOne] — a string literal, a method or TestMain leaked into the population", got)
	}
}

// TestDeclaredTestsSeesAFileGoListCannotSee is the whole point of walking the
// filesystem: a false build constraint hides a file from the go tool, and that
// is the shape ten disabled test files took.
func TestDeclaredTestsSeesAFileGoListCannotSee(t *testing.T) {
	dir := t.TempDir()
	// The constraint is assembled rather than written, so that no literal
	// slash-slash appears anywhere in this file. That is not fussiness:
	// verify.sh's citation scan is textual and reads everything after an
	// unqualified slash-slash as a comment, so a build constraint at the head
	// of a string literal makes this fixture's test name read as a citation of
	// a test declared nowhere at column 0. Bound 7, stated in verify.sh, hit
	// here for real rather than hypothetically — and splitting the literal in
	// two does NOT avoid it, because the scan works a line at a time.
	tag := strings.Repeat("/", 2) + "go:build ignore"
	src := tag + "\n\npackage p\n\nimport \"testing\"\n\nfunc TestHiddenBehindATag(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "y_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := declaredTests(dir)
	if err != nil {
		t.Fatalf("declaredTests: %v", err)
	}
	if len(got) != 1 || got[0] != "TestHiddenBehindATag" {
		t.Fatalf("got %v, want [TestHiddenBehindATag] — a build-tagged file must still count as declared", got)
	}
}

// TestDeclaredTestsIgnoresNonTestFiles pins the other half of the domain: a
// declaration in an ordinary .go file is not a test.
func TestDeclaredTestsIgnoresNonTestFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "z.go"), []byte("package p\n\nfunc TestNotInATestFile() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := declaredTests(dir)
	if err != nil {
		t.Fatalf("declaredTests: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want none — only _test.go files hold tests", got)
	}
}

package scan

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func osWriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// TestDefaultLocal pins the identifier an unaliased import binds.
//
// It exists because a mutant that replaced defaultLocal(path) with path
// SURVIVED the gate suites: every package either gate restricts today (time,
// context, fmt) is a single-element path, for which the two are the same
// string. The mutant was a no-op on the inputs in play and a real hole for the
// next restricted package with a slash in it.
func TestDefaultLocal(t *testing.T) {
	cases := map[string]string{
		"time":         "time",
		"context":      "context",
		"net/netip":    "netip", // the case the surviving mutant broke
		"math/rand/v2": "rand",  // a version element is not the package name
		// "v2" has no package to fall back to. It sliced path[:-1] and
		// panicked the gate before the length guard in defaultLocal; a gate
		// that crashes on a malformed import is not reporting on it.
		"v2": "v2",
		"v":  "v",
		"":   "",
	}
	for path, want := range cases {
		if got := defaultLocal(path); got != want {
			t.Errorf("defaultLocal(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestIsStdlib pins the rule that separates a standard-library import from
// everything else: a dot in the first path element.
func TestIsStdlib(t *testing.T) {
	cases := map[string]bool{
		"time":                                   true,
		"net/netip":                              true,
		"encoding/binary":                        true,
		"github.com/claymore666/dhcp-golib/wire": false,
		"example.com/x":                          false,
		"gopkg.in/yaml.v3":                       false,
	}
	for path, want := range cases {
		if got := IsStdlib(path); got != want {
			t.Errorf("IsStdlib(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestImportsLocal exercises the CALL SITE, not just defaultLocal.
//
// TestDefaultLocal alone did NOT kill the mutant that replaced
// defaultLocal(path) with path at its one call site: a test on a function does
// not cover the line that calls it. Both gates read Import.Local and nothing
// else, so this is the assertion that actually protects them.
func TestImportsLocal(t *testing.T) {
	src := `package x

import (
	"time"
	"net/netip"
	clk "time"
	. "strings"
	_ "os"
)
`
	root, path := writeTemp(t, src)
	f, err := Parse(root, path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	type got struct {
		local string
		dot   bool
		blank bool
	}
	want := map[string]got{
		"time":      {local: "time"},
		"net/netip": {local: "netip"},
		"strings":   {dot: true},
		"os":        {blank: true},
	}
	seen := map[string]bool{}
	for _, imp := range f.Imports() {
		if imp.Path == "time" && imp.Local == "clk" {
			seen["time-alias"] = true
			continue
		}
		w, ok := want[imp.Path]
		if !ok {
			t.Errorf("unexpected import %q", imp.Path)
			continue
		}
		seen[imp.Path] = true
		if imp.Local != w.local || imp.Dot != w.dot || imp.Blank != w.blank {
			t.Errorf("import %q: local=%q dot=%v blank=%v, want local=%q dot=%v blank=%v",
				imp.Path, imp.Local, imp.Dot, imp.Blank, w.local, w.dot, w.blank)
		}
	}
	for _, k := range []string{"time", "net/netip", "strings", "os", "time-alias"} {
		if !seen[k] {
			t.Errorf("import %q was not reported at all", k)
		}
	}
}

func writeTemp(t *testing.T, src string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	path = root + "/x.go"
	if err := osWriteFile(path, src); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return root, path
}

// TestRel pins both directions of Rel, the function that keeps absolute paths
// out of gate diagnostics.
//
// The fallback matters as much as the happy path: it is reached when a path
// cannot be expressed relative to root, and it returns a BASENAME. A basename
// satisfies every guard that looks for the root in the output while telling
// the reader strictly less than the relative path does. TestT1DiagnosticsNameTheRing
// is the call-site half of this; a unit test on Rel alone does not cover the
// line that calls it.
func TestRel(t *testing.T) {
	cases := []struct {
		name, root, path, want string
	}{
		{"inside the root keeps the directory", "/a/b", "/a/b/proto/x.go", "proto/x.go"},
		{"the root itself", "/a/b", "/a/b/x.go", "x.go"},
		{"below is expressed with ..", "/a/b/c", "/a/b/x.go", "../x.go"},
		// filepath.Rel returns an error only when one path is absolute and the
		// other is not; that is the fallback's whole domain.
		{"unrelatable path falls back to the basename", "/a/b", "relative/x.go", "x.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Rel(c.root, c.path); got != c.want {
				t.Fatalf("Rel(%q, %q) = %q, want %q", c.root, c.path, got, c.want)
			}
		})
	}
}

// TestRelErr pins both halves of the property RelErr exists to hold at once:
// the cause survives, and the root does not.
//
// Dropping the cause was the first fix for round 1's finding 2 — a diagnostic
// carrying the caller's temp directory let an assertion match the directory
// name instead of the diagnosis. Deleting the error kept the root out and took
// the reason with it. gatetest.Run's guard covers the root half at every call
// site; nothing covered the cause half until this case.
func TestRelErr(t *testing.T) {
	root := "/tmp/fixture-Test_third_party"
	cases := []struct {
		name          string
		root          string
		err           error
		wantContains  string
		wantOmitsRoot bool
	}{
		{
			name:          "a path under the root is relativised and the cause kept",
			root:          root,
			err:           fmt.Errorf("stat %s/proto: permission denied", root),
			wantContains:  "permission denied",
			wantOmitsRoot: true,
		},
		{
			name:          "the root itself becomes a dot",
			root:          root,
			err:           fmt.Errorf("open %s: is a directory", root),
			wantContains:  "is a directory",
			wantOmitsRoot: true,
		},
		{
			name:          "an error naming no path is returned unchanged",
			root:          root,
			err:           errors.New("unexpected EOF"),
			wantContains:  "unexpected EOF",
			wantOmitsRoot: true,
		},
		{
			name:          "an empty root cannot strip anything and must not try",
			root:          "",
			err:           errors.New("no such file or directory"),
			wantContains:  "no such file or directory",
			wantOmitsRoot: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RelErr(c.root, c.err)
			if !strings.Contains(got, c.wantContains) {
				t.Errorf("RelErr dropped the cause: got %q, want it to contain %q", got, c.wantContains)
			}
			if c.wantOmitsRoot && strings.Contains(got, c.root) {
				t.Errorf("RelErr left the root in the message: %q still contains %q", got, c.root)
			}
		})
	}
	if got := RelErr("/anything", nil); got != "<nil>" {
		t.Errorf("RelErr(nil) = %q; a nil error must not render as an empty diagnosis", got)
	}
}

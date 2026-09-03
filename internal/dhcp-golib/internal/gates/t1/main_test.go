package main_test

import (
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/internal/gates/gatetest"
)

// TestT1 drives the gate by its absence. Every case names a violation the gate
// must refuse and the reason it is not obvious; the control cases name what
// must keep passing, because a guard fails in one direction and a gate that
// refuses everything is as useless as one that refuses nothing.
func TestT1(t *testing.T) {
	bin := bin(t)

	cases := []struct {
		name  string
		files map[string]string
		want  int
		// substr, when set, must appear in the gate's output. It pins WHICH
		// rule fired: a case that goes red for the wrong reason is a false
		// kill, and the table would look identical.
		substr string
	}{{
		name:   "clean tree passes",
		files:  nil,
		want:   gatetest.Pass,
		substr: "T1 PASS",
	}, {
		name: "direct time import in ring 1",
		files: map[string]string{
			"proto/impure.go": "package proto\n\nimport \"time\"\n\nvar _ = time.Now\n",
		},
		want:   gatetest.Violate,
		substr: `imports "time"`,
	}, {
		// A text search for `"time"` finds this; a text search for
		// "time.Sleep" does not. The gate resolves the alias from the file's
		// own import declarations.
		name: "aliased time import in ring 1",
		files: map[string]string{
			"proto/impure.go": "package proto\n\nimport clk \"time\"\n\nvar _ = clk.Now\n",
		},
		want:   gatetest.Violate,
		substr: `imports "time"`,
	}, {
		// The case rule B exists for. go list -deps honours build
		// constraints, so a build-aware check reports a clean closure here.
		name: "impure import behind a false build tag",
		files: map[string]string{
			"proto/hidden.go": "//go:build never_built\n\npackage proto\n\nimport \"net\"\n\nvar _ = net.Dial\n",
		},
		want:   gatetest.Violate,
		substr: "[rule B]",
	}, {
		// wire is ring 0. Ring 1 imports it, so an impure ring 0 makes ring 1
		// impure transitively, and the gate has to scan it too.
		name: "impure import in ring 0",
		files: map[string]string{
			"wire/impure.go": "package wire\n\nimport \"os\"\n\nvar _ = os.Stdout\n",
		},
		want:   gatetest.Violate,
		substr: `imports "os"`,
	}, {
		name: "ring 1 importing ring 3",
		files: map[string]string{
			"proto/impure.go": "package proto\n\nimport _ \"github.com/claymore666/dhcp-golib/runtime\"\n",
		},
		want:   gatetest.Violate,
		substr: "outside the pure rings",
	}, {
		// fmt is ON the import allowlist, so an import-set check alone passes
		// this. Rule C is what closes it.
		name: "fmt.Println in ring 1",
		files: map[string]string{
			"proto/impure.go": "package proto\n\nimport \"fmt\"\n\nfunc leak() { fmt.Println(\"x\") }\n",
		},
		want:   gatetest.Violate,
		substr: "[rule C]",
	}, {
		name: "dot import in ring 1",
		files: map[string]string{
			"proto/impure.go": "package proto\n\nimport . \"strings\"\n\nvar _ = TrimSpace\n",
		},
		want:   gatetest.Violate,
		substr: "dot import",
	}, {
		name: "third-party import in ring 1",
		files: map[string]string{
			"proto/impure.go": "package proto\n\nimport _ \"example.com/whatever\"\n",
		},
		want:   gatetest.Violate,
		substr: "third-party",
	}, {
		// A universal gate is satisfied by emptying its domain. Deleting
		// ring 1's only source file must REFUSE, not pass.
		name: "empty ring-1 domain refuses",
		files: map[string]string{
			"proto/doc.go": gatetest.Delete,
		},
		want:   gatetest.Refuse,
		substr: "domain is empty",
	}, {
		// Same defect one ring over: a gate encoding a four-ring layout that
		// can no longer find one of the rings is measuring a different tree.
		name: "missing ring-3 root refuses",
		files: map[string]string{
			"runtime/doc.go": gatetest.Delete,
		},
		want:   gatetest.Refuse,
		substr: "domain is empty",
	}, {
		// Distinct from the case above and it took a mutant to notice: there,
		// the directory is gone and the os.Stat refusal fires. Here the
		// directory EXISTS and holds only a test file, which is the branch a
		// real tree reaches when somebody deletes the implementation and
		// leaves its tests behind. Without this case that branch never ran.
		name: "ring root holding only a test file refuses",
		files: map[string]string{
			"proto/doc.go":    gatetest.Delete,
			"proto/x_test.go": "package proto\n",
		},
		want:   gatetest.Refuse,
		substr: "holds no non-test .go file",
	}, {
		name: "ring root holding no .go file at all refuses",
		files: map[string]string{
			"proto/doc.go":   gatetest.Delete,
			"proto/NOTES.md": "not go source\n",
		},
		want:   gatetest.Refuse,
		substr: "holds no non-test .go file",
	}, {
		name: "renamed module refuses",
		files: map[string]string{
			"go.mod": "module github.com/claymore666/renamed\n\ngo 1.25\n",
		},
		want:   gatetest.Refuse,
		substr: "update internal/gates/rings.Module",
	}, {
		name: "missing go.mod refuses",
		files: map[string]string{
			"go.mod": gatetest.Delete,
		},
		want:   gatetest.Refuse,
		substr: "cannot read go.mod",
	}, {
		// Preservation controls. Ring 1 has real work to do and the gate must
		// not stand in its way, or it gets weakened the first time it does.
		name: "control: fmt.Errorf and net/netip pass",
		files: map[string]string{
			"proto/ok.go": "package proto\n\nimport (\n\t\"fmt\"\n\t\"net/netip\"\n)\n\nfunc d(a netip.Addr) error { return fmt.Errorf(\"%s\", a) }\n",
		},
		want:   gatetest.Pass,
		substr: "T1 PASS",
	}, {
		name: "control: ring 1 importing ring 0 passes",
		files: map[string]string{
			"proto/ok.go": "package proto\n\nimport _ \"github.com/claymore666/dhcp-golib/wire\"\n",
		},
		want:   gatetest.Pass,
		substr: "T1 PASS",
	}, {
		// T1 is a claim about the package, not about its tests. A ring-1 test
		// reading a golden file does not make the state machine impure, and
		// T2 is what governs test files.
		name: "control: a ring-1 test importing os passes T1",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport _ \"os\"\n",
		},
		want:   gatetest.Pass,
		substr: "T1 PASS",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := gatetest.Fixture(t, tc.files)
			code, out := gatetest.Run(t, bin, root)
			if code != tc.want {
				t.Errorf("exit code = %d, want %d\noutput:\n%s", code, tc.want, out)
			}
			if tc.substr != "" && !strings.Contains(out, tc.substr) {
				t.Errorf("output does not contain %q; the case may be red for the wrong reason\noutput:\n%s", tc.substr, out)
			}
		})
	}
}

// TestT1DiagnosticsNameTheRing pins the ring-qualified path in the diagnosis.
//
// The gates report positions relative to the tree root (scan.Rel) so that a
// diagnostic never carries the caller's temp directory — see review finding 2
// and gatetest.Run's guard. Rel has a fallback for a path it cannot relativise
// that returns the BASENAME, and a basename passes that guard perfectly well:
// it does not contain the root either.
//
// MEASURED 2026-08-29: mutating Rel to take the fallback for every path
// survived the whole suite. Nothing asserted that a diagnostic distinguishes
// proto/doc.go from wire/doc.go, and the fix for one finding had quietly
// created the conditions for an ambiguous one.
//
// So this plants the SAME basename in two rings and requires the gate to tell
// them apart. A basename-only diagnosis cannot: it would name "impure.go"
// twice and leave the reader to guess which ring is impure.
//
// What it CANNOT see: it fixes the shape of the position, not its accuracy —
// a gate emitting a correct-looking but wrong relative path would pass this.
func TestT1DiagnosticsNameTheRing(t *testing.T) {
	root := gatetest.Fixture(t, map[string]string{
		"proto/impure.go": "package proto\n\nimport \"os\"\n\nvar _ = os.Stdout\n",
		"wire/impure.go":  "package wire\n\nimport \"os\"\n\nvar _ = os.Stderr\n",
	})
	code, out := gatetest.Run(t, bin(t), root)
	if code != gatetest.Violate {
		t.Fatalf("exit %d, want %d\noutput:\n%s", code, gatetest.Violate, out)
	}
	for _, want := range []string{"proto/impure.go", "wire/impure.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnosis does not name %s. Two rings hold a file of that "+
				"basename; a diagnosis that names only the basename cannot say which "+
				"ring is impure.\noutput:\n%s", want, out)
		}
	}
}

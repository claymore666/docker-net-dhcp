package main_test

import (
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/internal/gates/gatetest"
)

// TestT2 drives the gate by its absence: every violating case is a way a test
// could come to wait on the clock, and every control is something a legitimate
// test must still be allowed to do.
func TestT2(t *testing.T) {
	bin := bin(t)

	// Fixture always writes the four ring packages; T2 needs at least one
	// _test.go somewhere or it refuses, so the clean baseline supplies one.
	clean := "package proto\n\nimport \"testing\"\n\nfunc TestNothing(t *testing.T) {}\n"

	cases := []struct {
		name   string
		files  map[string]string
		want   int
		substr string
	}{{
		name:   "clean tree with a test passes",
		files:  map[string]string{"proto/x_test.go": clean},
		want:   gatetest.Pass,
		substr: "T2 PASS",
	}, {
		name: "time.Sleep in a test",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestS(t *testing.T) { time.Sleep(time.Second) }\n",
		},
		want:   gatetest.Violate,
		substr: "time.Sleep",
	}, {
		// The case a grep for "time.Sleep" misses entirely. The gate resolves
		// the local name from the file's own import declaration.
		name: "aliased time.Sleep in a test",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"testing\"\n\tclk \"time\"\n)\n\nfunc TestS(t *testing.T) { clk.Sleep(clk.Second) }\n",
		},
		want:   gatetest.Violate,
		substr: "clk.Sleep",
	}, {
		name: "time.After in a select",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestA(t *testing.T) {\n\tch := make(chan int)\n\tselect {\n\tcase <-ch:\n\tcase <-time.After(time.Second):\n\t}\n}\n",
		},
		want:   gatetest.Violate,
		substr: "time.After",
	}, {
		name: "time.NewTicker in a test",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestT(t *testing.T) { _ = time.NewTicker(time.Second) }\n",
		},
		want:   gatetest.Violate,
		substr: "time.NewTicker",
	}, {
		name: "time.AfterFunc in a test",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestF(t *testing.T) { _ = time.AfterFunc(time.Second, func() {}) }\n",
		},
		want:   gatetest.Violate,
		substr: "time.AfterFunc",
	}, {
		// A deadline on a context is a wall-clock wait under another name:
		// whatever blocks on ctx.Done() is blocking until a timer fires.
		name: "context.WithTimeout in a test",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"context\"\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestC(t *testing.T) {\n\tctx, cancel := context.WithTimeout(context.Background(), time.Second)\n\tdefer cancel()\n\t_ = ctx\n}\n",
		},
		want:   gatetest.Violate,
		substr: "context.WithTimeout",
	}, {
		name: "dot import of time in a test",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"testing\"\n\t. \"time\"\n)\n\nfunc TestD(t *testing.T) { Sleep(Second) }\n",
		},
		want:   gatetest.Violate,
		substr: "dot import",
	}, {
		// The violation lives in an external test package, in a different
		// ring, under a name the gate was not told about. It is still a test
		// file and still waits.
		name: "sleep in a ring-3 external test package",
		files: map[string]string{
			"runtime/y_test.go": "package runtime_test\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestS(t *testing.T) { time.Sleep(time.Millisecond) }\n",
		},
		want:   gatetest.Violate,
		substr: "time.Sleep",
	}, {
		// A universal gate is satisfied by emptying its domain. No test files
		// at all must REFUSE, not pass.
		name:   "no test files refuses",
		files:  nil,
		want:   gatetest.Refuse,
		substr: "domain is empty",
	}, {
		// Controls. Durations and instants are how a test drives a fake
		// clock; forbidding them would make the gate unusable and it would be
		// weakened at the first inconvenience.
		name: "control: time.Duration and constants pass",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestD(t *testing.T) {\n\tvar d time.Duration = 30 * time.Second\n\t_ = time.Unix(0, 0).Add(d)\n}\n",
		},
		want:   gatetest.Pass,
		substr: "T2 PASS",
	}, {
		name: "control: context without a deadline passes",
		files: map[string]string{
			"proto/x_test.go": "package proto\n\nimport (\n\t\"context\"\n\t\"testing\"\n)\n\nfunc TestC(t *testing.T) {\n\tctx, cancel := context.WithCancel(context.Background())\n\tdefer cancel()\n\t_ = ctx\n}\n",
		},
		want:   gatetest.Pass,
		substr: "T2 PASS",
	}, {
		// T2's domain is test files. Non-test code legitimately sleeps —
		// ring 3 has a real clock in it — and T1 is what keeps that out of
		// ring 1.
		name: "control: time.Sleep in ring-3 NON-test code passes T2",
		files: map[string]string{
			"proto/x_test.go": clean,
			"runtime/impl.go": "package runtime\n\nimport \"time\"\n\nfunc wait() { time.Sleep(time.Second) }\n",
		},
		want:   gatetest.Pass,
		substr: "T2 PASS",
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

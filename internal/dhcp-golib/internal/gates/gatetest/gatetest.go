// Package gatetest builds throwaway module fixtures and runs a gate binary
// against them, so each gate's red-then-green behaviour is a test that reruns
// rather than a transcript somebody once pasted into a report.
//
// The gate is exercised as a BUILT BINARY, not by calling its run function.
// That is deliberate: the gates report three outcomes through three exit codes
// (0 pass, 1 violation, 2 refused), verify.sh distinguishes them, and the
// mapping from an internal result to an exit code is part of what has to work.
// Calling run() in-process would test everything except that.
package gatetest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exit codes the gates share.
const (
	Pass    = 0
	Violate = 1
	Refuse  = 2
)

// Build compiles the gate in the current package directory and returns the
// path to the binary.
func Build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gate")
	if err := build(bin); err != nil {
		t.Fatalf("%v", err)
	}
	return bin
}

// BuildForMain is Build for use from TestMain, where there is no *testing.T.
// It returns the binary path and a cleanup function.
//
// It exists because the binary must outlive any single test: caching one in a
// t.TempDir() gives every later test a path whose directory has already been
// removed, which surfaces as "fork/exec ...: no such file or directory" rather
// than as anything resembling the actual mistake.
func BuildForMain() (bin string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "gatebin")
	if err != nil {
		return "", func() {}, err
	}
	bin = filepath.Join(dir, "gate")
	if err := build(bin); err != nil {
		os.RemoveAll(dir)
		return "", func() {}, err
	}
	return bin, func() { os.RemoveAll(dir) }, nil
}

func build(bin string) error {
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		return fmt.Errorf("building the gate failed: %v\n%s", err, out)
	}
	return nil
}

// Fixture writes a throwaway module rooted at a fresh temp dir. files maps a
// path relative to that root to its contents; the go.mod and the four ring
// packages are written first and any entry in files overwrites them.
//
// The module path matches the real one on purpose: the gates classify an
// import as internal by that prefix, and a fixture declaring a different path
// would exercise a different code path from the one that runs in anger.
func Fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	base := map[string]string{
		"go.mod":         "module github.com/claymore666/dhcp-golib\n\ngo 1.25\n",
		"wire/doc.go":    "package wire\n",
		"proto/doc.go":   "package proto\n",
		"lease/doc.go":   "package lease\n",
		"runtime/doc.go": "package runtime\n",
	}
	for path, content := range files {
		base[path] = content
	}
	for path, content := range base {
		if content == Delete {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("fixture mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("fixture write %s: %v", path, err)
		}
	}
	return root
}

// Delete, used as a file's content in a Fixture map, omits that file. It is
// how a test empties a gate's domain — the case a universal gate passes
// vacuously if nobody checks it.
const Delete = "\x00delete\x00"

// Run executes the gate against root and returns its exit code and combined
// output.
//
// It also FAILS the test if the output contains the fixture root anywhere.
// That check is here, in the one function every case goes through, rather than
// beside each assertion: a guard that sits next to what it protects shares that
// thing's fate under every edit that moves either one, and a case added later
// would simply not have it.
//
// Why it matters is a defect this suite actually had. t.TempDir() names its
// directory after the SUBTEST, so if a gate prints an absolute fixture path,
// every case whose expected substring appears in its own name matches the PATH
// instead of the diagnosis. Six cases were in that state, and it cost a real
// surviving mutant: "third-party check bypassed" lived because the temp path
// contained "third-party" while the gate's message never did. The gates were
// changed to print root-relative paths; this assertion is what keeps them that
// way, and it goes red rather than silently passing if one regresses.
func Run(t *testing.T, bin, root string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"-root", root}, args...)...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if e, ok := err.(*exec.ExitError); ok {
			ee = e
			code = ee.ExitCode()
		} else {
			t.Fatalf("running the gate failed: %v\n%s", err, out)
		}
	}
	if strings.Contains(string(out), root) {
		t.Fatalf("gate output contains the fixture root %q. A diagnostic carrying the "+
			"caller's directory lets an assertion match the temp path — which is named "+
			"after this subtest — instead of the diagnosis.\noutput:\n%s", root, out)
	}
	return code, string(out)
}

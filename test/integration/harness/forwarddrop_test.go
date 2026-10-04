// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeIptables puts an iptables on PATH that keeps the FORWARD policy in a file, so WithForwardDrop runs without root.
func fakeIptables(t *testing.T, policy, nf string) (policyFile, nfFile string) {
	t.Helper()
	dir := t.TempDir()
	policyFile = filepath.Join(dir, "policy")
	nfFile = filepath.Join(dir, "nf")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"--version) echo 'iptables v1.8.10 (nf_tables)';;\n" +
		"-S) echo \"-P FORWARD $(cat " + policyFile + ")\"; echo '-A FORWARD -j DOCKER-USER';;\n" +
		"-P) [ -e " + policyFile + ".frozen ] || echo \"$3\" > " + policyFile + ";;\n" +
		"*) exit 2;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "iptables"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{policyFile: policy, nfFile: nf} {
		if err := os.WriteFile(f, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := nfCallSysctl
	nfCallSysctl = nfFile
	t.Cleanup(func() { nfCallSysctl = old })
	return policyFile, nfFile
}

func readTrim(t *testing.T, f string) string {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// #1116: the case sets the drop state itself and leaves the host as it found it.
func TestWithForwardDrop_SetsAndRestores(t *testing.T) {
	for _, tc := range []struct{ name, policy, nf string }{
		{"ACCEPT and sysctl 1, the lane engine", "ACCEPT", "1"},
		{"ACCEPT and sysctl 0", "ACCEPT", "0"},
		{"already DROP and sysctl 1", "DROP", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policyFile, nfFile := fakeIptables(t, tc.policy, tc.nf)
			t.Run("inside", func(t *testing.T) {
				WithForwardDrop(t)
				if drops, detail := ForwardDropState(); !drops {
					t.Errorf("ForwardDropState = false (%s) inside the case", detail)
				}
				if got, nf := readTrim(t, policyFile), readTrim(t, nfFile); got != "DROP" || nf != "1" {
					t.Errorf("policy %q, sysctl %q inside the case; want DROP and 1", got, nf)
				}
			})
			if got, nf := readTrim(t, policyFile), readTrim(t, nfFile); got != tc.policy || nf != tc.nf {
				t.Errorf("after the case policy %q, sysctl %q; want %q and %q back", got, nf, tc.policy, tc.nf)
			}
		})
	}
}

func TestForwardDropState_NamesTheBackend(t *testing.T) {
	fakeIptables(t, "ACCEPT", "1")
	if drops, detail := ForwardDropState(); drops || !strings.Contains(detail, "nf_tables") || !strings.Contains(detail, "-P FORWARD ACCEPT") {
		t.Errorf("drops %v, detail %q; want false, the policy and the backend", drops, detail)
	}
}

// recordingT stands in for a testing.T whose Fatalf must stop the helper, so a refusal can be observed.
type recordingT struct {
	testing.TB
	fatal    string
	errs     []string
	cleanups []func()
}

func (r *recordingT) Helper()                   {}
func (r *recordingT) Cleanup(f func())          { r.cleanups = append(r.cleanups, f) }
func (r *recordingT) Errorf(f string, a ...any) { r.errs = append(r.errs, fmt.Sprintf(f, a...)) }
func (r *recordingT) Fatalf(f string, a ...any) { r.fatal = fmt.Sprintf(f, a...); runtime.Goexit() }

// A host whose policy stays ACCEPT after the write must fail the case, not let it pass on the wrong premise.
func TestWithForwardDrop_FailsWhenThePolicyDoesNotTake(t *testing.T) {
	policyFile, _ := fakeIptables(t, "ACCEPT", "1")
	dir := filepath.Dir(policyFile)
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"--version) echo 'iptables v1.8.10 (legacy)';;\n" +
		"-S) echo \"-P FORWARD $(cat " + policyFile + ")\";;\n" +
		"-P) ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "iptables"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &recordingT{TB: t}
	done := make(chan struct{})
	go func() { defer close(done); WithForwardDrop(r) }()
	<-done
	if !strings.Contains(r.fatal, "still does not drop") || !strings.Contains(r.fatal, "legacy") {
		t.Errorf("fatal %q; want the refusal naming the backend", r.fatal)
	}
}

// A restore that does not take must be reported, since every later case would run on the wrong host.
func TestWithForwardDrop_ReportsARestoreThatDoesNotTake(t *testing.T) {
	policyFile, _ := fakeIptables(t, "ACCEPT", "1")
	r := &recordingT{TB: t}
	WithForwardDrop(r)
	if err := os.WriteFile(policyFile+".frozen", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	if len(r.errs) != 1 || !strings.Contains(r.errs[0], "not back to policy ACCEPT") {
		t.Errorf("errors %q; want one saying the host is not back to policy ACCEPT", r.errs)
	}
}

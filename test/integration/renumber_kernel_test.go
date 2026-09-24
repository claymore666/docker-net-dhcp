// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The unit job cannot create a network namespace (the hosted image refuses netlink inside an unprivileged user
// namespace), so these pkg/plugin tests skip there; this suite runs as root, where they must pass, not skip (#1081).
var renumberKernelTests = []string{
	"TestRenew_ARenumberLeavesExactlyTheNewAddressAndRoutes",
	"TestRenew_ARenumberWithTheOldAddressAlreadyGoneStillBinds",
	"TestApplyAddressChange_ARefusedNewAddressKeepsTheOldOne",
	"TestApplyAddressChange_AV6RenumberLeavesOnlyTheNewAddress",
}

// runRenumberKernelTests runs the kernel tests with env added and lists an error exit, a missing PASS or any SKIP.
func runRenumberKernelTests(t *testing.T, env ...string) (problems []string, output string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	run := "^(" + strings.Join(renumberKernelTests, "|") + ")$"
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-v", "-run", run,
		"github.com/claymore666/docker-net-dhcp/v2/pkg/plugin")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		problems = append(problems, "go test exited: "+err.Error())
	}
	for _, name := range renumberKernelTests {
		if !strings.Contains(text, "\n--- PASS: "+name+" (") {
			problems = append(problems, name+" did not report PASS at top level")
		}
	}
	if strings.Contains(text, "--- SKIP") {
		problems = append(problems, "a kernel test or one of its cases skipped")
	}
	return problems, text
}

func TestRenumber_ThePluginKernelTestsPassInTheirOwnNamespace(t *testing.T) {
	problems, text := runRenumberKernelTests(t)
	for _, p := range problems {
		t.Error(p)
	}
	if t.Failed() {
		t.Logf("output:\n%s", text)
	}
	// With the link refused the same judge must find the run wrong, so a run that tests nothing cannot read as PASS.
	refused, refusedText := runRenumberKernelTests(t, "DND_RENUMBER_REFUSE_LINK=1")
	if len(refused) == 0 {
		t.Errorf("with the link refused the run was judged clean:\n%s", refusedText)
	}
	for _, name := range renumberKernelTests {
		if strings.Contains(refusedText, "\n--- PASS: "+name+" (") {
			t.Errorf("%s reported PASS with the link refused", name)
		}
	}
}

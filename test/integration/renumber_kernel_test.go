// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
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

func TestRenumber_ThePluginKernelTestsPassInTheirOwnNamespace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	run := "^(" + strings.Join(renumberKernelTests, "|") + ")$"
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-v", "-run", run,
		"github.com/claymore666/docker-net-dhcp/v2/pkg/plugin")
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		t.Fatalf("go test %s: %v\n%s", run, err, text)
	}
	for _, name := range renumberKernelTests {
		if !strings.Contains(text, "\n--- PASS: "+name+" (") {
			t.Errorf("%s did not report PASS at top level", name)
		}
	}
	if strings.Contains(text, "--- SKIP") {
		t.Errorf("a kernel test skipped where it must run")
	}
	if t.Failed() {
		t.Logf("output:\n%s", text)
	}
}

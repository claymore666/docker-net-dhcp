// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"os/exec"
	"strings"
	"testing"
)

// Alpine's busybox provides udhcpc and udhcpc6 in every image, so "no second DHCP client" means nothing linked into the
// binary starts a child process; the population is `go list -deps ./cmd/net-dhcp`, and os/exec is judged, not a list of
// client names (#899).

func TestNothingLinkedIntoThePluginExecsAnything(t *testing.T) {
	// The pinned library is linked into the binary, so it is judged here too (#979).
	ours := []string{
		"github.com/claymore666/docker-net-dhcp/v2/",
		"github.com/claymore666/dhcp-golib/",
	}

	// A test's working directory is its package, so a relative pattern resolves under pkg/dhcp (#979).
	const mainPkg = "github.com/claymore666/docker-net-dhcp/v2/cmd/net-dhcp"
	cmd := exec.Command("go", "list", "-deps",
		"-f", "{{.ImportPath}} {{join .Imports \" \"}}", mainPkg)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", mainPkg, err, stderr.String())
	}

	var total, judged int
	var offenders []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		total++
		pkg := fields[0]
		mine := false
		for _, prefix := range ours {
			if strings.HasPrefix(pkg+"/", prefix) {
				mine = true
			}
		}
		if !mine {
			continue
		}
		judged++
		for _, imp := range fields[1:] {
			if imp == "os/exec" {
				offenders = append(offenders, pkg)
			}
		}
	}

	if total < 50 {
		t.Fatalf("go list reported only %d package(s) in the plugin's dependency closure; "+
			"the build is not being read and a pass here would mean nothing", total)
	}
	if judged < 5 {
		t.Fatalf("only %d package(s) of %d matched %v, so almost nothing was judged. Either the "+
			"module path changed or the filter stopped matching; a pass would be over an empty "+
			"population.", judged, total, ours)
	}
	t.Logf("%d package(s) ship in the plugin binary; %d of them are ours and were judged", total, judged)

	if len(offenders) != 0 {
		t.Errorf("these packages are linked into the plugin and import os/exec: %v.\n"+
			"2.0 leases in-process. The image cannot help here: busybox's udhcpc applet is in "+
			"every Alpine image and cannot be removed, so the guarantee that there is no "+
			"second, unmanaged DHCP client on the link rests entirely on this binary starting "+
			"no child process. If one is genuinely needed, change this test deliberately and "+
			"say why — do not add an exception for a single caller.", offenders)
	}
}

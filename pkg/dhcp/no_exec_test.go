// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNothingLinkedIntoThePluginExecsAnything is the source half of the
// "no second DHCP client" guarantee, and the half the image cannot
// make.
//
// WHY IT EXISTS AT ALL. 1.x drove dhcpcd as a child process. 2.0 leases
// in-process through the library, and the Dockerfile refuses a DHCP
// client that is INSTALLED in the image — but it cannot refuse the one
// already there: MEASURED against the pinned base digest, `udhcpc` and
// `udhcpc6` are symlinks to /bin/busybox, present in every Alpine
// image, removable only by removing the shell this check runs in. So
// "no second client on the link" cannot mean "no client binary exists".
// It means NOTHING RUNS ONE, and that is a property of the source.
//
// THE POPULATION IS DERIVED FROM THE BUILD, not from a path
// convention. `go list -deps ./cmd/net-dhcp` is exactly what gets
// linked into the binary the Dockerfile copies in — so a package that
// starts shipping is judged the day it does, and one that never ships
// is not. That distinction is load-bearing rather than tidy: the
// integration harness legitimately execs docker, ip and dnsmasq, and a
// filter written as "pkg/ and cmd/" would have had to name it as an
// exception. It is not an exception; it is not in the binary.
//
// WHY os/exec AND NOT A LIST OF CLIENT NAMES. A gate keyed on
// `dhcpcd`, `udhcpc`, `dhclient` reproduces its own blind spot: it is
// satisfied by exec.Command(cfg.Helper), by a name assembled from
// pieces, and by every client nobody thought of. The property that
// actually holds is stronger and simpler — the plugin starts no child
// process at all — so that is what is asserted.
//
// THIS REPLACES pkg/dhcp/dockerfile_parity_test.go, which compared the
// absolute binaries pkg/dhcp exec'd against the Dockerfile's `test -x`
// operands. With every exec site deleted its derived set is empty and
// it passed having compared nothing. It was deleted rather than left
// green; this is a POSITIVE statement in its place, and both its
// populations are asserted below so it cannot be retired the same way.
func TestNothingLinkedIntoThePluginExecsAnything(t *testing.T) {
	// The two module paths whose source this project is answerable
	// for. The pinned library copy is in the binary, so it is judged
	// here — unlike the repo-wide gates, which cannot act on a finding
	// in it, a client started from inside it would be a client on this
	// link.
	ours := []string{
		"github.com/claymore666/docker-net-dhcp/",
		"github.com/claymore666/dhcp-golib/",
	}

	// The FULL import path, not "./cmd/net-dhcp": a test runs with its
	// own package directory as the working directory, so the relative
	// form resolves to pkg/dhcp/cmd/net-dhcp and go list exits 1.
	const mainPkg = "github.com/claymore666/docker-net-dhcp/cmd/net-dhcp"
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

	// BOTH POPULATIONS, ASSERTED, and the second is the one that
	// matters. A `go list` that silently returned nothing, a renamed
	// module path, a cmd/ that stopped building — each leaves the loop
	// above with nothing to judge and the check below trivially
	// satisfied. That is precisely how this test's predecessor stopped
	// meaning anything, and it must not be how this one does.
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

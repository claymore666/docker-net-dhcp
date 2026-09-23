// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: locale_test.go's untagged scan requires every subprocess, including keaconfine.go's dmesg, to call this (#869).

package harness

import (
	"os"
	"os/exec"
)

// withCLocale sets LC_ALL=C, which beats LANG and LC_MESSAGES, on a fixture subprocess. dnsmasq is translated: a
// German locale broke waitChallengerReady's English match, and a translated protocol token would make the #800
// zero-DHCPRELEASE assertions pass vacuously (#869).
func withCLocale(cmd *exec.Cmd) *exec.Cmd {
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd
}

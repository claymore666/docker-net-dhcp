// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag, deliberately, and it is the reason
// this helper does not live in fixture.go any more.
//
// The rule withCLocale carries -- every subprocess this harness starts
// speaks English -- is enforced by a source scan in locale_test.go, and
// that scan is untagged so it runs in the ordinary `go test ./...` job.
// A tagged helper cannot be called from an untagged file, so keeping it
// in fixture.go meant the one untagged file that starts a subprocess
// (keaconfine.go, which shells out to dmesg for AppArmor denial
// records) could not comply with the rule at all. It shipped unpinned,
// and the guard caught it seven minutes into the integration suite
// rather than in the seconds an untagged check would have taken (#869).

package harness

import (
	"os"
	"os/exec"
)

// withCLocale pins a fixture server's messages to English before it is
// started, and every DHCP server this harness launches goes through it.
//
// WHY. dnsmasq is translated. On a host with a German locale it writes
// "DHCP, Sockets exklusiv an die Schnittstelle … gebunden" where an
// English one writes "sockets bound exclusively to interface …", and
// waitChallengerReady — which has no port to poll, because the socket is
// in another namespace — matches on the English text. Five server-policy
// tests failed on exactly that, against a server that had started
// correctly and said so in the operator's language.
//
// The sharper reason is the one that does not announce itself. The
// #800 assertions read these same logs to prove an ABSENCE: zero
// DHCPRELEASE lines for an address. Protocol tokens happen not to be
// translated today, but nothing guarantees that, and a token that got
// translated would make every one of those assertions pass VACUOUSLY —
// the matcher would find nothing because it no longer recognised
// anything, and report the clean result the test is looking for. The
// canned log in releasematcher_test.go is English, so the control and
// its subject would have drifted apart by locale alone.
//
// LC_ALL beats LANG and LC_MESSAGES, so one variable settles it. The
// rest of the environment is inherited: os.Environ() first, override
// after.
func withCLocale(cmd *exec.Cmd) *exec.Cmd {
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd
}

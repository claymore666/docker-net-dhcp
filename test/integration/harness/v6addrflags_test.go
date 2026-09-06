// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import "testing"

// Every string below is VERBATIM `ip -6 -o addr show` output, MEASURED
// 2026-09-06 in a user+network namespace on the session box: the same
// addresses, on the same links, read once by the host's iproute2 and
// once by alpine:3.20's busybox 1.36.1 (the image the suite runs
// containers in) chrooted into that namespace. The double spaces and
// the trailing backslash are theirs.
//
// The pair is the whole point of the file. The busybox rendering of
// IFA_F_NODAD is `flags 02` -- the word "nodad" appears nowhere in it
// -- so an observer validated only against the host's tool passes here
// and can never pass where it runs.

// A /128 installed the way the chassis installs a DHCPv6 lease:
// IFA_F_NODAD plus both lifetimes.
const (
	nodadLeasedIproute2 = `2: dummy0    inet6 fd00:dead::1/128 scope global nodad dynamic \       valid_lft 300sec preferred_lft 200sec`
	nodadLeasedBusybox  = `2: dummy0    inet6 fd00:dead::1/128 scope global dynamic flags 02 \       valid_lft 300sec preferred_lft 200sec`
)

// The same link a moment after two addresses were added, one with
// IFA_F_NODAD and one without: the second is still running the kernel's
// duplicate-address detection. This is the shape the chassis's re-apply
// exists to prevent.
const (
	tentativeIproute2 = `3: v0    inet6 fd00:beef::2/64 scope global nodad \       valid_lft forever preferred_lft forever
3: v0    inet6 fd00:beef::1/64 scope global tentative \       valid_lft forever preferred_lft forever
3: v0    inet6 fe80::c0c8:66ff:fe9e:97a6/64 scope link tentative proto kernel_ll \       valid_lft forever preferred_lft forever`
	tentativeBusybox = `3: v0    inet6 fd00:beef::2/64 scope global flags 02 \       valid_lft forever preferred_lft forever
3: v0    inet6 fd00:beef::1/64 scope global tentative \       valid_lft forever preferred_lft forever
3: v0    inet6 fe80::c0c8:66ff:fe9e:97a6/64 scope link tentative \       valid_lft forever preferred_lft forever`
)

// The RFC 7527 outcome the NODAD flag exists to make impossible: the
// same address claimed twice on one segment, and the kernel took the
// loser out of service. Both tools name this one.
const (
	dadfailedIproute2 = `3: v0    inet6 fd00:beef::5/64 scope global dadfailed tentative \       valid_lft forever preferred_lft forever`
	dadfailedBusybox  = `3: v0    inet6 fd00:beef::5/64 scope global tentative dadfailed \       valid_lft forever preferred_lft forever`
)

func TestV6AddrFlagsFromAddrShow_ReadsNODADUnderBothRenderings(t *testing.T) {
	for name, out := range map[string]string{
		"iproute2": nodadLeasedIproute2,
		"busybox":  nodadLeasedBusybox,
	} {
		t.Run(name, func(t *testing.T) {
			f := V6AddrFlagsFromAddrShow(out, "fd00:dead::1")
			if !f.Found {
				t.Fatalf("address not found in:\n%s", out)
			}
			if !f.NoDAD {
				t.Errorf("NoDAD false for an address installed with IFA_F_NODAD. "+
					"An observer that cannot see the flag in this rendering reports "+
					"a correctly installed lease as a defect, and a defect as a "+
					"defect, so it has one verdict. Line: %q", f.Line)
			}
			if f.Tentative || f.DADFailed {
				t.Errorf("tentative=%v dadfailed=%v on a settled nodad address: %q",
					f.Tentative, f.DADFailed, f.Line)
			}
		})
	}
}

// The other direction, and the one that decides whether this observer
// can fail at all: the address libnetwork installed and the chassis did
// NOT re-apply. Same line, same tool, no NODAD.
func TestV6AddrFlagsFromAddrShow_SeesTheAddressWithoutNODAD(t *testing.T) {
	for name, out := range map[string]string{
		"iproute2": tentativeIproute2,
		"busybox":  tentativeBusybox,
	} {
		t.Run(name, func(t *testing.T) {
			f := V6AddrFlagsFromAddrShow(out, "fd00:beef::1")
			if !f.Found {
				t.Fatalf("address not found in:\n%s", out)
			}
			if f.NoDAD {
				t.Errorf("NoDAD true for an address added without the flag: %q", f.Line)
			}
			if !f.Tentative {
				t.Errorf("Tentative false while the kernel is still probing: %q", f.Line)
			}

			// ...and the neighbouring address on the SAME output, which
			// does carry it, so a parser that returns the first line's
			// flags for every address fails here.
			g := V6AddrFlagsFromAddrShow(out, "fd00:beef::2")
			if !g.Found || !g.NoDAD || g.Tentative {
				t.Errorf("the nodad address on the same link read as found=%v nodad=%v tentative=%v: %q",
					g.Found, g.NoDAD, g.Tentative, g.Line)
			}
		})
	}
}

func TestV6AddrFlagsFromAddrShow_ReadsDADFailed(t *testing.T) {
	for name, out := range map[string]string{
		"iproute2": dadfailedIproute2,
		"busybox":  dadfailedBusybox,
	} {
		t.Run(name, func(t *testing.T) {
			f := V6AddrFlagsFromAddrShow(out, "fd00:beef::5")
			if !f.Found {
				t.Fatalf("address not found in:\n%s", out)
			}
			if !f.DADFailed {
				t.Errorf("DADFailed false on the kernel's own dadfailed line: %q", f.Line)
			}
			if f.NoDAD {
				t.Errorf("NoDAD true on an address the kernel ran DAD on: %q", f.Line)
			}
		})
	}
}

// An address that is not there must not read as a healthy one. This is
// the shape a mis-derived interface or a typo'd address produces, and
// without Found it is byte-identical to "installed, no flags set".
func TestV6AddrFlagsFromAddrShow_AbsentIsNotHealthy(t *testing.T) {
	f := V6AddrFlagsFromAddrShow(nodadLeasedBusybox, "fd00:dead::9")
	if f.Found {
		t.Fatalf("found an address that is not in the output: %q", f.Line)
	}
	if f.Line != "" {
		t.Errorf("a not-found result must carry no line, got %q", f.Line)
	}
}

// The #875 rule, inherited from V6IfaceFromAddrShow: the address is a
// whole field split at its prefix length, never a substring. Without it
// `fd00:dead::1` reads the flags of `fd00:dead::12`.
func TestV6AddrFlagsFromAddrShow_MatchesTheWholeAddressField(t *testing.T) {
	out := `2: dummy0    inet6 fd00:dead::12/128 scope global tentative \       valid_lft forever preferred_lft forever`
	if f := V6AddrFlagsFromAddrShow(out, "fd00:dead::1"); f.Found {
		t.Errorf("fd00:dead::1 matched the line for fd00:dead::12: %q", f.Line)
	}
	if f := V6AddrFlagsFromAddrShow(out, "fd00:dead::12"); !f.Found || !f.Tentative {
		t.Errorf("the whole-field address did not match: found=%v tentative=%v", f.Found, f.Tentative)
	}
}

// Nothing after the backslash is a flag, and a lifetime keyword must
// not become one.
func TestV6AddrFlagsFromAddrShow_StopsAtTheBackslash(t *testing.T) {
	out := `2: dummy0    inet6 fd00:dead::1/128 scope global \       valid_lft forever preferred_lft forever flags 02 nodad`
	f := V6AddrFlagsFromAddrShow(out, "fd00:dead::1")
	if !f.Found {
		t.Fatal("address not found")
	}
	if f.NoDAD {
		t.Errorf("read a flag from behind the backslash: %q", f.Line)
	}
}

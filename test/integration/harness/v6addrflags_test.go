// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"testing"

	"golang.org/x/sys/unix"
)

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

// The three lifetime shapes an address on a container link can be in,
// MEASURED 2026-09-16 on the session box under `unshare -Urn`, one
// dummy link, iproute2-6.15.0, LC_ALL=C: an address deprecated by
// `preferred_lft 0`, an ordinary leased one, and one with no lifetimes
// at all. The kernel's own renderings, in one capture, in this order.
//
// THE DEPRECATED ROW IS #819's ORACLE. The plugin's own preferred
// number says what it meant to install; this line says what the kernel
// holds. A change that stopped passing the preferred lifetime to
// netlink leaves the first right and the second clear.
const lifetimesIproute2 = `2: v0    inet6 fd00:beef::9/64 scope global nodad \       valid_lft forever preferred_lft forever
2: v0    inet6 fd00:beef::8/64 scope global nodad dynamic \       valid_lft 299sec preferred_lft 199sec
2: v0    inet6 fd00:beef::7/64 scope global nodad deprecated dynamic \       valid_lft 399sec preferred_lft 0sec`

// The same deprecated address as a tool that has no NAME for
// IFA_F_DEPRECATED would print it.
//
// IT IS NOT A CAPTURE and it is not labelled as one. busybox 1.36.1
// names `deprecated` (MEASURED 2026-09-06, recorded in the header of
// v6addrflags.go with the rest of that pass), so no tool this suite
// meets prints the line below today. It is here because the parser
// reads the residual `flags <hex>` word as well as the name, that path
// is the one that survives a tool which stops naming the flag, and
// nothing else in this file drives it for IFA_F_DEPRECATED. 0x20 is
// unix.IFA_F_DEPRECATED, and the assertion below reads the constant
// rather than trusting the hex written here.
const deprecatedResidual = `2: v0    inet6 fd00:beef::7/64 scope global dynamic flags 22 \       valid_lft 399sec preferred_lft 0sec`

func TestV6AddrFlagsFromAddrShow_ReadsBothLifetimesAndTheDeprecatedBit(t *testing.T) {
	cases := []struct {
		name           string
		out, addr      string
		wantDeprecated bool
		wantValid      V6Lifetime
		wantPreferred  V6Lifetime
		wantNoDAD      bool
	}{
		{
			name: "deprecated by a zero preferred lifetime", out: lifetimesIproute2,
			addr: "fd00:beef::7", wantDeprecated: true, wantNoDAD: true,
			wantValid: V6Lifetime{Seconds: 399}, wantPreferred: V6Lifetime{Seconds: 0},
		},
		{
			name: "an ordinary leased address", out: lifetimesIproute2,
			addr: "fd00:beef::8", wantDeprecated: false, wantNoDAD: true,
			wantValid: V6Lifetime{Seconds: 299}, wantPreferred: V6Lifetime{Seconds: 199},
		},
		{
			name: "no lifetimes at all", out: lifetimesIproute2,
			addr: "fd00:beef::9", wantDeprecated: false, wantNoDAD: true,
			wantValid: V6Lifetime{Forever: true}, wantPreferred: V6Lifetime{Forever: true},
		},
		{
			name: "deprecated under a tool that does not name the flag", out: deprecatedResidual,
			addr: "fd00:beef::7", wantDeprecated: true, wantNoDAD: true,
			wantValid: V6Lifetime{Seconds: 399}, wantPreferred: V6Lifetime{Seconds: 0},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := V6AddrFlagsFromAddrShow(c.out, c.addr)
			if !f.Found {
				t.Fatalf("address %s not found in:\n%s", c.addr, c.out)
			}
			if !f.Lifetimes {
				t.Fatalf("the lifetimes were not read from %q. A caller asserting that a "+
					"preferred lifetime reached zero would be handed a zero that means "+
					"'not printed', which is the reading that cannot fail", f.Line)
			}
			if f.Deprecated != c.wantDeprecated {
				t.Errorf("Deprecated = %v, want %v. RFC 4862 section 5.5.4's state is the "+
					"KERNEL's to report, and this is where #819's deprecation arm reads it: %q",
					f.Deprecated, c.wantDeprecated, f.Line)
			}
			if f.Valid != c.wantValid || f.Preferred != c.wantPreferred {
				t.Errorf("valid=%s preferred=%s, want valid=%s preferred=%s: %q",
					f.Valid, f.Preferred, c.wantValid, c.wantPreferred, f.Line)
			}
			if f.NoDAD != c.wantNoDAD {
				t.Errorf("NoDAD = %v, want %v: %q", f.NoDAD, c.wantNoDAD, f.Line)
			}
		})
	}

	// The residual rendering above claims 0x22 is NODAD plus
	// DEPRECATED. That is a claim about the kernel's headers, so it is
	// read from them rather than asserted by a passing parse: a typo in
	// the hex would otherwise show up as a parser defect somewhere else
	// entirely, or not at all.
	if unix.IFA_F_NODAD|unix.IFA_F_DEPRECATED != 0x22 {
		t.Fatalf("IFA_F_NODAD|IFA_F_DEPRECATED = %#x, and the residual rendering above is "+
			"written as `flags 22`; the two have to be the same bits or that case drives "+
			"a flag combination no kernel produces",
			unix.IFA_F_NODAD|unix.IFA_F_DEPRECATED)
	}

	// The other direction: an address the output does not carry reads
	// as no lifetimes, never as zero ones.
	if f := V6AddrFlagsFromAddrShow(lifetimesIproute2, "fd00:beef::99"); f.Found || f.Lifetimes {
		t.Errorf("an absent address read as Found=%v Lifetimes=%v; both must be false, or "+
			"'preferred lifetime is 0' is satisfied by an address that is not on the link",
			f.Found, f.Lifetimes)
	}
}

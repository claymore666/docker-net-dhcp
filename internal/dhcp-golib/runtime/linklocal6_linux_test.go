//go:build linux

package runtime

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"
)

// netlinkAddrMsg builds one RTM_NEWADDR message the way the KERNEL lays it
// out: a sixteen-byte nlmsghdr, then struct ifaddrmsg, then rtattrs, each
// padded to four bytes.
//
// The fixture writes the bytes and the subject reads them; nothing is shared
// between the two but this comment and include/uapi/linux/if_addr.h. attrType
// is a parameter so that a message carrying the address under the WRONG
// attribute can be built, and payload is []byte rather than netip.Addr so that
// an address of the wrong length can be.
func netlinkAddrMsg(msgType uint16, family, prefixLen, flags, scope byte, index uint32, attrType uint16, payload []byte) []byte {
	const nlmsghdr = 16
	attrLen := 4 + len(payload)
	attr := make([]byte, (attrLen+3)&^3)
	binary.NativeEndian.PutUint16(attr[0:2], uint16(attrLen))
	binary.NativeEndian.PutUint16(attr[2:4], attrType)
	copy(attr[4:], payload)

	ifa := make([]byte, syscall.SizeofIfAddrmsg)
	ifa[0], ifa[1], ifa[2], ifa[3] = family, prefixLen, flags, scope
	binary.NativeEndian.PutUint32(ifa[4:8], index)

	body := append(ifa, attr...)
	msg := make([]byte, nlmsghdr+len(body))
	binary.NativeEndian.PutUint32(msg[0:4], uint32(len(msg)))
	binary.NativeEndian.PutUint16(msg[4:6], msgType)
	copy(msg[nlmsghdr:], body)
	return msg
}

// bytes6 is the fixture's own way of writing an IPv6 address as sixteen bytes.
func bytes6(t *testing.T, s string) []byte {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("the fixture's own address %q does not parse: %v", s, err)
	}
	b := a.As16()
	return b[:]
}

// TestInterfaceLinkLocalRefusesAnAddressTheKernelIsStillChecking drives every
// arm of the parse from a fabricated netlink dump.
//
// IT DRIVES readLinkLocal AND NOT InterfaceLinkLocal, which is the half of the
// split that costs nothing and the reason the split exists: the exported
// function repeats this parse until linkLocalWait runs out, so four refusing
// cases through it would spend sixteen seconds proving something about a
// parse. The wait is driven where it can be driven honestly — against a real
// link whose address arrives late, and against one that never gets an address
// — in dnsmasq6_linux_test.go.
//
// IT EXISTS BECAUSE THE INTERESTING CASES CANNOT BE ARRANGED ON A REAL LINK.
// The tentative window is about a second wide on a freshly-upped veth and
// nothing can hold it open; a dadfailed link-local needs a second node
// answering for a MAC-derived address; and an interface with a global address
// and no link-local at all is a configuration this fixture cannot build. The
// netns proofs use the real kernel and cover the ordinary case — this covers
// the ones the kernel will not produce on demand.
//
// EVERY ARM IS THE ONE THE /proc/net/if_inet6 TABLE HAD BEFORE M7c's thread
// round, moved to the mechanism that replaced it. The interface is matched by
// INDEX now and not by a name in the sixth column, which is why the two
// "another interface" cases below name an index; the flag values are
// include/uapi/linux/if_addr.h's, and 0xc0 (permanent | tentative) is the
// value MEASURED on a veth that has just come up.
func TestInterfaceLinkLocalRefusesAnAddressTheKernelIsStillChecking(t *testing.T) {
	const (
		llA    = "fe80::3802:f6ff:fe2c:9d01"
		llB    = "fe80::aabb:ccff:fe11:2233"
		global = "fd00:99::42"
		ours   = 3
		theirs = 2
	)
	newAddr := func(index uint32, flags byte, a string) []byte {
		return netlinkAddrMsg(syscall.RTM_NEWADDR, syscall.AF_INET6, 64, flags, 0x20, index, syscall.IFA_ADDRESS, bytes6(t, a))
	}

	cases := []struct {
		name    string
		msgs    func() [][]byte
		want    string
		wantErr bool
		// inMsg is a substring the failure message must carry, so that the
		// two refusals are distinguishable by a reader and not only by a
		// counter.
		inMsg string
	}{
		{
			name: "a settled link-local is returned",
			msgs: func() [][]byte { return [][]byte{newAddr(ours, 0x80, llA)} },
			want: llA,
		},
		{
			name:    "a tentative link-local is refused",
			msgs:    func() [][]byte { return [][]byte{newAddr(ours, 0xc0, llA)} },
			wantErr: true,
			inMsg:   "1 tentative, 0 failed",
		},
		{
			name:    "one that failed the kernel's own check is refused",
			msgs:    func() [][]byte { return [][]byte{newAddr(ours, 0x88, llA)} },
			wantErr: true,
			inMsg:   "0 tentative, 1 failed",
		},
		{
			// THE ORDER THIS ANSWERS IN IS THE POINT. A tentative address
			// first and a settled one after is exactly the state a link is in
			// while it is coming up with two addresses, and returning the
			// first message that names the interface would send from an
			// address the kernel has not finished checking.
			name: "a settled address after a tentative one is still found",
			msgs: func() [][]byte {
				return [][]byte{newAddr(ours, 0xc0, llA), newAddr(ours, 0x80, llB)}
			},
			want: llB,
		},
		{
			name: "another interface's link-local is not this one's",
			msgs: func() [][]byte {
				return [][]byte{newAddr(theirs, 0x80, llA), newAddr(ours, 0x80, llB)}
			},
			want: llB,
		},
		{
			// The scope column says "global" and this code does not read it:
			// the address itself is what decides, which is why a fabricated
			// scope cannot talk it into returning a routable address as a
			// link-local one.
			name: "a global address on the interface is not a link-local",
			msgs: func() [][]byte {
				return [][]byte{netlinkAddrMsg(syscall.RTM_NEWADDR, syscall.AF_INET6, 64, 0x80, 0x00, ours, syscall.IFA_ADDRESS, bytes6(t, global))}
			},
			wantErr: true,
			inMsg:   "0 tentative, 0 failed",
		},
		{
			name:    "an interface with no addresses at all",
			msgs:    func() [][]byte { return [][]byte{newAddr(theirs, 0x80, llA)} },
			wantErr: true,
			inMsg:   ErrNoLinkLocal.Error(),
		},
		{
			// Messages that are not RTM_NEWADDR, ones too short to hold an
			// ifaddrmsg, ones whose address is under another attribute and
			// ones whose address is not sixteen bytes are SKIPPED and not
			// fatal: this dump is written by the kernel, and a client that
			// refused to start because one message of it was unfamiliar would
			// be refusing on the strength of a format it does not own.
			name: "unfamiliar messages are skipped rather than fatal",
			msgs: func() [][]byte {
				// Well framed and four-byte aligned, so the stream still
				// parses, but its body is too short to hold an ifaddrmsg.
				short := make([]byte, 20)
				binary.NativeEndian.PutUint32(short[0:4], 20)
				binary.NativeEndian.PutUint16(short[4:6], syscall.RTM_NEWADDR)
				return [][]byte{
					netlinkAddrMsg(syscall.RTM_NEWLINK, syscall.AF_INET6, 64, 0x80, 0x20, ours, syscall.IFA_ADDRESS, bytes6(t, llA)),
					short,
					netlinkAddrMsg(syscall.RTM_NEWADDR, syscall.AF_INET6, 64, 0x80, 0x20, ours, syscall.IFA_CACHEINFO, bytes6(t, llA)),
					netlinkAddrMsg(syscall.RTM_NEWADDR, syscall.AF_INET6, 64, 0x80, 0x20, ours, syscall.IFA_ADDRESS, bytes6(t, llA)[:4]),
					newAddr(ours, 0x80, llB),
				}
			},
			want: llB,
		},
		{
			// NLMSG_DONE closes every dump syscall.NetlinkRIB returns and is
			// not an address; a parse that counted it would refuse a dump
			// that had nothing wrong with it.
			name: "the message that ends the dump is not an address",
			msgs: func() [][]byte {
				done := make([]byte, 16)
				binary.NativeEndian.PutUint32(done[0:4], 16)
				binary.NativeEndian.PutUint16(done[4:6], syscall.NLMSG_DONE)
				return [][]byte{newAddr(ours, 0x80, llA), done}
			},
			want: llA,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dump []byte
			for _, m := range tc.msgs() {
				dump = append(dump, m...)
			}
			old := linkLocalDump
			linkLocalDump = func() ([]byte, error) { return dump, nil }
			defer func() { linkLocalDump = old }()

			got, err := readLinkLocal(3, "v6cli0")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("readLinkLocal returned %s; want a refusal", got)
				}
				if !errors.Is(err, ErrNoLinkLocal) {
					t.Errorf("error %v does not wrap ErrNoLinkLocal, so a caller cannot tell this apart from a dump that failed", err)
				}
				if !strings.Contains(err.Error(), tc.inMsg) {
					t.Errorf("error %q does not carry %q; the two refusals have different fixes and the message is where they are told apart", err, tc.inMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("readLinkLocal: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("readLinkLocal = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestADumpThatDoesNotParseIsNotAnAbsentAddress is the third outcome the two
// tables above do not carry.
//
// A dump whose framing is broken — a length field that does not agree with the
// bytes after it — is neither an address nor an interface without one. Folded
// into ErrNoLinkLocal it would be retried for the whole of linkLocalWait and
// then reported as a link that never got an address, which sends a reader to
// the wrong place for four seconds longer than necessary.
func TestADumpThatDoesNotParseIsNotAnAbsentAddress(t *testing.T) {
	torn := make([]byte, 24)
	binary.NativeEndian.PutUint32(torn[0:4], 4096) // longer than what follows
	binary.NativeEndian.PutUint16(torn[4:6], syscall.RTM_NEWADDR)

	old := linkLocalDump
	linkLocalDump = func() ([]byte, error) { return torn, nil }
	defer func() { linkLocalDump = old }()

	start := time.Now()
	_, err := InterfaceLinkLocal("lo")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("InterfaceLinkLocal succeeded on a dump that does not parse")
	}
	if errors.Is(err, ErrNoLinkLocal) {
		t.Errorf("error %v wraps ErrNoLinkLocal; a dump that does not parse is not an interface without an address", err)
	}
	if elapsed >= linkLocalWait {
		t.Errorf("the refusal took %s, which is linkLocalWait (%s) or more: a torn dump was retried as though the link had not settled", elapsed, linkLocalWait)
	}
}

// TestInterfaceLinkLocalReportsAKernelItCannotAsk is the other direction: a
// dump that fails is an ERROR and never "this interface has no address".
//
// The distinction is the one InterfaceLinkLocal ACTS on: an interface with no
// address yet is waited for, and a kernel that will not answer is not. So this
// case is also the only place the exported function's refusal path is driven
// without a network namespace, and it asserts the timing as well as the error
// — a failed dump retried for linkLocalWait would still return the right
// error, four seconds later, on every client this library ever builds on a
// host where the query is refused.
//
// IT GOES THROUGH THE LOOPBACK INTERFACE, which exists in every namespace this
// test can run in, because the exported function now resolves the interface
// BEFORE it reads anything: a name that does not resolve is its own error and
// would never reach the dump.
func TestInterfaceLinkLocalReportsAKernelItCannotAsk(t *testing.T) {
	refused := errors.New("the kernel said no")
	old := linkLocalDump
	linkLocalDump = func() ([]byte, error) { return nil, refused }
	defer func() { linkLocalDump = old }()

	start := time.Now()
	_, err := InterfaceLinkLocal("lo")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("InterfaceLinkLocal succeeded with no dump to read")
	}
	if elapsed >= linkLocalWait {
		t.Errorf("the refusal took %s, which is linkLocalWait (%s) or more: a failed dump was retried as though the link had not settled", elapsed, linkLocalWait)
	}
	if errors.Is(err, ErrNoLinkLocal) {
		t.Errorf("error %v wraps ErrNoLinkLocal; a kernel that will not answer is not an interface without an address", err)
	}
	if !errors.Is(err, refused) {
		t.Errorf("error %v does not wrap the failure the kernel reported, so what actually went wrong is not recoverable from it", err)
	}
}

// TestInterfaceLinkLocalReportsAnInterfaceThatIsNotThere is the case the old
// /proc read could not tell apart from an address that had not arrived, and it
// is the one that made the CI runner's failure unreadable.
//
// A client built in the wrong network namespace does not see the interface at
// all. Reported as ErrNoLinkLocal after four seconds it reads as "this link is
// slow"; reported here it reads as what it is, at once, and it never enters
// the wait.
func TestInterfaceLinkLocalReportsAnInterfaceThatIsNotThere(t *testing.T) {
	const absent = "v6cli0-nope"
	if _, err := net.InterfaceByName(absent); err == nil {
		t.Fatalf("%s exists on this host, so this case cannot be made", absent)
	}
	reached := 0
	old := linkLocalDump
	linkLocalDump = func() ([]byte, error) { reached++; return nil, nil }
	defer func() { linkLocalDump = old }()

	start := time.Now()
	_, err := InterfaceLinkLocal(absent)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("InterfaceLinkLocal succeeded on an interface that does not exist")
	}
	if reached != 0 {
		t.Errorf("the kernel was asked for its addresses %d time(s) for an interface that does not exist", reached)
	}
	if errors.Is(err, ErrNoLinkLocal) {
		t.Errorf("error %v wraps ErrNoLinkLocal; an interface that is not there is not an interface waiting for an address, and the whole cost of confusing the two is this milestone", err)
	}
	if elapsed >= linkLocalWait {
		t.Errorf("the refusal took %s, which is linkLocalWait (%s) or more: a missing interface was waited for", elapsed, linkLocalWait)
	}
	if !strings.Contains(err.Error(), absent) {
		t.Errorf("the refusal %q does not name %s", err, absent)
	}
}

// TestTheLinkLocalDumpAsksTheKernelForIPv6Addresses is the seam's own case:
// every table above hands readLinkLocal bytes, so nothing else in this file
// executes the one line that decides WHICH question the kernel is asked.
//
// It asserts the shape of the answer rather than its contents, because the
// contents are whatever this host holds: the dump must parse as netlink
// messages, and every RTM_NEWADDR in it must carry an address of the length
// AF_INET6 addresses have. A dump taken for AF_INET parses just as well and
// carries four-byte addresses, which is the substitution this catches.
func TestTheLinkLocalDumpAsksTheKernelForIPv6Addresses(t *testing.T) {
	b, err := linkLocalDump()
	if err != nil {
		t.Fatalf("the link-local dump: %v", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(b)
	if err != nil {
		t.Fatalf("the dump does not parse as netlink messages: %v", err)
	}
	seen := 0
	for i := range msgs {
		m := &msgs[i]
		if m.Header.Type != syscall.RTM_NEWADDR {
			continue
		}
		attrs, err := syscall.ParseNetlinkRouteAttr(m)
		if err != nil {
			t.Fatalf("an RTM_NEWADDR message's attributes do not parse: %v", err)
		}
		for _, a := range attrs {
			if a.Attr.Type != syscall.IFA_ADDRESS {
				continue
			}
			seen++
			if len(a.Value) != 16 {
				t.Fatalf("an address in the dump is %d byte(s) long; AF_INET6 addresses are 16, so this dump was taken for another family: %v", len(a.Value), a.Value)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the dump carried no IPv6 address at all; this host has at least the loopback's ::1, so the question asked was not the one this file thinks it asks")
	}
	t.Logf("the kernel answered with %d IPv6 address(es) over %d netlink message(s)", seen, len(msgs))
}

//go:build linux

package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrNoLinkLocal is returned when an interface has no usable link-local
// address for this library to send from.
var ErrNoLinkLocal = errors.New("runtime: interface has no assigned IPv6 link-local address")

// linkLocalDump asks the kernel for every IPv6 address it holds, as raw
// netlink messages.
//
// A variable so a test can hand the parse a fabricated dump and drive every
// arm of it without a network namespace, and so that the failure direction —
// a kernel this process cannot ask — has a seam to fail at.
var linkLocalDump = func() ([]byte, error) {
	return syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_INET6)
}

// Linux's per-address flags, from include/uapi/linux/if_addr.h. Only the two
// this library acts on are named; the rest are read and ignored.
//
// BOUND: they are read out of the eight-bit ifa_flags field of the ifaddrmsg
// header and not out of the thirty-two-bit IFA_FLAGS attribute beside it. Both
// carry the same value, truncated; the two named here are 0x08 and 0x40 and
// fit. A flag above 0xff would not, and none of them is one this library acts
// on.
const (
	ifaDADFailed = 0x08
	ifaTentative = 0x40
)

// linkLocalWait is how long InterfaceLinkLocal keeps looking before it
// refuses, and linkLocalPoll is how often it looks.
//
// TWO SECONDS ARE DERIVED AND MEASURED; THE OTHER TWO ARE A MARGIN. The
// kernel does two things between the moment a link comes up and the moment its
// link-local address is usable, and RFC 4862 section 5.4.2 names both. First it
// waits: "If the Neighbor Solicitation is going to be the first message sent
// from an interface after interface (re)initialization, the node SHOULD delay
// joining the solicited-node multicast address by a random delay between 0 and
// MAX_RTR_SOLICITATION_DELAY as specified in [RFC4861]" — one second, RFC 4861
// section 10. Then it probes: "To check an address, a node sends
// DupAddrDetectTransmits Neighbor Solicitations, each separated by RetransTimer
// milliseconds" — one solicitation and one RETRANS_TIMER of listening at RFC
// 4861 section 10's defaults, another second. Linux spells those two variables
// dad_transmits and retrans_time, and the address is tentative for the whole of
// it. Two seconds is the kernel's own worst case at the defaults, MEASURED at
// 1.973s and 2.043s on a veth whose accept_dad was left on.
//
// THE REMAINING TWO SECONDS ARE A MARGIN AND ARE DERIVED FROM NOTHING. They
// cover a poll that is scheduled late: the loop below observes the end of the
// kernel's window from a goroutine, and on a loaded two-core host a twenty
// millisecond tick is not a twenty millisecond tick. It is stated as a margin
// rather than dressed as a derivation. An earlier version of this comment said
// the doubling was demanded by a measurement — a CI runner on which the address
// was ABSENT rather than tentative for four seconds. That reading is WITHDRAWN
// (2026-09-06): the address was not absent, the read was of the wrong network
// namespace. See InterfaceLinkLocal. No measurement of a long absence exists.
//
// IT IS A CONSTANT AND NOT A READING OF THE TWO SYSCTLS, deliberately. The
// seam design's section 5 keeps this library off /proc/sys: a library that
// reads a host's tuning has to decide what to do when it disagrees with the
// host, and nothing here can. The cost is stated rather than hidden: on a host
// that has raised dad_transmits or retrans_time above the defaults this bound
// is too short, and the refusal says how long it actually waited so that a
// reader can tell that case from a link with no address at all.
//
// WHAT THE POLL COSTS, as a function of the host and not as a count. Each tick
// is one RTM_GETADDR dump of EVERY IPv6 address in the calling thread's
// network namespace, because syscall.NetlinkRIB does not negotiate the strict
// checking that would let the kernel honour a per-interface filter in the
// request. So the worst case is two hundred whole-namespace address dumps, on
// a link whose address never arrives, and it is proportional to the number of
// IPv6 addresses the host holds rather than to the one being waited for.
//
// A NETLINK SUBSCRIPTION would give the answer on the kernel's edge instead of
// on a clock and would cost one message rather than two hundred dumps. It is
// the better instrument and it is not what this does, because it is a second
// socket that would have to be opened, and its subscription established, before
// the thing it is waiting for exists — and because the dump is the same
// mechanism net.InterfaceByName already uses three lines earlier.
const (
	linkLocalWait = 4 * time.Second
	linkLocalPoll = 20 * time.Millisecond
)

// InterfaceLinkLocal returns the IPv6 link-local address the KERNEL has
// assigned to ifName, refusing one that is tentative or has failed duplicate
// address detection, and WAITING up to linkLocalWait for one to appear.
//
// IT IS A NETLINK DUMP AND NOT A READ OF /proc/net/if_inet6, and that is this
// function's defect history rather than a preference.
//
// MEASURED 2026-09-06. The kernel resolves /proc/net — and /proc/self/net,
// which it is a symlink to — against the THREAD GROUP LEADER's network
// namespace, not the calling thread's (fs/proc/proc_net.c, get_proc_task_net,
// which takes pid_task on the tgid). From a locked goroutine on a non-leader
// thread that had entered another namespace, /proc/net/if_inet6 and
// /proc/self/net/if_inet6 listed no row for the interface that had just been
// created there, while /proc/thread-self/net/if_inet6 and
// /proc/<pid>/task/<tid>/net/if_inet6 listed it — three runs of three, both a
// hundred milliseconds and two and a half seconds after the link came up.
//
// So the version of this function that read /proc/net answered for whichever
// namespace the process leader happened to be in. On a host where no interface
// of that name existed there it refused, with "0 tentative, 0 failed duplicate
// address detection" — a truthful sentence about the wrong file, which is what
// a CI runner reported intermittently and what an earlier round read as
// addrconf latency. Where an interface of that name DID exist there, it
// returned that interface's address, and the client would have sent from an
// address on one link over sockets bound to another. That is the shape the
// plugin's chassis meets on its first endpoint: it builds a client from a
// goroutine locked to a thread that has entered the endpoint's namespace while
// the process leader stays on the host.
//
// Every other namespace-bearing thing this package opens — the AF_PACKET
// transports, the Neighbor Discovery socket, net.InterfaceByName — is a socket
// or a netlink dump on the CALLING THREAD. This is now one of them.
//
// A DUMP THAT FAILS IS NOT WAITED OUT. A kernel that refuses the query is not a
// link that has not settled, and retrying it for four seconds turns a clear
// failure into a slow one. Neither is an interface that does not exist: it is
// resolved once, before the wait, and its absence is its own error rather than
// four seconds of "this interface has no address".
//
// WHY IT WAITS AT ALL. RFC 4862 section 5.4 makes an address tentative until
// the kernel's own duplicate check finishes, and a link that has just come up
// spends a second or two there; a single read answers a question about an
// interface that is still being configured, and the answer it gives is
// indistinguishable from the answer for an interface that will never have an
// address. The refusal carries the elapsed wait for the same reason.
//
// WHY THE KERNEL'S AND NOT ONE DERIVED FROM THE MAC. Both are one line of
// arithmetic and they agree on an ordinary Linux host, so the choice looks
// free; it is not. A DHCPv6 server's reply is UNICAST to the source address of
// the message it answers — RFC 9915 section 18.3.10: "the server unicasts the
// Advertise or Reply message directly to the client using the address in the
// source address field from the IP datagram in which the original message was
// received" — and section 5 says which address that field carries: "The client
// uses a link-local source address or addresses determined through other
// mechanisms for transmitting and receiving DHCP messages." Delivering that
// unicast means resolving the address with a Neighbor Solicitation that THIS
// LIBRARY DOES NOT ANSWER — the kernel does, and only for an address the
// kernel holds. RFC 4291 Appendix A's modified EUI-64 is what Linux forms when
// the interface's addr_gen_mode is 0, which is the default and is MEASURED for
// a veth pair in an unprivileged network namespace in this milestone; it is
// NOT what a host doing RFC 7217 stable-privacy or RFC 8981 temporary
// addressing forms, and on such a host a MAC-derived source names an address
// nothing on the link answers for. The client would then send perfectly valid
// Solicits and time out with nothing to point at.
//
// So the address is READ, and wire.LinkLocalFromMAC stays what its own comment
// says it is — the fixture's way of predicting what the kernel will do. The
// netns proof asserts the two agree there, which is what keeps this from being
// an unexamined divergence: the derivation is checked against the kernel in
// the one environment where both are available.
//
// TENTATIVE AND DAD-FAILED ADDRESSES ARE REFUSED. RFC 4862 section 5.4: "An
// address on which the Duplicate Address Detection procedure is applied is
// said to be tentative until the procedure has completed successfully. A
// tentative address is not considered 'assigned to an interface' in the
// traditional sense." Sending from one is sending from an address that is not
// assigned, which is what section 5.4's whole procedure exists to prevent, and
// ifa_flags is where Linux says so — MEASURED: a freshly-upped veth reads flag
// 0xc0 (permanent | tentative) and settles to 0x80 about a second later.
//
// BOUND: the namespace it reports is the namespace of the thread this
// goroutine is running on, and a goroutine that is not locked to its thread
// does not have one. That is the contract every socket constructor in this
// package carries, it is the reason NewClient6 opens all of them together, and
// it cannot be closed from inside this function.
func InterfaceLinkLocal(ifName string) (netip.Addr, error) {
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("runtime: interface %q: %w", ifName, err)
	}
	return interfaceLinkLocal(iface.Index, ifName)
}

// interfaceLinkLocal is InterfaceLinkLocal with the interface already
// resolved, for the one caller that has just resolved it: the index the
// transport BINDS to and the index whose address is read are then one reading
// and cannot drift apart, which is the same failure this file is about wearing
// a smaller size.
func interfaceLinkLocal(index int, ifName string) (netip.Addr, error) {
	start := time.Now()
	for {
		addr, err := readLinkLocal(index, ifName)
		if err == nil {
			return addr, nil
		}
		if !errors.Is(err, ErrNoLinkLocal) {
			return netip.Addr{}, err
		}
		if elapsed := time.Since(start); elapsed >= linkLocalWait {
			// The elapsed wait is the ACTUAL one and not the constant: a
			// reader who sees a number much larger than linkLocalWait knows
			// this goroutine was not running for most of it, which on a
			// loaded host is the more useful of the two facts.
			return netip.Addr{}, fmt.Errorf("%w after waiting %s", err, elapsed.Round(time.Millisecond))
		}
		time.Sleep(linkLocalPoll)
	}
}

// readLinkLocal is one dump and the parse of it, with no wait around it.
//
// It is separate so that the parse's arms — a tentative address, one that
// failed the kernel's check, a global address, a malformed message — can be
// driven from a fabricated dump without paying linkLocalWait for each refusing
// case, and so that the wait above has exactly one thing to repeat.
func readLinkLocal(index int, ifName string) (netip.Addr, error) {
	b, err := linkLocalDump()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("runtime: netlink RTM_GETADDR (AF_INET6): %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(b)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("runtime: netlink RTM_GETADDR (AF_INET6): %w", err)
	}

	var (
		refusedTentative int
		refusedDADFailed int
	)
	for i := range msgs {
		m := &msgs[i]
		if m.Header.Type != syscall.RTM_NEWADDR || len(m.Data) < syscall.SizeofIfAddrmsg {
			continue
		}
		// struct ifaddrmsg: family, prefixlen, flags, scope, then the
		// interface index in the host's byte order.
		if int(binary.NativeEndian.Uint32(m.Data[4:8])) != index {
			continue
		}
		flags := uint32(m.Data[2])
		attrs, err := syscall.ParseNetlinkRouteAttr(m)
		if err != nil {
			// A message whose attribute area does not parse is SKIPPED and
			// not fatal, for the reason the whole dump is: it is written by
			// the kernel, and a client that refused to start because one
			// message of it was unfamiliar would be refusing on the strength
			// of a format it does not own.
			continue
		}
		var raw []byte
		for _, a := range attrs {
			if a.Attr.Type == syscall.IFA_ADDRESS {
				raw = a.Value
			}
		}
		if len(raw) != 16 {
			continue
		}
		addr := netip.AddrFrom16([16]byte(raw))
		if !addr.IsLinkLocalUnicast() {
			continue
		}
		switch {
		case flags&ifaDADFailed != 0:
			refusedDADFailed++
		case flags&ifaTentative != 0:
			refusedTentative++
		default:
			return addr, nil
		}
	}
	// The counts are in the message rather than dropped, because "the
	// interface has no link-local address" and "it has one and the kernel's
	// own duplicate address detection has not finished with it" are different
	// problems with different fixes, and a caller that sees only the first
	// wording waits for the wrong thing.
	return netip.Addr{}, fmt.Errorf("%w: %s (%d tentative, %d failed duplicate address detection)",
		ErrNoLinkLocal, ifName, refusedTentative, refusedDADFailed)
}

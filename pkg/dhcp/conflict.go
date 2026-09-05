// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// DefaultConflictCheck is the `conflict_check` value a network that never
// set one runs under.
//
// IT IS READ FROM THE LIBRARY, not written here. The mode the chassis
// defaults to and the mode the library defaults to have to be the same
// mode, and the only way to say that once is to derive one from the
// other: proto.Params.Conflict's zero value is what an unset
// Params.Conflict means on the wire, and this is that value's name. A
// literal "wait" here would be a second spelling of a fact the library
// owns, and the two would agree until the day they did not.
var DefaultConflictCheck = proto.ConflictMode(0).String()

// ConflictModes is every value `conflict_check` accepts, in the
// library's own order.
//
// Derived for the same reason: this package spells no mode name
// anywhere. A mode added to the library appears here without an edit,
// and a mode renamed there cannot leave the chassis matching a string
// nothing produces any more.
func ConflictModes() []string {
	all := proto.AllConflictModes()
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.String())
	}
	return out
}

// ParseConflictCheck turns the operator's `conflict_check` value into
// the library's mode.
//
// An empty value is the default. Anything else must be one of the names
// the library prints; a value that is not is REFUSED and never quietly
// resolved to the zero value, because the zero value is `wait` and a
// typo would then silently buy the safest, slowest mode on a network
// whose operator asked for the fastest.
func ParseConflictCheck(v string) (proto.ConflictMode, error) {
	if v == "" {
		return proto.ConflictMode(0), nil
	}
	for _, m := range proto.AllConflictModes() {
		if m.String() == v {
			return m, nil
		}
	}
	return 0, fmt.Errorf("conflict_check %q is not one of %s", v, strings.Join(ConflictModes(), ", "))
}

// ConflictWindow is the longest RFC 5227 section 2.1 can hold an
// address back after the DHCPACK, for a client running these constants.
//
// Section 2.1.1's schedule, term by term, and section 2.1's completion
// condition — which is NOT "the last probe was sent":
//
//	initial delay      uniform [0, PROBE_WAIT]                <= PROBE_WAIT
//	PROBE_NUM-1 gaps   uniform [PROBE_MIN, PROBE_MAX] each    <= (n-1) * PROBE_MAX
//	ANNOUNCE_WAIT      "If, by ANNOUNCE_WAIT seconds after
//	                   the transmission of the last ARP Probe
//	                   no conflicting ARP Reply or ARP Probe
//	                   has been received, then the host has
//	                   successfully determined that the
//	                   desired address may be used safely"    == ANNOUNCE_WAIT
//
// The announcements are NOT in it: section 2.3 says "the host may begin
// legitimately using the IP address immediately after sending the first
// of the two ARP Announcements", and the library emits Acquired there.
//
// With the RFC's table that is 1 + 2*2 + 2 = 7s at worst, 4s at best.
func ConflictWindow(p proto.ACDParams) time.Duration {
	gaps := p.ProbeNum - 1
	if gaps < 0 {
		gaps = 0
	}
	return time.Duration(p.ProbeWait) +
		time.Duration(gaps)*time.Duration(p.ProbeMax) +
		time.Duration(p.AnnounceWait)
}

// AcquisitionWindow is the longest a proto.ConflictWait acquisition can
// take from the first DHCPDISCOVER to Acquired, on a quiet link, with
// one retransmission — which is what CreateEndpoint's deadline has to
// cover if a single lost DISCOVER is not to fail `docker run`.
//
// Two terms, and nothing else on a quiet link:
//
//   - one DISCOVER retransmission. RFC 2131 section 4.1's schedule is
//     "four seconds randomized by the value of a uniform random number
//     chosen from the range -1 to +1", which is Backoff.Delay(0) at its
//     upper end. The OFFER, DHCPREQUEST and DHCPACK that follow add no
//     further wait when the server is answering.
//   - the whole of ConflictWindow, after the ACK.
//
// RFC 2131 section 4.4.1's desync is deliberately NOT in it: the
// chassis zeroes DesyncMin/DesyncMax on both managers (see
// buildParams), so the first packet is at t=0.
//
// With the library's defaults: (4+1) + 7 = 12.0s.
func AcquisitionWindow(p proto.Params) time.Duration {
	acd := p.ACD
	if acd == (proto.ACDParams{}) {
		acd = proto.DefaultACDParams()
	}
	retransmit := time.Duration(p.Discover.Initial + p.Discover.Jitter)
	return retransmit + ConflictWindow(acd)
}

// ConflictRecoveryWindow is the longest a proto.ConflictWait
// acquisition can take when the FIRST address the server offers is
// already in use — which is the case conflict detection exists for,
// and therefore the case the default deadline has to fund.
//
// MEASURED on the 2.x lane 2026-09-04 with a deadline of
// AcquisitionWindow alone: the squatter answered the probe, the library
// sent the DHCPDECLINE, waited RFC 2131 section 3.1(5)'s ten seconds,
// asked again and was granted a clean address 10.7s after the first
// ACK — and the chassis had given up 0.8s earlier. `docker run` failed
// with "context deadline exceeded" for a container that had a good
// address waiting for it, which is exactly the failure GetIP's own
// comment refuses to cause by returning early. A deadline can cause it
// too, and a number derived for the quiet-link case does.
//
// Three terms:
//
//	AcquisitionWindow   the first attempt, up to and including the
//	                    probe window in which the conflict is found
//	RestartDelay        RFC 2131 section 3.1(5): "The client SHOULD
//	                    wait a minimum of ten seconds before restarting
//	                    the configuration process"
//	AcquisitionWindow   the second attempt, which gets its own
//	                    retransmission allowance because it is a fresh
//	                    exchange and a lost DISCOVER is no less likely
//
// ONE conflict, not a squatted segment. Two conflicting addresses in a
// row is a network to fix, not a timeout to raise, and the operator
// raises `lease_timeout` for it. With the library's constants:
// 12.0 + 10.0 + 12.0 = 34.0s.
//
// What this does NOT change is the cost of a SILENT server: nothing
// waits here unless a conflict happened, and a deadline is a ceiling
// rather than a price. What it does change is how long `docker run`
// takes to give up on a segment with no DHCP server at all — from 12s
// to 34s — and that is stated in the option's documentation rather
// than left for an operator to time.
func ConflictRecoveryWindow(p proto.Params) time.Duration {
	// Zero means the library's default rather than "no wait" — its own
	// decision of 2026-08-30, stated on Params.RestartDelay — so it is
	// resolved the same way here. Reading a zero as zero would derive a
	// deadline ten seconds shorter than the wait the library is about
	// to take, which is the shape this whole function exists to close.
	restart := time.Duration(p.RestartDelay)
	if restart <= 0 {
		restart = time.Duration(proto.DefaultRestartDelay)
	}
	return AcquisitionWindow(p) + restart + AcquisitionWindow(p)
}

// ErrLeaseTimeoutTooShort is a `lease_timeout` that cannot fund one
// proto.ConflictWait acquisition however fast the server answers.
//
// It is a REFUSAL and not a warning because the failure it prevents is
// total and silent: below the probe window no `wait` acquisition can
// ever return a lease, so every `docker run` on the network fails with
// a DHCP timeout while the DHCP server is answering perfectly.
type ErrLeaseTimeoutTooShort struct {
	Timeout time.Duration
	Window  time.Duration
	Params  proto.ACDParams
}

func (e ErrLeaseTimeoutTooShort) Error() string {
	return fmt.Sprintf(
		"lease_timeout %v is shorter than the RFC 5227 section 2.1 probe window that conflict_check=%s waits out "+
			"before the address may be used: PROBE_WAIT %v + (PROBE_NUM-1) * PROBE_MAX %v + ANNOUNCE_WAIT %v = %v. "+
			"No acquisition on this network could ever return a lease. Raise lease_timeout to at least %v, "+
			"or set conflict_check to one of %s",
		e.Timeout, proto.ConflictWait,
		time.Duration(e.Params.ProbeWait),
		time.Duration(e.Params.ProbeMax),
		time.Duration(e.Params.AnnounceWait),
		e.Window, e.Window,
		strings.Join(otherModes(proto.ConflictWait), ", "),
	)
}

// otherModes is every mode but one, for an error that has to name the
// alternatives without spelling any of them.
func otherModes(not proto.ConflictMode) []string {
	out := make([]string, 0, 2)
	for _, m := range proto.AllConflictModes() {
		if m != not {
			out = append(out, m.String())
		}
	}
	sort.Strings(out)
	return out
}

// CheckLeaseTimeout refuses a lease_timeout that cannot fund one
// acquisition in mode.
//
// Only proto.ConflictWait is bounded from below, and that asymmetry is
// the whole rule rather than an omission: it is the only mode that puts
// the probe window in front of Acquired. proto.ConflictAsync returns at
// the DHCPACK and proto.ConflictOff never probes, so any timeout that
// funds a DORA funds them, and refusing a short one there would refuse
// a configuration that works.
//
// A zero timeout means the caller has not set one and will use the
// derived default, which covers the window by construction.
func CheckLeaseTimeout(timeout time.Duration, mode proto.ConflictMode) error {
	if timeout <= 0 || mode != proto.ConflictWait {
		return nil
	}
	acd := proto.DefaultACDParams()
	window := ConflictWindow(acd)
	if timeout < window {
		return ErrLeaseTimeoutTooShort{Timeout: timeout, Window: window, Params: acd}
	}
	return nil
}

// ACDStats is the RFC 5227 half of the library's counters, which the
// plugin exposes process-wide.
//
// Four fields and not lease.Stats entire: these are the ones that answer
// "is the detector running at all", which is the question an operator
// has to be able to answer before reading address_conflicts=0 as a
// clean segment (#524). The rest of lease.Stats is already per-endpoint
// on the durable record.
type ACDStats struct {
	// ProbesSent and AnnouncementsSent are RFC 5227 section 2.1.1's
	// ARP Probes and section 2.3's ARP Announcements. Both move in
	// proto.ConflictWait and proto.ConflictAsync and neither moves in
	// proto.ConflictOff, which is what makes them the evidence that the
	// mode reached the wire.
	ProbesSent        uint64
	AnnouncementsSent uint64
	// ConflictsDetected is the LIBRARY's own count of addresses found
	// in use, by its rules or by a caller's ReportConflict. The chassis
	// counts the same conflicts from the events it receives; the two
	// are separate derivations on purpose, and a disagreement between
	// them is a finding rather than a rounding difference.
	ConflictsDetected uint64
	// ARPSendFailures is probes and announcements the ARP socket
	// refused. A probe that was never sent proves nothing about the
	// address, so this is the difference between "no conflict" and "no
	// question asked".
	ARPSendFailures uint64
}

// acdStats projects the library's counters onto the four above.
func acdStats(s lease.Stats) ACDStats {
	return ACDStats{
		ProbesSent:        s.ProbesSent,
		AnnouncementsSent: s.AnnouncementsSent,
		ConflictsDetected: s.ConflictsDetected,
		ARPSendFailures:   s.ARPSendFailures,
	}
}

// Sub returns the counters gained since prev.
//
// Saturating rather than wrapping: the library's counters only rise
// within one manager, so a negative difference is impossible by
// construction and a nonsense value here would be a huge positive delta
// on an unsigned subtraction — the one direction a monotonic plugin
// counter must never move in.
func (s ACDStats) Sub(prev ACDStats) ACDStats {
	return ACDStats{
		ProbesSent:        sub(s.ProbesSent, prev.ProbesSent),
		AnnouncementsSent: sub(s.AnnouncementsSent, prev.AnnouncementsSent),
		ConflictsDetected: sub(s.ConflictsDetected, prev.ConflictsDetected),
		ARPSendFailures:   sub(s.ARPSendFailures, prev.ARPSendFailures),
	}
}

// IsZero reports whether nothing moved, so a caller can skip the
// accumulation entirely on the overwhelmingly common event.
func (s ACDStats) IsZero() bool { return s == ACDStats{} }

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

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

// DefaultConflictCheck is the `conflict_check` a network that set none runs under, read from the library's zero value
// (#882).
var DefaultConflictCheck = proto.ConflictMode(0).String()

// ConflictModes is every `conflict_check` value, derived from the library so the chassis spells no mode name (#882).
func ConflictModes() []string {
	all := proto.AllConflictModes()
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.String())
	}
	return out
}

// A value outside the set is refused: the zero value is `wait`, and a typo would silently buy the slowest mode (#882).

// ParseConflictCheck turns a `conflict_check` value into the library's mode, empty meaning the default.
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

// RFC 5227 section 2.1.1: delay up to PROBE_WAIT, PROBE_NUM-1 gaps up to PROBE_MAX, then ANNOUNCE_WAIT after the last
// probe (section 2.1). Announcements are out: section 2.3 allows use after the first. The RFC's table gives 1 + 2*2 + 2
// = 7 s at worst.

// ConflictWindow is the longest RFC 5227 section 2.1 can hold an address back after the DHCPACK.
func ConflictWindow(p proto.ACDParams) time.Duration {
	gaps := p.ProbeNum - 1
	if gaps < 0 {
		gaps = 0
	}
	return time.Duration(p.ProbeWait) +
		time.Duration(gaps)*time.Duration(p.ProbeMax) +
		time.Duration(p.AnnounceWait)
}

// One DISCOVER retransmission at RFC 2131 section 4.1's 4 s +1 upper end (Backoff.Delay(0)), plus ConflictWindow; no
// section 4.4.1 desync, since buildParams zeroes it. Library defaults: (4+1) + 7 = 12.0 s (#882).

// AcquisitionWindow is the longest a proto.ConflictWait acquisition takes on a quiet link with one lost DISCOVER.
func AcquisitionWindow(p proto.Params) time.Duration {
	acd := p.ACD
	if acd == (proto.ACDParams{}) {
		acd = proto.DefaultACDParams()
	}
	retransmit := time.Duration(p.Discover.Initial + p.Discover.Jitter)
	return retransmit + ConflictWindow(acd)
}

// Measured on the 2.x lane 2026-09-04: with AcquisitionWindow alone, a DECLINE and RFC 2131 section 3.1(5)'s ten-second
// restart got a clean address 10.7 s after the first ACK, 0.8 s after the chassis gave up (#882). So two
// AcquisitionWindows plus RestartDelay: 12.0 + 10.0 + 12.0 = 34.0 s. It covers one conflict, not a squatted segment,
// and raises the give-up on a serverless segment from 12 s to 34 s, which the option's documentation states.

// ConflictRecoveryWindow is the longest a proto.ConflictWait acquisition takes when the first offer is in use.
func ConflictRecoveryWindow(p proto.Params) time.Duration {
	// Zero is the library's default, not "no wait", its decision of 2026-08-30 (Params.RestartDelay); reading it as
	// zero derives a deadline 10 s short (#882).
	restart := time.Duration(p.RestartDelay)
	if restart <= 0 {
		restart = time.Duration(proto.DefaultRestartDelay)
	}
	return AcquisitionWindow(p) + restart + AcquisitionWindow(p)
}

// A refusal, not a warning: below the probe window every `wait` acquisition times out while the server answers (#882).

// ErrLeaseTimeoutTooShort is a `lease_timeout` that cannot fund one proto.ConflictWait acquisition.
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

// otherModes is every mode but not, for an error that names the alternatives.
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

// Only proto.ConflictWait puts the probe window before Acquired; ConflictAsync returns at the ACK and ConflictOff never
// probes, so refusing them would refuse a working setup. Zero means the derived default (#882).

// CheckLeaseTimeout refuses a lease_timeout that cannot fund one acquisition in mode.
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

// Four fields answer "is the detector running at all", needed before address_conflicts=0 reads as a clean segment
// (#524).

// ACDStats is the RFC 5227 half of the library's counters, exposed process-wide.
type ACDStats struct {
	// ProbesSent and AnnouncementsSent are RFC 5227 section 2.1.1's probes and 2.3's announcements; zero under
	// ConflictOff (#882).
	ProbesSent        uint64
	AnnouncementsSent uint64
	// ConflictsDetected is the library's own count, derived apart from the chassis's event count on purpose (#882).
	ConflictsDetected uint64
	// ARPSendFailures is probes and announcements the ARP socket refused: no probe sent is no question asked (#882).
	ARPSendFailures uint64
}

func acdStats(s lease.Stats) ACDStats {
	return ACDStats{
		ProbesSent:        s.ProbesSent,
		AnnouncementsSent: s.AnnouncementsSent,
		ConflictsDetected: s.ConflictsDetected,
		ARPSendFailures:   s.ARPSendFailures,
	}
}

// Sub returns the counters gained since prev, saturating at zero, since the library's counters only rise (#882).
func (s ACDStats) Sub(prev ACDStats) ACDStats {
	return ACDStats{
		ProbesSent:        sub(s.ProbesSent, prev.ProbesSent),
		AnnouncementsSent: sub(s.AnnouncementsSent, prev.AnnouncementsSent),
		ConflictsDetected: sub(s.ConflictsDetected, prev.ConflictsDetected),
		ARPSendFailures:   sub(s.ARPSendFailures, prev.ARPSendFailures),
	}
}

// IsZero reports whether nothing moved.
func (s ACDStats) IsZero() bool { return s == ACDStats{} }

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

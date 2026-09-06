//go:build !linux

package runtime

import "net/netip"

// DADStats is what a DADProbe has done and seen.
//
// The field list must match the Linux declaration, and not because the build
// says so — the fields' only readers are Linux-only test files. What holds it
// is TestDADStatsDeclarationsAgree, which parses both files.
type DADStats struct {
	Present           bool
	Started           uint64
	Solicits          uint64
	Free              uint64
	Duplicate         uint64
	SendFailures      uint64
	OwnIgnored        uint64
	ForeignSolicits   uint64
	ResolvingSolicits uint64
	Adverts           uint64
	Restarted         uint64
	JoinFailures      uint64
}

// DADProbe is not available on this platform.
//
// Start REPORTS A DUPLICATE rather than doing nothing, and that is the whole
// content of this file. RFC 4862 section 5.4 makes the check mandatory before
// an address may be used, so the two honest answers off Linux are "refuse to
// build" — which NewDADProbe does — and, for a probe that somehow exists,
// "this address may not be used". Reporting free would make a client announce
// an address nothing checked, which is the failure the whole runner exists to
// prevent, and leaving the callback uncalled would hang ring 1 until its
// deadline and blame the silence on the network.
type DADProbe struct{}

// NewDADProbe always fails off Linux, with ErrUnsupportedPlatform.
func NewDADProbe(*NDSocket) (*DADProbe, error) { return nil, ErrUnsupportedPlatform }

// Start reports every address as a duplicate. See DADProbe.
func (*DADProbe) Start(addr netip.Addr, report func(netip.Addr, bool)) {
	if report != nil {
		report(addr, true)
	}
}

func (*DADProbe) Close() error { return nil }

func (*DADProbe) Stats() DADStats { return DADStats{} }

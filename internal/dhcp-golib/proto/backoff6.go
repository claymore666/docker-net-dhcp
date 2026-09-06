package proto

import (
	"math/bits"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// The DHCPv6 retransmission machinery, RFC 9915 §7.6 and §15.
//
// It sits beside backoff.go, which is the same thing for RFC 2131 §4.1, and the
// two are separate for the reason §15 and §4.1 are separate documents: v4
// doubles a delay and clamps it, v6 carries five named variables per message
// type and takes its randomisation from a different distribution. D30's rule —
// IPv6 takes the same SHAPE as IPv4 — is satisfied by both being a value
// computed from (rnd, previous, table) with no clock in sight, not by folding
// them into one struct whose fields half apply.
//
// THE TABLE IS A VALUE, NOT A CONSTANT BLOCK, and that is forced by the
// protocol rather than chosen: §21.24 and §21.25 let the SERVER replace
// SOL_MAX_RT and INF_MAX_RT at run time, so a client holding them as constants
// cannot be conformant. Build plan §10.2 asks for this from the start.

// Params6 is RFC 9915 §7.6's Table 1, "Transmission and Retransmission
// Parameters", as a value.
//
// Two of the twenty-five are absent. REC_TIMEOUT and REC_MAX_RC belong to
// Reconfigure, which is v2.2's (D25) and which this client refuses at the codec
// (wire.ErrV6NotForClient); HOP_COUNT_LIMIT belongs to a relay agent. Naming
// them here would be an enumeration of parameters nothing reads.
type Params6 struct {
	// Solicit, §18.2.1.
	SolMaxDelay Duration
	SolTimeout  Duration
	SolMaxRT    Duration

	// Request, §18.2.2.
	ReqTimeout Duration
	ReqMaxRT   Duration
	ReqMaxRC   int

	// Confirm, §18.2.3. CnfMaxRD is a DURATION budget where the others are
	// counts: §15's MRD, "an upper bound on the length of time a client may
	// retransmit a message".
	CnfMaxDelay Duration
	CnfTimeout  Duration
	CnfMaxRT    Duration
	CnfMaxRD    Duration

	// Renew at T1, §18.2.4, and Rebind at T2, §18.2.5. Neither has an MRC or
	// an MRD in §7.6: the lease's own expiry is what ends them.
	RenTimeout Duration
	RenMaxRT   Duration
	RebTimeout Duration
	RebMaxRT   Duration

	// Information-request, §18.2.6.
	InfMaxDelay Duration
	InfTimeout  Duration
	InfMaxRT    Duration

	// Release, §18.2.7, and Decline, §18.2.8. Both are bounded by a retry
	// count and not by a maximum timeout.
	RelTimeout Duration
	RelMaxRC   int
	DecTimeout Duration
	DecMaxRC   int

	// IRTDefault and IRTMinimum bound the Information Refresh Time option
	// (§21.23), which is not a retransmission parameter but is in the same
	// table.
	IRTDefault Duration
	IRTMinimum Duration

	// MaxWaitTime is §15's MAX_WAIT_TIME: "A client is not expected to listen
	// for a response during the entire RT period and may turn off listening
	// capabilities after waiting at least the shorter of RT and
	// MAX_WAIT_TIME".
	MaxWaitTime Duration

	// ------------------------------------------------ this client's own --
	//
	// The fields below are NOT in §7.6's table. They are the client's
	// identity, the caller's preferences, and the two schedules RFC 4861 and
	// RFC 4862 own rather than RFC 9915. They sit in the same struct because
	// one Machine6 takes one configuration value: a second struct beside this
	// one would make "which of the two holds SOL_MAX_RT" a question every
	// caller has to answer, and the server can rewrite SolMaxRT at run time
	// while nothing can rewrite DUID, so the two halves are already
	// distinguished by who may change them. params6.go holds their
	// validation, their defaults and the derivation of DADTimeout.

	// DUID is §21.2's client DUID, as the bytes to send. Build it with
	// wire.DUIDLL or wire.DUIDUUID; the machine never invents one, because
	// §11 says a DUID "SHOULD NOT change over time if at all possible" and a
	// value the library generated per process is one that changes on every
	// restart.
	DUID []byte

	// IAID is §21.4's "IAID: The unique identifier for this IA_NA; the
	// IAID must be unique among the identifiers for all of this client's
	// IA_NAs." Zero is a legal value and is not a sentinel here.
	IAID uint32

	// Hint is the address the caller would like, sent as an IA Address
	// option inside the Solicit's IA_NA. §18.2.1: "The client MAY include
	// addresses in IA Address options (see Section 21.6) encapsulated within
	// IA_NA option as hints to the server about the addresses for which the
	// client has a preference." The zero value sends no hint, and a server
	// is free to ignore one that is sent.
	Hint netip.Addr

	// ORO is the option codes the caller wants beyond the ones the machine
	// requests on its own. §21.24 and §21.25 make 82 and 83 mandatory on the
	// Solicit and the Information-request respectively, and §21.23 makes 32
	// mandatory on the Information-request; those are added by the machine,
	// so a caller that leaves this nil still sends a conformant ORO.
	ORO []wire.OptionCodeV6

	// Resume is the binding remembered from a previous run, or nil.
	//
	// It maps to Confirm the way Params.Resume maps to INIT-REBOOT in v4
	// (D30): §18.2.12 says "When the client detects that it may have moved to
	// a new link and it has obtained addresses and no delegated prefixes from
	// a server, the client SHOULD initiate a Confirm/Reply message exchange."
	// A client that has just been restarted by its chassis is exactly the
	// first bullet of that section's list, "The client reboots (and has stable
	// storage and persistent DHCP state)".
	Resume *Resume6

	// DADTimeout is how long the machine waits for the EvDADResult that ring
	// 3 owes it. DefaultDADTimeout says where the value comes from.
	DADTimeout Duration

	// RouterSolicitations and RouterSolicitInterval are RFC 4861 §6.3.7's
	// MAX_RTR_SOLICITATIONS and RTR_SOLICITATION_INTERVAL, both listed in
	// §10 under "Host constants".
	RouterSolicitations   int
	RouterSolicitInterval Duration

	// MaxSendFailures is how many consecutive ActSendV6 failures end the
	// acquisition with ReasonTransport. It is Params.MaxSendFailures's
	// counterpart and exists for R2's reason: a machine whose every send
	// fails otherwise sits in SELECTING forever looking healthy.
	MaxSendFailures int
}

// DefaultParams6 is RFC 9915 §7.6's Table 1, unmodified, plus the defaults for
// the fields §7.6 does not carry: the two RFC 4861 §10 host constants, the
// DAD deadline params6.go derives, and this library's send-failure budget. It
// leaves DUID, IAID, Hint, ORO and Resume at their zero values, because those
// are the caller's and there is no defensible default for an identity.
func DefaultParams6() Params6 {
	return Params6{
		SolMaxDelay: 1 * Second,
		SolTimeout:  1 * Second,
		SolMaxRT:    3600 * Second,

		ReqTimeout: 1 * Second,
		ReqMaxRT:   30 * Second,
		ReqMaxRC:   10,

		CnfMaxDelay: 1 * Second,
		CnfTimeout:  1 * Second,
		CnfMaxRT:    4 * Second,
		CnfMaxRD:    10 * Second,

		RenTimeout: 10 * Second,
		RenMaxRT:   600 * Second,
		RebTimeout: 10 * Second,
		RebMaxRT:   600 * Second,

		InfMaxDelay: 1 * Second,
		InfTimeout:  1 * Second,
		InfMaxRT:    3600 * Second,

		RelTimeout: 1 * Second,
		RelMaxRC:   4,
		DecTimeout: 1 * Second,
		DecMaxRC:   4,

		IRTDefault: 86400 * Second,
		IRTMinimum: 600 * Second,

		MaxWaitTime: 60 * Second,

		DADTimeout:            DefaultDADTimeout,
		RouterSolicitations:   MaxRtrSolicitations,
		RouterSolicitInterval: RtrSolicitationInterval,
		MaxSendFailures:       DefaultMaxSendFailures6,
	}
}

// The range §21.24 and §21.25 both impose on a server-supplied value, in
// seconds: "MUST be in this range: 60 <= 'value' <= 86400 (1 day)".
const (
	MaxRTOptionMinSeconds uint32 = 60
	MaxRTOptionMaxSeconds uint32 = 86400
)

// ApplySolMaxRT applies a SOL_MAX_RT option value, §21.24, and reports whether
// it was taken.
//
// §21.24: "A DHCP client MUST ignore any SOL_MAX_RT option values that are less
// than 60 or more than 86400." IGNORED means the previous value stands, not
// that the value is clamped into range: a server that sends 10 is not asking
// for 60, it is sending something this client is told to disregard, and
// clamping would have the client behave as though a valid option had arrived.
//
// The boolean exists so that "the option was there and was ignored" is
// observable. It is the only evidence that a server is configured out of range,
// and without it the symptom is a retransmission schedule nobody can explain.
func (p *Params6) ApplySolMaxRT(secs uint32) bool {
	if !maxRTInRange(secs) {
		return false
	}
	p.SolMaxRT = Duration(secs) * Second
	return true
}

// ApplyInfMaxRT applies an INF_MAX_RT option value, §21.25, under the same
// range rule and for the same reason.
func (p *Params6) ApplyInfMaxRT(secs uint32) bool {
	if !maxRTInRange(secs) {
		return false
	}
	p.InfMaxRT = Duration(secs) * Second
	return true
}

func maxRTInRange(secs uint32) bool {
	return secs >= MaxRTOptionMinSeconds && secs <= MaxRTOptionMaxSeconds
}

// ApplyOptions applies every SOL_MAX_RT and INF_MAX_RT option in a received
// message's top-level options and reports which codes were taken and which were
// present and ignored.
//
// A malformed option — one whose length is not four octets — is neither taken
// nor reported as ignored-for-range; the error says which one, because a
// four-octet field arriving with three octets is a different fault from a value
// out of range and the operator's next step differs.
func (p *Params6) ApplyOptions(o wire.OptionsV6) (applied, ignored []wire.OptionCodeV6, err error) {
	for _, c := range []wire.OptionCodeV6{wire.OptV6SolMaxRTCode, wire.OptV6InfMaxRTCode} {
		v, ok, verr := o.Uint32V6(c)
		if verr != nil {
			return applied, ignored, verr
		}
		if !ok {
			continue
		}
		var took bool
		if c == wire.OptV6SolMaxRTCode {
			took = p.ApplySolMaxRT(v)
		} else {
			took = p.ApplyInfMaxRT(v)
		}
		if took {
			applied = append(applied, c)
		} else {
			ignored = append(ignored, c)
		}
	}
	return applied, ignored, nil
}

// Retransmit is one message exchange's retransmission schedule: RFC 9915 §15's
// IRT, MRT, MRC and MRD, drawn from Params6 by the message type.
//
// §15 names five variables. RAND is not a field because it is not a
// per-exchange parameter: "The algorithm for RAND is common across all message
// transmissions."
type Retransmit struct {
	// IRT is the initial retransmission time.
	IRT Duration
	// MRT is the maximum retransmission time. §15: "If MRT has a value of 0,
	// there is no upper limit on the value of RT."
	MRT Duration
	// MRC is the maximum retransmission count. §15: "Unless MRC is zero, the
	// message exchange fails once the client has transmitted the message MRC
	// times."
	MRC int
	// MRD is the maximum retransmission duration. §15: "Unless MRD is zero,
	// the message exchange fails once MRD seconds have elapsed since the
	// client first transmitted the message."
	MRD Duration
}

// The schedules of §18.2, each built from the §7.6 table.
//
// Solicit's MRC and MRD are both zero, which §15 makes explicit: "If both MRC
// and MRD are zero, the client continues to transmit the message until it
// receives a response." With SOL_MAX_RT at an hour that is a client which never
// gives up on its own — the CALLER bounds it, the way 1.9.0's lease_timeout
// did, and this table is the reason that is a design decision rather than an
// omission.
func (p Params6) Solicit() Retransmit {
	return Retransmit{IRT: p.SolTimeout, MRT: p.SolMaxRT}
}

// Request is §18.2.2's schedule.
func (p Params6) Request() Retransmit {
	return Retransmit{IRT: p.ReqTimeout, MRT: p.ReqMaxRT, MRC: p.ReqMaxRC}
}

// Confirm is §18.2.3's schedule, the one bounded by a duration rather than a
// count.
func (p Params6) Confirm() Retransmit {
	return Retransmit{IRT: p.CnfTimeout, MRT: p.CnfMaxRT, MRD: p.CnfMaxRD}
}

// Renew is §18.2.4's schedule.
func (p Params6) Renew() Retransmit {
	return Retransmit{IRT: p.RenTimeout, MRT: p.RenMaxRT}
}

// Rebind is §18.2.5's schedule.
func (p Params6) Rebind() Retransmit {
	return Retransmit{IRT: p.RebTimeout, MRT: p.RebMaxRT}
}

// InfoRequest is §18.2.6's schedule.
func (p Params6) InfoRequest() Retransmit {
	return Retransmit{IRT: p.InfTimeout, MRT: p.InfMaxRT}
}

// Release is §18.2.7's schedule.
func (p Params6) Release() Retransmit {
	return Retransmit{IRT: p.RelTimeout, MRC: p.RelMaxRC}
}

// Decline is §18.2.8's schedule.
func (p Params6) Decline() Retransmit {
	return Retransmit{IRT: p.DecTimeout, MRC: p.DecMaxRC}
}

// randScale and randDenom express §15's RAND as an exact rational.
//
// §15: "Each of the computations of a new RT includes a randomization factor
// (RAND), which is a random number chosen with a uniform distribution between
// -0.1 and +0.1." RAND is n/randDenom for an n drawn uniformly from
// [-randScale, +randScale], which is 0.1 exactly at the ends and needs no
// floating point — ring 1 computes deadlines that a replay has to reproduce bit
// for bit, and a float would make that a property of the FPU.
const (
	randScale int64 = 1_000_000
	randDenom int64 = 10_000_000
)

// randFactor draws §15's RAND from one entropy value.
//
// BOUND, the same one backoff.go's jitter records: the modulo biases the draw
// by about one part in 2^64/2000001, which is order 10^-13. A rejection loop
// would consume an unbounded number of entropy values per Step and break the
// one-rnd-per-Step contract the journal replays against.
func randFactor(rnd uint64) int64 {
	span := uint64(2*randScale + 1)
	return int64(rnd%span) - randScale
}

// First is §15's "RT for the first message transmission is based on IRT: RT =
// IRT + RAND*IRT".
//
// The MRT cap is NOT applied here. §15 introduces MRT only after both formulas,
// and applying it to the first transmission would silently change Solicit's
// behaviour for any deployment that set SOL_MAX_RT below SOL_TIMEOUT.
func (r Retransmit) First(rnd uint64) Duration {
	return nonNegative(r.IRT + scaleRand(r.IRT, randFactor(rnd)))
}

// Next is §15's "RT for each subsequent message transmission is based on the
// previous value of RT: RT = 2*RTprev + RAND*RTprev", followed by the cap.
//
// THE CAP COMES AFTER THE RANDOMISATION AND REPLACES IT, and the order is the
// whole content of this function. §15, verbatim: "MRT specifies an upper bound
// on the value of RT (disregarding the randomization added by the use of RAND).
// If MRT has a value of 0, there is no upper limit on the value of RT.
// Otherwise: if (RT > MRT) RT = MRT + RAND*MRT". So a capped RT is NOT MRT and
// is not bounded by MRT: it is MRT spread over ±10%, which is what stops a
// fleet of clients that all reached the ceiling from retransmitting in lockstep
// — the reason §15 gives for RAND in the first place ("to minimize
// synchronization of messages transmitted by DHCP clients"). A cap applied
// before the randomisation, or one that clamped to MRT exactly, produces that
// lockstep and no test on a single client can see it.
func (r Retransmit) Next(prev Duration, rnd uint64) Duration {
	n := randFactor(rnd)
	rt := 2*prev + scaleRand(prev, n)
	if r.MRT > 0 && rt > r.MRT {
		rt = r.MRT + scaleRand(r.MRT, n)
	}
	return nonNegative(rt)
}

// Exhausted reports §15's failure condition for an exchange that has
// transmitted the message count times and has been running for elapsed.
//
// §15: "MRC specifies an upper bound on the number of times a client may
// retransmit a message. Unless MRC is zero, the message exchange fails once the
// client has transmitted the message MRC times. MRD specifies an upper bound on
// the length of time a client may retransmit a message. Unless MRD is zero, the
// message exchange fails once MRD seconds have elapsed since the client first
// transmitted the message. If both MRC and MRD are non-zero, the message
// exchange fails whenever either of the conditions specified in the previous
// two paragraphs is met."
//
// count is TRANSMISSIONS, the first one included, because that is what §15
// counts.
func (r Retransmit) Exhausted(count int, elapsed Duration) bool {
	if r.MRC > 0 && count >= r.MRC {
		return true
	}
	if r.MRD > 0 && elapsed >= r.MRD {
		return true
	}
	return false
}

// scaleRand returns d * n / randDenom exactly, for an n from randFactor.
//
// It goes through a 128-bit intermediate because the product overflows int64
// for the values the protocol actually permits: a server may set SOL_MAX_RT to
// 86400 seconds (§21.24), which is 8.64e13 nanoseconds, and 8.64e13 * 1e6 is
// 8.64e19 — past both int64 and uint64. The visible symptom of the overflow
// would be a NEGATIVE retransmission delay, i.e. a timer that fires at once and
// a client that retransmits in a tight loop against a server that has already
// asked it to slow down.
func scaleRand(d Duration, n int64) Duration {
	if d == 0 || n == 0 {
		return 0
	}
	neg := (d < 0) != (n < 0)
	ad, an := abs64(int64(d)), abs64(n)
	hi, lo := bits.Mul64(ad, an)
	// hi is at most (2^63 * 2^20) >> 64, far below randDenom, so Div64 cannot
	// overflow. The guard is here anyway: Div64 PANICS on overflow, and a
	// panic in ring 1 takes the plugin's network driver down with it.
	//
	// UNREACHABLE, so it is written to be harmless rather than clever: |RAND|
	// is at most 0.1 by construction, so a tenth of the magnitude is the
	// largest answer this function can honestly give, and returning d itself
	// would be a tenfold error at exactly the moment nobody is watching.
	if hi >= uint64(randDenom) {
		q := Duration(ad / 10)
		if neg {
			return -q
		}
		return q
	}
	q, _ := bits.Div64(hi, lo, uint64(randDenom))
	if neg {
		return -Duration(q)
	}
	return Duration(q)
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

// nonNegative keeps a computed RT from being a timer that fires immediately.
func nonNegative(d Duration) Duration {
	if d < 0 {
		return 0
	}
	return d
}

package proto

import (
	"math"
	"reflect"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// The three entropy values that make RAND exact. randFactor maps rnd into
// [-randScale, +randScale] by rnd % (2*randScale+1), so these are RAND = -0.1,
// RAND = 0 and RAND = +0.1 with nothing rounded.
const (
	randMinus = uint64(0)
	randZero  = uint64(randScale)
	randPlus  = uint64(2 * randScale)
)

// TestRandFactorSpansExactlyPlusOrMinusATenth pins the distribution §15 names:
// "a random number chosen with a uniform distribution between -0.1 and +0.1".
// The ends are inclusive and exact, and nothing outside them is reachable.
func TestRandFactorSpansExactlyPlusOrMinusATenth(t *testing.T) {
	if got := randFactor(randMinus); got != -randScale {
		t.Errorf("randFactor(%d) = %d, want %d (RAND = -0.1)", randMinus, got, -randScale)
	}
	if got := randFactor(randZero); got != 0 {
		t.Errorf("randFactor(%d) = %d, want 0", randZero, got)
	}
	if got := randFactor(randPlus); got != randScale {
		t.Errorf("randFactor(%d) = %d, want %d (RAND = +0.1)", randPlus, got, randScale)
	}
	if randScale*10 != randDenom {
		t.Fatalf("randScale/randDenom = %d/%d, which is not a tenth", randScale, randDenom)
	}
	// Sweep the whole span plus a long way past it: nothing may escape.
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	seenMin, seenMax := false, false
	for r := uint64(0); r < uint64(4*randScale+7); r++ {
		n := randFactor(r)
		if n < -randScale || n > randScale {
			t.Fatalf("randFactor(%d) = %d, outside [-%d, %d]", r, n, randScale, randScale)
		}
		if n < lo {
			lo = n
		}
		if n > hi {
			hi = n
		}
		seenMin = seenMin || n == -randScale
		seenMax = seenMax || n == randScale
	}
	if !seenMin || !seenMax {
		t.Errorf("the sweep reached [%d, %d] but never both ends; the distribution is not the closed interval §15 names", lo, hi)
	}
	if randFactor(math.MaxUint64) < -randScale || randFactor(math.MaxUint64) > randScale {
		t.Error("the largest entropy value escaped the interval")
	}
}

// TestSection76TableIsTable1 is C-1: one row per parameter, each quoting the
// line of RFC 9915 §7.6's Table 1 it comes from. Nothing in this round runs a
// retransmission, so a mistyped default is silent until a deployment sees it.
//
// The quotations are the table's own "Parameter / Value / Description" rows,
// abbreviated to the parameter and its value.
func TestSection76TableIsTable1(t *testing.T) {
	p := DefaultParams6()
	for _, tc := range []struct {
		table string
		got   Duration
		want  Duration
	}{
		{"SOL_MAX_DELAY  1 sec   Max delay of first Solicit", p.SolMaxDelay, 1 * Second},
		{"SOL_TIMEOUT    1 sec   Initial Solicit timeout", p.SolTimeout, 1 * Second},
		{"SOL_MAX_RT     3600 secs  Max Solicit timeout value", p.SolMaxRT, 3600 * Second},
		{"REQ_TIMEOUT    1 sec   Initial Request timeout", p.ReqTimeout, 1 * Second},
		{"REQ_MAX_RT     30 secs  Max Request timeout value", p.ReqMaxRT, 30 * Second},
		{"CNF_MAX_DELAY  1 sec   Max delay of first Confirm", p.CnfMaxDelay, 1 * Second},
		{"CNF_TIMEOUT    1 sec   Initial Confirm timeout", p.CnfTimeout, 1 * Second},
		{"CNF_MAX_RT     4 secs  Max Confirm timeout", p.CnfMaxRT, 4 * Second},
		{"CNF_MAX_RD     10 secs  Max Confirm duration", p.CnfMaxRD, 10 * Second},
		{"REN_TIMEOUT    10 secs  Initial Renew timeout", p.RenTimeout, 10 * Second},
		{"REN_MAX_RT     600 secs  Max Renew timeout value", p.RenMaxRT, 600 * Second},
		{"REB_TIMEOUT    10 secs  Initial Rebind timeout", p.RebTimeout, 10 * Second},
		{"REB_MAX_RT     600 secs  Max Rebind timeout value", p.RebMaxRT, 600 * Second},
		{"INF_MAX_DELAY  1 sec   Max delay of first Information-request", p.InfMaxDelay, 1 * Second},
		{"INF_TIMEOUT    1 sec   Initial Information-request timeout", p.InfTimeout, 1 * Second},
		{"INF_MAX_RT     3600 secs  Max Information-request timeout value", p.InfMaxRT, 3600 * Second},
		{"REL_TIMEOUT    1 sec   Initial Release timeout", p.RelTimeout, 1 * Second},
		{"DEC_TIMEOUT    1 sec   Initial Decline timeout", p.DecTimeout, 1 * Second},
		{"IRT_DEFAULT    86400 secs  Default information refresh time", p.IRTDefault, 86400 * Second},
		{"IRT_MINIMUM    600 secs  Min information refresh time", p.IRTMinimum, 600 * Second},
		{"MAX_WAIT_TIME  60 secs  Max required time to wait for a response", p.MaxWaitTime, 60 * Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: have %s, want %s", tc.table, tc.got, tc.want)
		}
	}
	for _, tc := range []struct {
		table string
		got   int
		want  int
	}{
		{"REQ_MAX_RC     10  Max Request retry attempts", p.ReqMaxRC, 10},
		{"REL_MAX_RC     4   MAX Release attempts", p.RelMaxRC, 4},
		{"DEC_MAX_RC     4   Max Decline attempts", p.DecMaxRC, 4},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: have %d, want %d", tc.table, tc.got, tc.want)
		}
	}
}

// TestScheduleDrawsFromTheRightTableEntry closes the other half of C-1: a value
// can be right in Params6 and read into the wrong schedule. Every §18.2 exchange
// is checked against the entries §7.6 names for it, and the zero fields matter
// as much as the set ones — Solicit with a non-zero MRC is a client that gives
// up, and Renew with an MRD is a client that stops renewing early.
func TestScheduleDrawsFromTheRightTableEntry(t *testing.T) {
	// Every field distinct, so a schedule reading the wrong one is visible.
	p := Params6{
		SolTimeout: 1 * Second, SolMaxRT: 2 * Second,
		ReqTimeout: 3 * Second, ReqMaxRT: 4 * Second, ReqMaxRC: 5,
		CnfTimeout: 6 * Second, CnfMaxRT: 7 * Second, CnfMaxRD: 8 * Second,
		RenTimeout: 9 * Second, RenMaxRT: 10 * Second,
		RebTimeout: 11 * Second, RebMaxRT: 12 * Second,
		InfTimeout: 13 * Second, InfMaxRT: 14 * Second,
		RelTimeout: 15 * Second, RelMaxRC: 16,
		DecTimeout: 17 * Second, DecMaxRC: 18,
	}
	for _, tc := range []struct {
		name string
		got  Retransmit
		want Retransmit
	}{
		{"Solicit §18.2.1", p.Solicit(), Retransmit{IRT: 1 * Second, MRT: 2 * Second}},
		{"Request §18.2.2", p.Request(), Retransmit{IRT: 3 * Second, MRT: 4 * Second, MRC: 5}},
		{"Confirm §18.2.3", p.Confirm(), Retransmit{IRT: 6 * Second, MRT: 7 * Second, MRD: 8 * Second}},
		{"Renew §18.2.4", p.Renew(), Retransmit{IRT: 9 * Second, MRT: 10 * Second}},
		{"Rebind §18.2.5", p.Rebind(), Retransmit{IRT: 11 * Second, MRT: 12 * Second}},
		{"Information-request §18.2.6", p.InfoRequest(), Retransmit{IRT: 13 * Second, MRT: 14 * Second}},
		{"Release §18.2.7", p.Release(), Retransmit{IRT: 15 * Second, MRC: 16}},
		{"Decline §18.2.8", p.Decline(), Retransmit{IRT: 17 * Second, MRC: 18}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
	// §15, on Solicit: "If both MRC and MRD are zero, the client continues to
	// transmit the message until it receives a response."
	s := DefaultParams6().Solicit()
	if s.MRC != 0 || s.MRD != 0 {
		t.Errorf("Solicit has MRC=%d MRD=%s; §7.6 leaves both unset and §15 says such a client never stops on its own", s.MRC, s.MRD)
	}
	if r := DefaultParams6().Renew(); r.MRC != 0 || r.MRD != 0 {
		t.Errorf("Renew has MRC=%d MRD=%s; §7.6 gives it neither — the lease's own expiry ends it", r.MRC, r.MRD)
	}
}

// TestFirstIsIRTPlusRandTimesIRT is §15's first formula: "RT for the first
// message transmission is based on IRT: RT = IRT + RAND*IRT".
//
// The MRT cap is deliberately absent here. §15 introduces MRT after both
// formulas, and applying it to the first transmission changes Solicit for any
// deployment whose server set SOL_MAX_RT below SOL_TIMEOUT.
func TestFirstIsIRTPlusRandTimesIRT(t *testing.T) {
	r := Retransmit{IRT: 10 * Second, MRT: 1 * Second}
	for _, tc := range []struct {
		name string
		rnd  uint64
		want Duration
	}{
		{"RAND = -0.1", randMinus, 9 * Second},
		{"RAND = 0", randZero, 10 * Second},
		{"RAND = +0.1", randPlus, 11 * Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.First(tc.rnd); got != tc.want {
				t.Errorf("First = %s, want %s", got, tc.want)
			}
		})
	}
	if got := r.First(randPlus); got <= r.MRT {
		t.Errorf("First = %s with MRT %s: the cap was applied to the FIRST transmission, which §15 introduces only after both formulas",
			got, r.MRT)
	}
	if got := (Retransmit{IRT: 0}).First(randPlus); got != 0 {
		t.Errorf("First with IRT 0 = %s, want 0", got)
	}
}

// TestNextCapsAfterTheRandomisationNotBefore is C-2, and the order is the whole
// content of the row.
//
// §15, verbatim: "MRT specifies an upper bound on the value of RT
// (disregarding the randomization added by the use of RAND). If MRT has a value
// of 0, there is no upper limit on the value of RT. Otherwise: if (RT > MRT) RT
// = MRT + RAND*MRT".
//
// A capped RT is therefore NOT MRT: it is MRT spread over ±10%, and it may
// EXCEED MRT. An implementation that clamped to MRT exactly would pass every
// coarse assertion and would put a fleet of clients that all reached the
// ceiling into lockstep — which is the reason §15 gives for RAND existing at
// all: "to minimize synchronization of messages transmitted by DHCP clients".
func TestNextCapsAfterTheRandomisationNotBefore(t *testing.T) {
	r := Retransmit{IRT: 1 * Second, MRT: 100 * Second}

	// Below the cap: 2*prev + RAND*prev, untouched.
	for _, tc := range []struct {
		name string
		prev Duration
		rnd  uint64
		want Duration
	}{
		{"RAND = -0.1", 10 * Second, randMinus, 19 * Second},
		{"RAND = 0", 10 * Second, randZero, 20 * Second},
		{"RAND = +0.1", 10 * Second, randPlus, 21 * Second},
	} {
		t.Run("below the cap, "+tc.name, func(t *testing.T) {
			if got := r.Next(tc.prev, tc.rnd); got != tc.want {
				t.Errorf("Next(%s) = %s, want %s", tc.prev, got, tc.want)
			}
		})
	}

	// THE COMPARISON IS AGAINST THE RANDOMISED RT, not against 2*prev. §15's
	// order is "RT = 2*RTprev + RAND*RTprev" and then "if (RT > MRT)", so at
	// prev = 50s the same previous value falls on either side of the cap
	// depending only on the draw — 95s and 100s pass through untouched, 105s
	// is over the line and comes back as 110s.
	if got, want := r.Next(50*Second, randMinus), 95*Second; got != want {
		t.Errorf("prev 50s with RAND = -0.1: Next = %s, want %s (95s is not over MRT)", got, want)
	}
	if got, want := r.Next(50*Second, randZero), 100*Second; got != want {
		t.Errorf("prev 50s with RAND = 0: Next = %s, want %s — §15 caps only when RT > MRT, and 100s is not", got, want)
	}
	if got, want := r.Next(50*Second, randPlus), 110*Second; got != want {
		t.Errorf("prev 50s with RAND = +0.1: Next = %s, want %s — 105s IS over MRT, so the cap fires and replaces it with MRT + RAND*MRT",
			got, want)
	}

	// Past the boundary on every draw, the result is MRT ± 10% of MRT. At
	// prev = 60s even RAND = -0.1 lands over the line (114s), so all three
	// rows exercise the cap itself.
	past := 60 * Second
	for _, tc := range []struct {
		name string
		rnd  uint64
		want Duration
	}{
		{"RAND = -0.1", randMinus, 90 * Second},
		{"RAND = 0", randZero, 100 * Second},
		{"RAND = +0.1", randPlus, 110 * Second},
	} {
		t.Run("capped, "+tc.name, func(t *testing.T) {
			if got := r.Next(past, tc.rnd); got != tc.want {
				t.Errorf("Next(%s) = %s, want %s", past, got, tc.want)
			}
		})
	}
	if got := r.Next(past, randPlus); got <= r.MRT {
		t.Fatalf("a capped RT is %s, which does not EXCEED MRT %s: §15's capped value is MRT + RAND*MRT, "+
			"and a clamp to MRT puts every client that reached the ceiling into lockstep", got, r.MRT)
	}

	// Far past the cap the answer is the same: the cap REPLACES the value, it
	// does not bound the doubling.
	if got, want := r.Next(1000*Second, randZero), 100*Second; got != want {
		t.Errorf("far past the cap: Next = %s, want %s", got, want)
	}

	// §15: "If MRT has a value of 0, there is no upper limit on the value of
	// RT." Solicit runs with SOL_MAX_RT set, but Release and Decline have no
	// MRT at all.
	unbounded := Retransmit{IRT: 1 * Second}
	if got, want := unbounded.Next(1000*Second, randZero), 2000*Second; got != want {
		t.Errorf("with MRT 0: Next = %s, want %s (no upper limit)", got, want)
	}
}

// TestBackoffConvergesIntoTheCapBand drives the schedule the way a client would
// and checks the shape rather than one step: a Solicit that never hears an
// answer climbs, then stays inside [0.9*MRT, 1.1*MRT] for ever, and never
// returns a delay that would fire a timer immediately.
func TestBackoffConvergesIntoTheCapBand(t *testing.T) {
	p := DefaultParams6()
	r := p.Solicit()
	rt := r.First(randZero)
	if rt != p.SolTimeout {
		t.Fatalf("the first RT is %s, want SOL_TIMEOUT %s", rt, p.SolTimeout)
	}
	lo, hi := r.MRT-r.MRT/10, r.MRT+r.MRT/10
	capped := 0
	for i := 0; i < 200; i++ {
		rt = r.Next(rt, uint64(i)*7919)
		if rt <= 0 {
			t.Fatalf("step %d produced RT %s; a non-positive delay is a timer that fires at once", i, rt)
		}
		if rt > hi {
			t.Fatalf("step %d produced RT %s, above MRT + 10%% (%s)", i, rt, hi)
		}
		if rt >= lo {
			capped++
		}
	}
	if capped == 0 {
		t.Fatalf("200 steps from %s never reached the cap band around MRT %s; the loop measured nothing", p.SolTimeout, r.MRT)
	}
}

// TestScaleRandSurvivesTheLargestPermittedTimeout is C-3.
//
// §21.24 lets a server set SOL_MAX_RT to 86400 seconds. That is 8.64e13
// nanoseconds, and 8.64e13 * randScale is 8.64e19 — past int64 AND past
// uint64. The visible symptom of the overflow is a NEGATIVE delay: a timer that
// fires at once, so a client retransmits in a tight loop against the very
// server that asked it to slow down.
func TestScaleRandSurvivesTheLargestPermittedTimeout(t *testing.T) {
	max := Duration(MaxRTOptionMaxSeconds) * Second
	r := Retransmit{IRT: 1 * Second, MRT: max}
	for _, tc := range []struct {
		name string
		rnd  uint64
		want Duration
	}{
		{"RAND = -0.1", randMinus, max - max/10},
		{"RAND = 0", randZero, max},
		{"RAND = +0.1", randPlus, max + max/10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Next(max, tc.rnd)
			if got != tc.want {
				t.Fatalf("Next at the largest permitted MRT = %s (%d ns), want %s (%d ns)", got, got, tc.want, tc.want)
			}
			if got <= 0 {
				t.Fatalf("Next produced %s: the 64-bit product overflowed", got)
			}
		})
	}
	// scaleRand itself, at the value that overflows, and its sign symmetry.
	if got, want := scaleRand(max, randScale), max/10; got != want {
		t.Errorf("scaleRand(%s, +0.1) = %s, want %s", max, got, want)
	}
	if got, want := scaleRand(max, -randScale), -(max / 10); got != want {
		t.Errorf("scaleRand(%s, -0.1) = %s, want %s", max, got, want)
	}
	if got := scaleRand(0, randScale); got != 0 {
		t.Errorf("scaleRand(0, +0.1) = %s, want 0", got)
	}
	if got := scaleRand(max, 0); got != 0 {
		t.Errorf("scaleRand(d, 0) = %s, want 0", got)
	}
	// The doubling itself must not wrap for any RT the protocol permits.
	if got := r.Next(max*2, randPlus); got != max+max/10 {
		t.Errorf("Next from twice the cap = %s, want %s", got, max+max/10)
	}
	// nonNegative is the last line of defence, and it has to be reachable:
	// a negative previous RT cannot arise from the formulas, but a caller
	// driving Next directly is exactly the caller that would produce one.
	if got := r.Next(-1*Second, randZero); got != 0 {
		t.Errorf("Next from a negative previous RT = %s, want 0", got)
	}
	if got := (Retransmit{}).First(randMinus); got != 0 {
		t.Errorf("First with a zero schedule = %s, want 0", got)
	}
}

// TestExhaustedIsOffByNothing is C-4. §15 counts TRANSMISSIONS, the first one
// included: "the message exchange fails once the client has transmitted the
// message MRC times".
func TestExhaustedIsOffByNothing(t *testing.T) {
	byCount := Retransmit{MRC: 10}
	for _, tc := range []struct {
		count int
		want  bool
	}{{0, false}, {1, false}, {9, false}, {10, true}, {11, true}} {
		if got := byCount.Exhausted(tc.count, 0); got != tc.want {
			t.Errorf("MRC 10, %d transmission(s): Exhausted = %v, want %v", tc.count, got, tc.want)
		}
	}

	byTime := Retransmit{MRD: 10 * Second}
	for _, tc := range []struct {
		elapsed Duration
		want    bool
	}{
		{0, false},
		{10*Second - 1, false},
		{10 * Second, true},
		{10*Second + 1, true},
	} {
		if got := byTime.Exhausted(0, tc.elapsed); got != tc.want {
			t.Errorf("MRD 10s, elapsed %s: Exhausted = %v, want %v", tc.elapsed, got, tc.want)
		}
	}

	// §15: "Unless MRC is zero" and "Unless MRD is zero" — a zero is not a
	// bound of zero, it is the absence of the bound.
	none := Retransmit{}
	if none.Exhausted(1<<30, 1<<62) {
		t.Error("a schedule with neither MRC nor MRD gave up; §15 says such a client transmits until it gets an answer")
	}

	// "If both MRC and MRD are non-zero, the message exchange fails whenever
	// either of the conditions specified in the previous two paragraphs is
	// met." Either, not both.
	both := Retransmit{MRC: 10, MRD: 10 * Second}
	if !both.Exhausted(10, 0) {
		t.Error("the count bound alone did not end the exchange")
	}
	if !both.Exhausted(0, 10*Second) {
		t.Error("the duration bound alone did not end the exchange")
	}
	if both.Exhausted(9, 10*Second-1) {
		t.Error("the exchange ended with neither bound met")
	}
	// The default Confirm is the one exchange §7.6 bounds by time.
	if c := DefaultParams6().Confirm(); !c.Exhausted(1000, 10*Second) || c.Exhausted(1000, 10*Second-1) {
		t.Errorf("CNF_MAX_RD is %s; the boundary is not at 10 seconds", c.MRD)
	}
}

// TestMaxRTOptionsAreIgnoredOutOfRange is A-6, and the boundary is the row.
//
// §21.24 and §21.25 both say: "A DHCP client MUST ignore any SOL_MAX_RT option
// values that are less than 60 or more than 86400." IGNORED means the previous
// value stands. Clamping into range would have the client behave as though a
// valid option had arrived, and the operator who configured 10 would see a
// schedule of 60 that nothing in their configuration explains.
func TestMaxRTOptionsAreIgnoredOutOfRange(t *testing.T) {
	if MaxRTOptionMinSeconds != 60 || MaxRTOptionMaxSeconds != 86400 {
		t.Fatalf("the range is %d..%d, want 60..86400", MaxRTOptionMinSeconds, MaxRTOptionMaxSeconds)
	}
	for _, tc := range []struct {
		secs  uint32
		taken bool
	}{
		{0, false},
		{1, false},
		{59, false},
		{60, true},
		{61, true},
		{86399, true},
		{86400, true},
		{86401, false},
		{math.MaxUint32, false},
	} {
		p := DefaultParams6()
		before := p.SolMaxRT
		if got := p.ApplySolMaxRT(tc.secs); got != tc.taken {
			t.Errorf("ApplySolMaxRT(%d) = %v, want %v", tc.secs, got, tc.taken)
		}
		want := before
		if tc.taken {
			want = Duration(tc.secs) * Second
		}
		if p.SolMaxRT != want {
			t.Errorf("after ApplySolMaxRT(%d), SOL_MAX_RT is %s, want %s", tc.secs, p.SolMaxRT, want)
		}

		p = DefaultParams6()
		before = p.InfMaxRT
		if got := p.ApplyInfMaxRT(tc.secs); got != tc.taken {
			t.Errorf("ApplyInfMaxRT(%d) = %v, want %v", tc.secs, got, tc.taken)
		}
		want = before
		if tc.taken {
			want = Duration(tc.secs) * Second
		}
		if p.InfMaxRT != want {
			t.Errorf("after ApplyInfMaxRT(%d), INF_MAX_RT is %s, want %s", tc.secs, p.InfMaxRT, want)
		}
	}

	// Each option touches its own field and not the other's.
	p := DefaultParams6()
	inf := p.InfMaxRT
	p.ApplySolMaxRT(600)
	if p.InfMaxRT != inf {
		t.Errorf("SOL_MAX_RT changed INF_MAX_RT to %s", p.InfMaxRT)
	}
	sol := p.SolMaxRT
	p.ApplyInfMaxRT(700)
	if p.SolMaxRT != sol {
		t.Errorf("INF_MAX_RT changed SOL_MAX_RT to %s", p.SolMaxRT)
	}
	if p.Solicit().MRT != 600*Second || p.InfoRequest().MRT != 700*Second {
		t.Errorf("the schedules did not follow the applied values: %s and %s", p.Solicit().MRT, p.InfoRequest().MRT)
	}
}

// TestApplyOptionsReportsWhatItIgnored keeps the evidence: "the option was
// there and was ignored" is the only sign a server is configured out of range,
// and without it the symptom is a retransmission schedule nobody can explain.
func TestApplyOptionsReportsWhatItIgnored(t *testing.T) {
	u32 := func(v uint32) []byte {
		return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	}
	t.Run("both applied", func(t *testing.T) {
		p := DefaultParams6()
		applied, ignored, err := p.ApplyOptions(wire.OptionsV6{
			{Code: wire.OptV6SolMaxRTCode, Data: u32(120)},
			{Code: wire.OptV6InfMaxRTCode, Data: u32(240)},
		})
		if err != nil {
			t.Fatalf("ApplyOptions: %v", err)
		}
		if len(applied) != 2 || len(ignored) != 0 {
			t.Fatalf("applied %v ignored %v, want both applied", applied, ignored)
		}
		if p.SolMaxRT != 120*Second || p.InfMaxRT != 240*Second {
			t.Errorf("values %s and %s, want 120s and 240s", p.SolMaxRT, p.InfMaxRT)
		}
	})
	t.Run("one out of range", func(t *testing.T) {
		p := DefaultParams6()
		before := p.SolMaxRT
		applied, ignored, err := p.ApplyOptions(wire.OptionsV6{
			{Code: wire.OptV6SolMaxRTCode, Data: u32(59)},
			{Code: wire.OptV6InfMaxRTCode, Data: u32(240)},
		})
		if err != nil {
			t.Fatalf("ApplyOptions: %v", err)
		}
		if len(applied) != 1 || applied[0] != wire.OptV6InfMaxRTCode {
			t.Errorf("applied %v, want only INF_MAX_RT", applied)
		}
		if len(ignored) != 1 || ignored[0] != wire.OptV6SolMaxRTCode {
			t.Errorf("ignored %v, want SOL_MAX_RT", ignored)
		}
		if p.SolMaxRT != before {
			t.Errorf("SOL_MAX_RT became %s; an ignored option leaves the previous value standing, it does not clamp", p.SolMaxRT)
		}
	})
	t.Run("absent", func(t *testing.T) {
		p := DefaultParams6()
		applied, ignored, err := p.ApplyOptions(wire.OptionsV6{{Code: wire.OptV6ClientID, Data: []byte{1}}})
		if err != nil || len(applied) != 0 || len(ignored) != 0 {
			t.Errorf("ApplyOptions with neither option: %v %v %v", applied, ignored, err)
		}
		if !reflect.DeepEqual(p, DefaultParams6()) {
			t.Error("a message with neither option changed the parameters")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		p := DefaultParams6()
		_, _, err := p.ApplyOptions(wire.OptionsV6{{Code: wire.OptV6SolMaxRTCode, Data: []byte{0, 0, 60}}})
		if err == nil {
			t.Fatal("a 3-octet SOL_MAX_RT was accepted; a short option is a different fault from a value out of range")
		}
		if !reflect.DeepEqual(p, DefaultParams6()) {
			t.Error("a malformed option changed the parameters")
		}
	})
	t.Run("nil options", func(t *testing.T) {
		p := DefaultParams6()
		if applied, ignored, err := p.ApplyOptions(nil); err != nil || applied != nil || ignored != nil {
			t.Errorf("ApplyOptions(nil) = %v %v %v", applied, ignored, err)
		}
	})
}

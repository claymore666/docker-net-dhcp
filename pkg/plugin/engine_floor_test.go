// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	dTypes "github.com/docker/docker/api/types"
)

// THE VERSION STRINGS BELOW ARE NOT INVENTED. Every spelling here is one
// a daemon actually reports somewhere: the plain upstream releases, the
// Debian and Ubuntu packages (`+dfsg1`, `+azure`), the pre-2017 naming
// that is still in long-lived distributions (`-ce`), Rancher Desktop
// (`-rd`), and an upstream release candidate. The floor is compared
// against whatever `docker version` prints on the operator's host, and
// the failure this table exists to prevent is a plugin that refuses to
// start on a host it works on because nobody anticipated the suffix.
//
// THE DIRECTION OF THE UNREADABLE CASE IS THE POINT. A string this
// cannot parse yields ok=false and NEVER below=true: the caller says so
// out loud and starts anyway. The reason for that direction is the cost
// comparison written above engineBelowFloor, not a claim about what
// engines below the floor do; nothing here measured one.
func TestEngineBelowFloor_VersionSpellings(t *testing.T) {
	const floor = "20.10"

	cases := []struct {
		name    string
		version string
		below   bool
		ok      bool
	}{
		{"the floor itself", "20.10.24", false, true},
		{"a patch of the floor line", "20.10.5", false, true},
		{"the line below the floor", "19.03.15", true, true},
		{"two lines below", "18.09.9", true, true},
		{"the current release", "29.8.0", false, true},
		{"a major above with a lower minor", "23.0.6", false, true},

		// 20.10 is above 20.9 and below 23.0. A float comparison makes
		// 20.10 LESS than 20.9, and a string comparison makes "9"
		// greater than "20"; both orderings are wrong in a way that
		// changes a verdict.
		{"a minor that is not a decimal fraction", "20.9.1", true, true},
		{"a two-digit minor sorts above a one-digit one", "20.10.0", false, true},

		{"a Debian package suffix", "20.10.5+dfsg1", false, true},
		{"a Debian package suffix below the floor", "19.03.8+dfsg1", true, true},
		{"an Azure build", "24.0.9+azure.1", false, true},
		{"the pre-2017 community spelling", "20.10.7-ce", false, true},
		{"the pre-2017 community spelling below the floor", "19.03.8-ce", true, true},
		{"a Rancher Desktop build", "24.0.7-rd", false, true},
		{"an upstream release candidate", "29.0.0-rc.1", false, true},
		{"a version with no patch at all", "23.0", false, true},

		// No evidence, so no verdict. Each of these has been seen in a
		// field that was supposed to hold a version: an empty answer
		// from a daemon that replied but said nothing, the word this
		// plugin itself writes when it never found out, and the text of
		// a runtime error that was captured into a version field.
		{"an empty version", "", false, false},
		{"the word unknown", unknownEngineField, false, false},
		{"a captured error message", "OCI runtime exec failed: exec failed", false, false},
		{"a version with a leading v", "v25.0.3", false, false},
		{"a major with no minor", "25", false, false},
		{"a negative major", "-1.0", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			below, ok := engineBelowFloor(tc.version, floor)
			if ok != tc.ok {
				t.Fatalf("engineBelowFloor(%q, %q) ok = %v, want %v", tc.version, floor, ok, tc.ok)
			}
			if below != tc.below {
				t.Errorf("engineBelowFloor(%q, %q) below = %v, want %v", tc.version, floor, below, tc.below)
			}
		})
	}
}

// An unreadable FLOOR is the same no-evidence case as an unreadable
// engine version, and it is reachable: scripts/engine-floor.sh refuses a
// malformed constant, but a tree that has not run it yet still compiles.
func TestEngineBelowFloor_UnreadableFloorIsNotAVerdict(t *testing.T) {
	if below, ok := engineBelowFloor("19.03.15", "twenty-ten"); ok || below {
		t.Errorf("an unreadable floor gave below=%v ok=%v, want false/false", below, ok)
	}
}

// THE CONSTANT ITSELF, in the shape the shell gate and the documentation
// both read. A floor that stopped being a major.minor pair would pass
// every case above and break the lane that reconciles it.
func TestMinEngineVersion_IsAMeasuredLine(t *testing.T) {
	if _, _, ok := versionKey(MinEngineVersion); !ok {
		t.Fatalf("MinEngineVersion %q does not parse as a version", MinEngineVersion)
	}
	if strings.Count(MinEngineVersion, ".") != 1 {
		t.Errorf("MinEngineVersion %q is not major.minor; the matrix measures lines, not patches", MinEngineVersion)
	}
}

// engineFake is a daemon that answers the probe with one identity.
func engineFake(version, api string) *fakeDocker {
	return &fakeDocker{versionResult: dTypes.Version{Version: version}, clientVersion: api}
}

// THE REFUSAL NAMES BOTH NUMBERS. An operator who sees only "unsupported
// engine" has to go and find out what the minimum is and what they are
// running; the message is the one place both are already known.
func TestProbeEngine_RefusesBelowTheFloorNamingBoth(t *testing.T) {
	p := &Plugin{docker: engineFake("19.03.15", "1.40")}

	err := p.probeEngine(context.Background())
	if err == nil {
		t.Fatal("probeEngine accepted an engine below the floor")
	}
	if !errors.Is(err, errEngineTooOld) {
		t.Errorf("error %v does not wrap errEngineTooOld", err)
	}
	if !strings.Contains(err.Error(), "19.03.15") {
		t.Errorf("the refusal does not name the engine it saw: %v", err)
	}
	if !strings.Contains(err.Error(), MinEngineVersion) {
		t.Errorf("the refusal does not name the floor %q: %v", MinEngineVersion, err)
	}

	// The identity is still published. An operator reading the health
	// document of a plugin that refused needs the version it refused on.
	if got := p.engineSnapshot().Version; got != "19.03.15" {
		t.Errorf("engine version after a refusal: got %q want 19.03.15", got)
	}
}

func TestProbeEngine_AcceptsTheFloorAndAbove(t *testing.T) {
	for _, v := range []string{MinEngineVersion + ".24", "26.1.4", "29.8.0"} {
		p := &Plugin{docker: engineFake(v, "1.44")}
		if err := p.probeEngine(context.Background()); err != nil {
			t.Errorf("probeEngine(%s): %v", v, err)
		}
		if got := p.engineSnapshot().Version; got != v {
			t.Errorf("engine version: got %q want %q", got, v)
		}
		if got := p.engineSnapshot().APIVersion; got != "1.44" {
			t.Errorf("api version: got %q want 1.44", got)
		}
	}
}

// #383's shape, and the reason this probe cannot fail closed. Docker
// respawns the plugin during its own startup, and the daemon is
// routinely not serving at that moment. A refusal here would turn the
// normal startup race into a plugin that will not install.
func TestProbeEngine_AnUnreachableDaemonIsNotARefusal(t *testing.T) {
	f := &fakeDocker{pingErr: errors.New("dial unix /var/run/docker.sock: connect: no such file")}
	p := &Plugin{docker: f}

	out := captureLog(t, func() {
		if err := p.probeEngine(context.Background()); err != nil {
			t.Fatalf("an unreachable daemon must not refuse: %v", err)
		}
	})

	id := p.engineSnapshot()
	if id.Version != unknownEngineField || id.APIVersion != unknownEngineField {
		t.Errorf("identity after an unreachable daemon: %+v, want both %q", id, unknownEngineField)
	}
	// A floor that was never applied must not look like one that passed.
	if !strings.Contains(out, "the minimum was not checked") {
		t.Errorf("the log does not say the minimum went unchecked: %s", out)
	}
}

// A version string the comparison cannot read is the other no-verdict
// arm, and it must be audible for the same reason.
func TestProbeEngine_AnUnreadableVersionIsNotARefusal(t *testing.T) {
	p := &Plugin{docker: engineFake("Docker Engine, but not a version", "1.44")}

	out := captureLog(t, func() {
		if err := p.probeEngine(context.Background()); err != nil {
			t.Fatalf("an unreadable version must not refuse: %v", err)
		}
	})

	if !strings.Contains(out, "could not be read as a version") {
		t.Errorf("the log does not say the version was unreadable: %s", out)
	}
}

// A daemon that answers the ping and then reports an empty version is
// not an identity. Recording it would publish `engine_version: ""`,
// which reads as "nothing to report" in a document whose whole purpose
// here is to say what the daemon is.
func TestProbeEngine_AnEmptyVersionIsNoIdentity(t *testing.T) {
	p := &Plugin{docker: engineFake("", "1.44")}

	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("an empty version must not refuse: %v", err)
	}
	if got := p.engineSnapshot().Version; got != unknownEngineField {
		t.Errorf("engine version: got %q want %q", got, unknownEngineField)
	}
}

// THE TWO NUMBERS DISAGREEING is the case the choice of comparand has to
// have an answer for, and it is not hypothetical: the negotiated API is
// min(this client's maximum, the daemon's maximum), and an operator can
// put a socket proxy in front of the daemon that pins an old API in
// front of a current engine. The floor compares the ENGINE version, so
// this starts; the disagreement is said out loud because it is the shape
// where the health document's two fields look contradictory.
func TestEngineFloor_APIBelowFloorWarnsWithoutRefusing(t *testing.T) {
	p := &Plugin{docker: engineFake("29.8.0", "1.24")}

	var err error
	out := captureLog(t, func() { err = p.probeEngine(context.Background()) })
	if err != nil {
		t.Fatalf("a low negotiated API must not refuse: %v", err)
	}
	if !strings.Contains(out, "negotiated Docker API version is below") {
		t.Errorf("the log does not name the disagreement: %s", out)
	}
	if !strings.Contains(out, MinEngineAPIVersion) {
		t.Errorf("the warning does not name the API the floor row reported: %s", out)
	}

	// Both values reach the health document unchanged. The one the
	// plugin did NOT compare on is the one an operator needs to see to
	// understand the warning.
	id := p.engineSnapshot()
	if id.Version != "29.8.0" || id.APIVersion != "1.24" {
		t.Errorf("identity: %+v, want engine 29.8.0 and api 1.24", id)
	}
}

// The opposite direction: an engine BELOW the floor whose negotiated API
// is above MinEngineAPIVersion still refuses. Without this, "compare the
// engine version" could be implemented as "compare whichever of the two
// looks worse" and both tests would pass.
func TestEngineFloor_AHighAPIDoesNotRescueAnOldEngine(t *testing.T) {
	p := &Plugin{docker: engineFake("19.03.15", "1.51")}

	if err := p.probeEngine(context.Background()); !errors.Is(err, errEngineTooOld) {
		t.Fatalf("a high negotiated API rescued an engine below the floor: %v", err)
	}
}

// reprobeEngine runs when the startup probe found no daemon. It must not
// refuse: by then the socket is up and the daemon may already have
// driven CreateNetwork through this process, so tearing the process down
// from a goroutine replaces one visible failure with a less visible one.
// What it owes is the statement and the published version.
func TestReprobeEngine_PublishesWithoutRefusing(t *testing.T) {
	f := &fakeDocker{pingErr: errors.New("no daemon yet")}
	p := &Plugin{docker: f}
	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("probeEngine: %v", err)
	}

	// The daemon comes up, and it is below the floor.
	f.pingErr = nil
	f.versionResult = dTypes.Version{Version: "19.03.15"}
	f.clientVersion = "1.40"

	out := captureLog(t, func() { p.reprobeEngine(context.Background()) })

	if got := p.engineSnapshot().Version; got != "19.03.15" {
		t.Errorf("engine version after the re-probe: got %q want 19.03.15", got)
	}
	if !strings.Contains(out, "below the minimum") {
		t.Errorf("the re-probe did not say the engine is below the minimum: %s", out)
	}
}

// A SECOND PROBE OF A KNOWN DAEMON IS NOT FREE and, worse, would let a
// later reading overwrite the one the refusal was taken on. The re-probe
// exists for exactly one condition.
func TestReprobeEngine_DoesNothingWhenTheIdentityIsKnown(t *testing.T) {
	f := engineFake("26.1.4", "1.45")
	p := &Plugin{docker: f}
	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("probeEngine: %v", err)
	}
	calls := f.versionCalls

	p.reprobeEngine(context.Background())

	if f.versionCalls != calls {
		t.Errorf("the re-probe asked again: %d version calls, want %d", f.versionCalls, calls)
	}
	if got := p.engineSnapshot().Version; got != "26.1.4" {
		t.Errorf("engine version: got %q want 26.1.4", got)
	}
}

// THE HEALTH DOCUMENT NEVER CARRIES AN EMPTY ENGINE FIELD. A plugin
// whose probe never ran at all — the zero Plugin, which is what every
// unit fixture in this package builds — must still render a word.
func TestHealthSnapshot_EngineFieldsAreNeverEmpty(t *testing.T) {
	p := newTestPlugin(t)

	h := p.healthSnapshot()
	if h.EngineVersion != unknownEngineField || h.APIVersion != unknownEngineField {
		t.Errorf("health engine fields: got %q/%q, want both %q",
			h.EngineVersion, h.APIVersion, unknownEngineField)
	}
}

func TestHealthSnapshot_PublishesWhatTheDaemonSaid(t *testing.T) {
	p := newTestPlugin(t)
	p.docker = engineFake("26.1.4", "1.45")
	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("probeEngine: %v", err)
	}

	h := p.healthSnapshot()
	if h.EngineVersion != "26.1.4" {
		t.Errorf("engine_version: got %q want 26.1.4", h.EngineVersion)
	}
	if h.APIVersion != "1.45" {
		t.Errorf("api_version: got %q want 1.45", h.APIVersion)
	}
}

// THE BOUNDARY BELOW IS MEASURED, not read off a changelog: the engine
// matrix's own rig was pointed at each line with an endpoint carrying
// com.docker.network.endpoint.ifname=lan0, and the container's `ip link`
// was read. 28.5.2 and 29.7.2 named the interface by the driver prefix;
// 29.8.0 named it lan0.
func TestEngineVersionAppliesIfname_TheMeasuredBoundary(t *testing.T) {
	cases := []struct {
		version string
		applies bool
		known   bool
	}{
		{"29.8.0", true, true},
		{"29.7.2", false, true},
		{"28.5.2", false, true},
		{"20.10.24", false, true},
		{"30.0.0", true, true},
		{unknownEngineField, false, false},
		{"", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			p := &Plugin{}
			p.engine.Store(&engineIdentity{Version: tc.version, APIVersion: "1.51"})

			applies, known := p.engineVersionAppliesIfname()
			if known != tc.known {
				t.Fatalf("known = %v, want %v", known, tc.known)
			}
			if applies != tc.applies {
				t.Errorf("applies = %v, want %v", applies, tc.applies)
			}
		})
	}
}

// THE DEGRADATION IS SILENT EVERYWHERE ELSE. Docker accepts the request,
// the container comes up, the network works, and the interface has a
// name nobody asked for. These two cases pin the statement and the
// counter, in both directions, because a plugin that warns on every
// engine would pass a one-sided test and tell every operator their
// engine is too old.
func TestNoteIfnameRequest_SaysSoWhenTheEngineIgnoresIt(t *testing.T) {
	p := &Plugin{}
	p.engine.Store(&engineIdentity{Version: "28.5.2", APIVersion: "1.51"})

	out := captureLog(t, func() { p.noteIfnameRequest("net1", "ep1", "lan0") })

	if got := p.ifnameUnsupported.Load(); got != 1 {
		t.Errorf("ifname_unsupported: got %d want 1", got)
	}
	for _, want := range []string{"older than the first", "lan0", "28.5.2", MinEngineIfnameVersion} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %q: %s", want, out)
		}
	}
	// The fallback name is the engine's to choose, so the line must not
	// claim one. "eth0" is only the default when the driver's prefix is
	// eth and the index is 0.
	if strings.Contains(out, "eth0") {
		t.Errorf("the log names a fallback interface this plugin did not choose: %s", out)
	}
}

func TestNoteIfnameRequest_HonoursOnAnEngineThatApplies(t *testing.T) {
	p := &Plugin{}
	p.engine.Store(&engineIdentity{Version: "29.8.0", APIVersion: "1.56"})

	out := captureLog(t, func() { p.noteIfnameRequest("net1", "ep1", "lan0") })

	if got := p.ifnameUnsupported.Load(); got != 0 {
		t.Errorf("ifname_unsupported: got %d want 0 on an engine that applies the name", got)
	}
	if !strings.Contains(out, "Honoring custom interface name") {
		t.Errorf("the log does not record the honoured request: %s", out)
	}
}

// An engine whose version nobody read is not an engine that ignores the
// name. Counting it would put a number on a question this process never
// asked, and the counter is the one an operator alerts on.
func TestNoteIfnameRequest_CountsNothingOnAnUnknownEngine(t *testing.T) {
	p := &Plugin{}

	out := captureLog(t, func() { p.noteIfnameRequest("net1", "ep1", "lan0") })

	if got := p.ifnameUnsupported.Load(); got != 0 {
		t.Errorf("ifname_unsupported: got %d want 0 when the engine version is unknown", got)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("the log does not say the engine version is unknown: %s", out)
	}
}

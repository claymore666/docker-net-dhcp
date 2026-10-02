// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const (
	absenceNetA = "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	absenceNetB = "bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"
)

// absenceOutcome is what the stubbed one-shot does with the options it is handed.
type absenceOutcome int

const (
	outcomeFallback absenceOutcome = iota
	outcomeSLAAC
	outcomeLease
	outcomeSilentFatal
)

// absenceRig drives acquireInitialV6 through the dhcpGetIP seam on a fake clock.
type absenceRig struct {
	p       *Plugin
	mu      sync.Mutex
	now     time.Time
	outcome absenceOutcome
	modes   []proto.Mode6
}

func newAbsenceRig(t *testing.T, window time.Duration) *absenceRig {
	t.Helper()
	r := &absenceRig{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	r.p = newTestPlugin(t)
	r.p.docker = &fakeDocker{inspectErr: errors.New("this test must not call docker")}
	if err := saveOptions(absenceNetA, DHCPNetworkOptions{Bridge: "br-test", IPv6Mode: "auto"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	r.p.v6Absence.window = window
	r.p.v6Absence.now = func() time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.now
	}
	restore := dhcpGetIP
	dhcpGetIP = func(_ context.Context, _ string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		r.mu.Lock()
		r.modes = append(r.modes, o.Mode6)
		outcome := r.outcome
		r.mu.Unlock()
		switch {
		case outcome == outcomeFallback && o.Mode6 == proto.Mode6Auto:
			if o.OnV6Fallback != nil {
				o.OnV6Fallback(1)
			}
			return dhcp.Info{IP: "2001:db8::1/64", SLAAC: true}, dhcp.RAObservation{Seen: true, Managed: true}, nil
		case outcome == outcomeLease && o.Mode6 != proto.Mode6SLAAC:
			return dhcp.Info{IP: "2001:db8::100/128"}, dhcp.RAObservation{Seen: true, Managed: true}, nil
		case outcome == outcomeSilentFatal:
			return dhcp.Info{}, dhcp.RAObservation{Seen: true, Managed: true}, errors.New("no DHCPv6 server answered")
		default:
			return dhcp.Info{IP: "2001:db8::1/64", SLAAC: true}, dhcp.RAObservation{Seen: true, Managed: true}, nil
		}
	}
	t.Cleanup(func() { dhcpGetIP = restore })
	return r
}

func (r *absenceRig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

func (r *absenceRig) set(o absenceOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcome = o
}

// attach runs one endpoint's v6 one-shot and returns the mode the library was handed.
func (r *absenceRig) attach(t *testing.T, networkID, endpointID string, opts DHCPNetworkOptions) (proto.Mode6, error) {
	t.Helper()
	r.mu.Lock()
	n := len(r.modes)
	r.mu.Unlock()
	_, err := r.p.acquireInitialV6(t.Context(), opts, dhcp.DHCPClientOptions{}, v6Acquire{iface: "eth0",
		networkID: networkID, endpointID: endpointID, callStart: time.Now(), timeout: 30 * time.Second})
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.modes) != n+1 {
		t.Fatalf("the one-shot ran %d times for one attach, want 1", len(r.modes)-n)
	}
	return r.modes[n], err
}

func autoOpts() DHCPNetworkOptions { return DHCPNetworkOptions{IPv6Mode: "auto"} }

func TestV6AbsenceMemory_AFallbackMakesTheNextAttachOnTheNetworkSkipTheSolicit(t *testing.T) {
	r := newAbsenceRig(t, 10*time.Minute)
	if m, err := r.attach(t, absenceNetA, "ep-1", autoOpts()); err != nil || m != proto.Mode6Auto {
		t.Fatalf("first attach: mode %v err %v, want auto and no error", m, err)
	}
	if got := r.p.dhcpv6AutoFallbacks.Load(); got != 1 {
		t.Fatalf("dhcpv6_auto_fallbacks = %d after one fallback, want 1", got)
	}
	r.advance(3 * time.Minute)
	if m, err := r.attach(t, absenceNetA, "ep-2", autoOpts()); err != nil || m != proto.Mode6SLAAC {
		t.Errorf("second endpoint on the same network: mode %v err %v, want slaac: the memory is per network, "+
			"so another endpoint must read it", m, err)
	}
	if m, _ := r.attach(t, absenceNetB, "ep-3", autoOpts()); m != proto.Mode6Auto {
		t.Errorf("an endpoint on another network ran %v, want auto: one network's silent server says nothing "+
			"about another's", m)
	}
	if got := r.p.dhcpv6AutoFallbacks.Load(); got != 2 {
		t.Errorf("dhcpv6_auto_fallbacks = %d, want 2 (ep-1 and ep-3): the remembered attach fell back from nothing", got)
	}
}

func TestV6AbsenceMemory_TheWindowExpiresOnTheInjectedClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want proto.Mode6
	}{
		{"one nanosecond inside the window", 10*time.Minute - time.Nanosecond, proto.Mode6SLAAC},
		{"exactly the window", 10 * time.Minute, proto.Mode6Auto},
		{"past the window", time.Hour, proto.Mode6Auto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAbsenceRig(t, 10*time.Minute)
			if _, err := r.attach(t, absenceNetA, "ep-1", autoOpts()); err != nil {
				t.Fatal(err)
			}
			r.advance(tc.age)
			if m, _ := r.attach(t, absenceNetA, "ep-2", autoOpts()); m != tc.want {
				t.Errorf("attach %v after the fallback ran %v, want %v", tc.age, m, tc.want)
			}
		})
	}
}

func TestV6AbsenceMemory_ARemovedNetworkForgets(t *testing.T) {
	r := newAbsenceRig(t, 10*time.Minute)
	if _, err := r.attach(t, absenceNetA, "ep-1", autoOpts()); err != nil {
		t.Fatal(err)
	}
	if err := r.p.DeleteNetwork(DeleteNetworkRequest{NetworkID: absenceNetA}); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	r.set(outcomeSLAAC)
	if m, _ := r.attach(t, absenceNetA, "ep-2", autoOpts()); m != proto.Mode6Auto {
		t.Errorf("an attach after the network was removed ran %v, want auto: a recreated network may have a server", m)
	}
}

func TestV6AbsenceMemory_OnlyAnAutoAttachReadsIt(t *testing.T) {
	r := newAbsenceRig(t, 10*time.Minute)
	if _, err := r.attach(t, absenceNetA, "ep-1", autoOpts()); err != nil {
		t.Fatal(err)
	}
	r.set(outcomeSLAAC)
	for _, tc := range []struct {
		name string
		opts DHCPNetworkOptions
		want proto.Mode6
	}{
		{"ipv6_mode=dhcp keeps soliciting: a required server is not made optional", DHCPNetworkOptions{IPv6Mode: "dhcp"}, proto.Mode6DHCP},
		{"ipv6_auto_strict keeps auto: strict fails rather than forms", DHCPNetworkOptions{IPv6Mode: "auto", IPv6AutoStrict: true}, proto.Mode6Auto},
		{"ipv6_mode=slaac stays slaac", DHCPNetworkOptions{IPv6Mode: "slaac"}, proto.Mode6SLAAC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := r.p.dhcpv6AbsenceRemembered.Load()
			if m, _ := r.attach(t, absenceNetA, "ep-x", tc.opts); m != tc.want {
				t.Errorf("ran %v, want %v", m, tc.want)
			}
			if got := r.p.dhcpv6AbsenceRemembered.Load() - before; got != 0 {
				t.Errorf("dhcpv6_absence_remembered moved by %d, want 0", got)
			}
		})
	}
	if m, _ := r.attach(t, absenceNetA, "ep-y", autoOpts()); m != proto.Mode6SLAAC {
		t.Errorf("the memory did not survive the other modes' attaches: auto ran %v, want slaac", m)
	}
}

func TestV6AbsenceMemory_ARememberedAttachIsCountedAndLoggedOnce(t *testing.T) {
	r := newAbsenceRig(t, 10*time.Minute)
	if _, err := r.attach(t, absenceNetA, "ep-1", autoOpts()); err != nil {
		t.Fatal(err)
	}
	r.advance(90 * time.Second)
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()
	if _, err := r.attach(t, absenceNetA, "ep-2", autoOpts()); err != nil {
		t.Fatal(err)
	}
	if got := r.p.dhcpv6AbsenceRemembered.Load(); got != 1 {
		t.Errorf("dhcpv6_absence_remembered = %d after one remembered attach, want 1", got)
	}
	var lines []*log.Entry
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "without soliciting") {
			lines = append(lines, e)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d log lines name the skipped Solicit, want 1", len(lines))
	}
	e := lines[0]
	if e.Level != log.InfoLevel {
		t.Errorf("level %v, want info: the network is working as configured", e.Level)
	}
	want := log.Fields{"network": shortID(absenceNetA), "age": "1m30s", "window": "10m0s"}
	for k, v := range want {
		if e.Data[k] != v {
			t.Errorf("log field %s = %v, want %v", k, e.Data[k], v)
		}
	}
}

func TestV6AbsenceMemory_ZeroWindowNeverRecordsNorReads(t *testing.T) {
	r := newAbsenceRig(t, 0)
	for _, ep := range []string{"ep-1", "ep-2"} {
		if m, _ := r.attach(t, absenceNetA, ep, autoOpts()); m != proto.Mode6Auto {
			t.Errorf("%s ran %v with DHCPV6_ABSENCE_MEMORY=0, want auto", ep, m)
		}
	}
	if got := r.p.dhcpv6AbsenceRemembered.Load(); got != 0 {
		t.Errorf("dhcpv6_absence_remembered = %d with the memory off, want 0", got)
	}
}

func TestV6AbsenceMemory_NewPluginWindow(t *testing.T) {
	zero, five := time.Duration(0), 5*time.Minute
	for _, tc := range []struct {
		name string
		opt  *time.Duration
		want time.Duration
	}{
		{"unset takes the default", nil, defaultDHCPv6AbsenceMemory},
		{"zero is off, not the default", &zero, 0},
		{"a value is kept", &five, five},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := absenceWindowFor(Options{DHCPv6AbsenceMemory: tc.opt}); got != tc.want {
				t.Errorf("window %v, want %v", got, tc.want)
			}
		})
	}
}

func TestV6AbsenceMemory_AGrantedLeaseClearsIt(t *testing.T) {
	r := newAbsenceRig(t, 10*time.Minute)
	if _, err := r.attach(t, absenceNetA, "ep-1", autoOpts()); err != nil {
		t.Fatal(err)
	}
	// A dhcp-mode network shares no id with an auto one in practice; the same id isolates the clearing rule.
	r.set(outcomeLease)
	if m, err := r.attach(t, absenceNetA, "ep-2", DHCPNetworkOptions{IPv6Mode: "dhcp"}); err != nil || m != proto.Mode6DHCP {
		t.Fatalf("lease attach: mode %v err %v", m, err)
	}
	if m, _ := r.attach(t, absenceNetA, "ep-3", autoOpts()); m != proto.Mode6Auto {
		t.Errorf("an attach after a DHCPv6 lease was granted on the network ran %v, want auto", m)
	}
}

func TestV6AbsenceMemory_RecordedOnlyByTheFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome absenceOutcome
	}{
		{"an M=0 slaac address", outcomeSLAAC},
		{"a failed attach on a silent managed segment", outcomeSilentFatal},
		{"a granted lease", outcomeLease},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAbsenceRig(t, 10*time.Minute)
			r.set(tc.outcome)
			_, _ = r.attach(t, absenceNetA, "ep-1", autoOpts())
			r.set(outcomeSLAAC)
			if m, _ := r.attach(t, absenceNetA, "ep-2", autoOpts()); m != proto.Mode6Auto {
				t.Errorf("ran %v after %s, want auto: only the fallback proves a silent server", m, tc.name)
			}
		})
	}
}

func TestV6AbsenceMemory_ConcurrentAttachesDoNotRace(t *testing.T) {
	r := newAbsenceRig(t, 10*time.Minute)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			net := absenceNetA
			if i%2 == 1 {
				net = absenceNetB
			}
			_, _ = r.p.acquireInitialV6(context.Background(), autoOpts(), dhcp.DHCPClientOptions{}, v6Acquire{
				iface: "eth0", networkID: net, endpointID: "ep-" + string(rune('a'+i)), callStart: time.Now(),
				timeout: 30 * time.Second})
			r.p.v6Absence.forget(absenceNetB)
		}()
	}
	wg.Wait()
	if _, ok := r.p.v6Absence.age(absenceNetA); !ok {
		t.Errorf("no memory for a network every second attach fell back on")
	}
}

func TestV6AbsenceMemory_ThePersistentClientFollowsItsAttach(t *testing.T) {
	r := newAbsenceRig(t, 10*time.Minute)
	if _, err := r.attach(t, absenceNetA, "ep-unserved", autoOpts()); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.attach(t, absenceNetA, "ep-served", autoOpts()); m != proto.Mode6SLAAC {
		t.Fatalf("second attach ran %v, want slaac", m)
	}
	for _, tc := range []struct {
		endpoint string
		want     proto.Mode6
		fallback bool
	}{
		{"ep-served", proto.Mode6SLAAC, false},
		{"ep-unserved", proto.Mode6Auto, true},
	} {
		var o dhcp.DHCPClientOptions
		if err := r.p.v6Wiring(&o, autoOpts(), dhcp.Identity6{}, "", "", tc.endpoint); err != nil {
			t.Fatal(err)
		}
		if o.Mode6 != tc.want || (o.OnV6Fallback != nil) != tc.fallback {
			t.Errorf("%s's persistent client: mode %v, fallback callback %v; want %v, %v. A served endpoint whose "+
				"persistent client solicits in auto can bind a lease and move the address Docker was given",
				tc.endpoint, o.Mode6, o.OnV6Fallback != nil, tc.want, tc.fallback)
		}
	}
	if err := r.p.DeleteEndpoint(t.Context(), DeleteEndpointRequest{NetworkID: absenceNetA, EndpointID: "ep-served"}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if r.p.v6AbsenceServedEndpoint("ep-served") {
		t.Errorf("a deleted endpoint is still marked served")
	}
}

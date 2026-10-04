// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func forcerenewCounters(p *Plugin) []struct {
	name string
	got  int32
} {
	return []struct {
		name string
		got  int32
	}{
		{"forcerenews_renewed", p.forcerenewsRenewed.Load()},
		{"forcerenews_already_renewing", p.forcerenewsAlreadyRenewing.Load()},
		{"forcerenews_ack_refused", p.forcerenewsAckRefused.Load()},
		{"forcerenews_refused", p.forcerenewsRefused.Load()},
		{"reconfigures_accepted", p.reconfiguresAccepted.Load()},
		{"reconfigures_refused", p.reconfiguresRefused.Load()},
	}
}

func TestAddForcerenewStats_EachDeltaReachesItsOwnCounter(t *testing.T) {
	p := &Plugin{}
	p.addForcerenewStats(dhcp.ForcerenewStats{
		Renewed: 11, AlreadyRenewing: 22, AckRefused: 33, Refused: 44,
		ReconfiguresAccepted: 55, ReconfiguresRefused: 66,
	})
	want := []int32{11, 22, 33, 44, 55, 66}
	for i, c := range forcerenewCounters(p) {
		if c.got != want[i] {
			t.Errorf("%s = %d, want %d; the fold is writing another counter", c.name, c.got, want[i])
		}
	}
}

func TestAddForcerenewStats_TwoDeltasAccumulateAndAHugeOneSaturates(t *testing.T) {
	p := &Plugin{}
	p.addForcerenewStats(dhcp.ForcerenewStats{Renewed: 1, Refused: 2})
	p.addForcerenewStats(dhcp.ForcerenewStats{Renewed: 10, Refused: 20})
	if got := p.forcerenewsRenewed.Load(); got != 11 {
		t.Errorf("forcerenews_renewed = %d after two managers reported, want 11", got)
	}
	if got := p.forcerenewsRefused.Load(); got != 22 {
		t.Errorf("forcerenews_refused = %d after two managers reported, want 22", got)
	}
	p.addForcerenewStats(dhcp.ForcerenewStats{ReconfiguresAccepted: math.MaxInt32 + 1})
	if got := p.reconfiguresAccepted.Load(); got != math.MaxInt32 {
		t.Errorf("a delta above the surface's range left the counter at %d, want %d", got, int32(math.MaxInt32))
	}
}

func TestHealth_ServesTheForcerenewCountersAndNoneOfThemMakesItUnhealthy(t *testing.T) {
	p := &Plugin{}
	p.addForcerenewStats(dhcp.ForcerenewStats{
		Renewed: 1, AlreadyRenewing: 2, Refused: 3, AckRefused: 4,
		ReconfiguresAccepted: 5, ReconfiguresRefused: 6,
	})
	h := p.healthSnapshot()
	for _, tc := range []struct {
		name      string
		got, want int32
	}{
		{"forcerenews_renewed", h.ForcerenewsRenewed, 1},
		{"forcerenews_already_renewing", h.ForcerenewsAlreadyRenewing, 2},
		{"forcerenews_refused", h.ForcerenewsRefused, 3},
		{"forcerenews_ack_refused", h.ForcerenewsAckRefused, 4},
		{"reconfigures_accepted", h.ReconfiguresAccepted, 5},
		{"reconfigures_refused", h.ReconfiguresRefused, 6},
	} {
		if tc.got != tc.want {
			t.Errorf("%s served %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	if !h.Healthy {
		t.Error("a refused FORCERENEW or Reconfigure made the plugin unhealthy; a refusal is the protection " +
			"working or a lease with no key, and no operator action follows from it")
	}
}

func TestForcerenewReporter_LogsAtTheLevelTheDeltaDeserves(t *testing.T) {
	const net, ep = "net1234567890", "ep1234567890"
	for _, tc := range []struct {
		name  string
		delta dhcp.ForcerenewStats
		level log.Level
		// fields the line must carry with the gained value
		fields map[string]uint64
	}{
		{"obeyed only", dhcp.ForcerenewStats{Renewed: 2, AlreadyRenewing: 1, ReconfiguresAccepted: 3}, log.InfoLevel,
			map[string]uint64{"renewed": 2, "already_renewing": 1, "reconfigures_accepted": 3,
				"refused": 0, "ack_refused": 0, "reconfigures_refused": 0}},
		{"refused", dhcp.ForcerenewStats{Refused: 4}, log.WarnLevel,
			map[string]uint64{"refused": 4, "renewed": 0}},
		{"ack refused", dhcp.ForcerenewStats{AckRefused: 5}, log.WarnLevel,
			map[string]uint64{"ack_refused": 5}},
		{"reconfigure refused", dhcp.ForcerenewStats{ReconfiguresRefused: 6}, log.WarnLevel,
			map[string]uint64{"reconfigures_refused": 6}},
		{"obeyed beside refused", dhcp.ForcerenewStats{Renewed: 1, Refused: 1}, log.WarnLevel,
			map[string]uint64{"renewed": 1, "refused": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			hook := logtest.NewLocal(log.StandardLogger())
			defer hook.Reset()

			p.forcerenewReporter(net, ep)(tc.delta)

			entries := hook.AllEntries()
			if len(entries) != 1 {
				t.Fatalf("a non-zero delta logged %d line(s), want exactly 1", len(entries))
			}
			e := entries[0]
			if e.Level != tc.level {
				t.Errorf("logged at %s, want %s", e.Level, tc.level)
			}
			if e.Data["endpoint"] != shortID(ep) || e.Data["network"] != shortID(net) {
				t.Errorf("endpoint/network fields are %v/%v, want %v/%v; the counters are plugin-wide and "+
					"these say which container the server addressed", e.Data["endpoint"], e.Data["network"],
					shortID(ep), shortID(net))
			}
			for k, want := range tc.fields {
				if got := e.Data[k]; got != want {
					t.Errorf("field %s = %v, want %d", k, got, want)
				}
			}
			if got, want := p.forcerenewsRenewed.Load(), int32(tc.delta.Renewed); got != want {
				t.Errorf("the reporter folded %d into forcerenews_renewed, want %d", got, want)
			}
		})
	}
}

// The IPAM reserve one-shot has no endpoint, so its line must not carry an empty one (#1119).
func TestForcerenewReporter_TheReserveOneShotLogsNoEmptyEndpoint(t *testing.T) {
	p := &Plugin{}
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	var o dhcp.DHCPClientOptions
	if err := p.conflictWiring(&o, DHCPNetworkOptions{}, roleAcquire, "net1234567890", "", false); err != nil {
		t.Fatalf("conflictWiring: %v", err)
	}
	o.OnForcerenewStats(dhcp.ForcerenewStats{AckRefused: 1})

	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("logged %d line(s), want exactly 1", len(entries))
	}
	d := entries[0].Data
	if v, ok := d["endpoint"]; ok {
		t.Errorf("endpoint field = %q on a reservation with no endpoint, want the field omitted", v)
	}
	if d["acquisition"] != "ipam-reserve" {
		t.Errorf("acquisition = %v, want ipam-reserve; without it the line names neither an endpoint nor a path",
			d["acquisition"])
	}
	if d["ack_refused"] != uint64(1) {
		t.Errorf("ack_refused = %v, want 1", d["ack_refused"])
	}
	if got := p.forcerenewsAckRefused.Load(); got != 1 {
		t.Errorf("forcerenews_ack_refused = %d, want 1; the fold must still happen", got)
	}

	hook.Reset()
	p.forcerenewReporter("net1234567890", "ep1234567890")(dhcp.ForcerenewStats{AckRefused: 1})
	if got := hook.LastEntry().Data; got["acquisition"] != nil || got["endpoint"] != "ep1234567890" {
		t.Errorf("an endpoint line carries acquisition=%v endpoint=%v, want no acquisition and the endpoint",
			got["acquisition"], got["endpoint"])
	}
}

func TestForcerenewReporter_AZeroDeltaLogsNothingAndFoldsNothing(t *testing.T) {
	p := &Plugin{}
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p.forcerenewReporter("net1", "ep1")(dhcp.ForcerenewStats{})

	if n := len(hook.AllEntries()); n != 0 {
		t.Errorf("a zero delta logged %d line(s), want none", n)
	}
	for _, c := range forcerenewCounters(p) {
		if c.got != 0 {
			t.Errorf("%s = %d after a zero delta", c.name, c.got)
		}
	}
}

func TestForcerenewWiring_ArmsAClientAndNeverANilPlugin(t *testing.T) {
	p := &Plugin{}
	var o dhcp.DHCPClientOptions
	p.forcerenewWiring(&o, "net1", "ep1")
	if o.OnForcerenewStats == nil {
		t.Fatal("no FORCERENEW callback on a client this plugin started; nothing folds its counters")
	}
	o.OnForcerenewStats(dhcp.ForcerenewStats{Renewed: 1})
	if got := p.forcerenewsRenewed.Load(); got != 1 {
		t.Errorf("the callback left forcerenews_renewed at %d, want 1; it points somewhere else", got)
	}

	var none dhcp.DHCPClientOptions
	var nilPlugin *Plugin
	nilPlugin.forcerenewWiring(&none, "net1", "ep1")
	if none.OnForcerenewStats != nil {
		t.Error("a client with no plugin behind it was given a callback into one")
	}
}

func TestConflictWiring_ArmsTheForcerenewCallbackForEveryRoleAndFamily(t *testing.T) {
	for _, role := range []struct {
		name string
		role clientRole
	}{{"acquire", roleAcquire}, {"join", roleJoin}} {
		for _, v6 := range []bool{false, true} {
			p := &Plugin{}
			var o dhcp.DHCPClientOptions
			if err := p.conflictWiring(&o, DHCPNetworkOptions{}, role.role, "net1", "ep1", v6); err != nil {
				t.Fatalf("conflictWiring: %v", err)
			}
			if o.OnForcerenewStats == nil {
				t.Errorf("role %s v6=%v: no callback; a discarded ACK on a one-shot, or a FORCERENEW on the "+
					"persistent client, is counted nowhere", role.name, v6)
				continue
			}
			o.OnForcerenewStats(dhcp.ForcerenewStats{AckRefused: 1})
			if got := p.forcerenewsAckRefused.Load(); got != 1 {
				t.Errorf("role %s v6=%v: callback left forcerenews_ack_refused at %d, want 1", role.name, v6, got)
			}
		}
	}
}

// conflictWiring is the one function every client of either family and role is built through, so one call arms them
// all (#1119).
func TestForcerenewWiring_IsCalledOnceAndOnlyFromConflictWiring(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var sites []string
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "forcerenewWiring" {
					sites = append(sites, fn.Name.Name)
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("no production sources parsed; this test would pass vacuously")
	}
	if len(sites) != 1 || sites[0] != "conflictWiring" {
		t.Fatalf("forcerenewWiring is called from %v, want exactly one call, in conflictWiring: none leaves every "+
			"counter at zero, and a second site would arm a client twice", sites)
	}
}

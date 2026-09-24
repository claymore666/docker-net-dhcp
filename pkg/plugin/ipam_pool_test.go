// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// libnetwork re-sends the persisted --ipam-opt map at every daemon start and keeps the PoolID it was first given,
// and Go randomises map order per range, so the derivation is repeated (#110, #1010).
func TestIpamPoolID_IsAFunctionOfItsInputs(t *testing.T) {
	both := map[string]string{"parent": "eth0", "bridge": "br-lan"}
	if _, err := ipamPoolID(ipamLocalAddressSpace, "192.168.100.0/24", both); !errors.Is(err, util.ErrIPAM) {
		t.Fatalf("--ipam-opt parent and bridge together: got %v, want a %v refusal -- two keys in one suffix are two requests with one identity", err, util.ErrIPAM)
	}

	opts := map[string]string{"parent": "eth0"}
	first, err := ipamPoolID(ipamLocalAddressSpace, "192.168.100.0/24", opts)
	if err != nil {
		t.Fatalf("ipamPoolID: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := ipamPoolID(ipamLocalAddressSpace, "192.168.100.0/24", opts)
		if err != nil {
			t.Fatalf("ipamPoolID (run %d): %v", i, err)
		}
		if got != first {
			t.Fatalf("derivation %d gave %q, the first gave %q — a PoolID that is not a "+
				"function of its inputs unbinds every endpoint at the next daemon restart",
				i, got, first)
		}
	}
	if !strings.HasPrefix(first, ipamPoolIDPrefix) {
		t.Errorf("PoolID %q does not carry the %q prefix that marks it as ours in `docker network inspect`", first, ipamPoolIDPrefix)
	}
}

func TestIpamPoolID_TheTwoKeyRefusalNamesTheOptionEachModeOwns(t *testing.T) {
	owns := []struct{ mode, option, phrase string }{
		{ModeBridge, "`-o bridge=`", "`-o bridge=` on a bridge network"},
		{ModeMacvlan, "`-o parent=`", "`-o parent=` on a macvlan or ipvlan one"},
		{ModeIPvlan, "`-o parent=`", "`-o parent=` on a macvlan or ipvlan one"},
	}

	parentFirst := map[string]string{}
	parentFirst["parent"] = "eth0"
	parentFirst["bridge"] = "br-lan"
	bridgeFirst := map[string]string{}
	bridgeFirst["bridge"] = "br-lan"
	bridgeFirst["parent"] = "eth0"

	var seen string
	for i, opts := range []map[string]string{parentFirst, bridgeFirst} {
		_, err := ipamPoolID(ipamLocalAddressSpace, "192.168.100.0/24", opts)
		if !errors.Is(err, util.ErrIPAM) {
			t.Fatalf("both keys (map %d): got %v, want a %v refusal", i, err, util.ErrIPAM)
		}
		for _, o := range owns {
			if !strings.Contains(err.Error(), o.option) {
				t.Errorf("mode %q owns %s and the refusal does not name it, so an operator on such a "+
					"network is pointed at an option it does not have. Message: %q", o.mode, o.option, err)
				continue
			}
			if !strings.Contains(err.Error(), o.phrase) {
				t.Errorf("mode %q owns %s, and the refusal names that option without attaching it to "+
					"this mode (%q). An operator cannot tell from it which half is theirs. Message: %q",
					o.mode, o.option, o.phrase, err)
			}
		}
		for _, o := range owns {
			if n := strings.Count(err.Error(), o.option); n != 1 {
				t.Errorf("%s is named %d times in the refusal and must be named once, as one half "+
					"of the pair. A second mention is this driver answering a question it cannot "+
					"answer, since it has no mode to read here. Message: %q", o.option, n, err)
			}
		}

		if i == 0 {
			seen = err.Error()
			continue
		}
		if err.Error() != seen {
			t.Errorf("the same mistake produced two messages:\n  %q\n  %q", seen, err.Error())
		}
	}
}

func TestIpamPoolIDNames_ItsOneMarkerPremiseIsTheTwoKeyRefusal(t *testing.T) {
	values := map[string]string{"parent": "eth0", "bridge": "br-lan"}

	var subsets []map[string]string
	for mask := 0; mask < 1<<len(ipamPoolOptKeys); mask++ {
		opts := map[string]string{}
		for i, k := range ipamPoolOptKeys {
			if mask&(1<<i) != 0 {
				opts[k] = values[k]
			}
		}
		subsets = append(subsets, opts)
	}

	for _, opts := range subsets {
		id, err := ipamPoolID(ipamLocalAddressSpace, "192.168.100.0/24", opts)
		if err != nil {
			continue
		}
		var markers []string
		for _, k := range ipamPoolOptKeys {
			if strings.Contains(id, "/"+k+"=") {
				markers = append(markers, k)
			}
		}
		if len(markers) > 1 {
			t.Errorf("opts %v minted PoolID %q carrying %v. ipamPoolIDNames returns the first of "+
				"those by position and that answer becomes the issued pool's interface name, so "+
				"the other one names nothing and two networks differing only in it share an "+
				"identity", opts, id, markers)
		}
		key, name := ipamPoolIDNames(id)
		if len(markers) == 1 {
			if key != markers[0] || name != values[markers[0]] {
				t.Errorf("PoolID %q carries %s=%s and ipamPoolIDNames read %s=%s",
					id, markers[0], values[markers[0]], key, name)
			}
		} else if key != "" || name != "" {
			t.Errorf("PoolID %q carries no marker and ipamPoolIDNames read %s=%s", id, key, name)
		}
	}
}

func TestReferenceDocAndTheTwoKeyRefusalShareOneSentence(t *testing.T) {
	_, err := ipamPoolID(ipamLocalAddressSpace, "192.168.100.0/24",
		map[string]string{"parent": "eth0", "bridge": "br-lan"})
	if err == nil {
		t.Fatal("both keys were accepted, so there is no refusal for the page to describe")
	}

	const clause = "`-o bridge=` on a bridge network, `-o parent=` on a macvlan or ipvlan one"

	if !strings.Contains(err.Error(), clause) {
		t.Errorf("the refusal no longer carries the clause the page quotes.\n  clause: %q\n  message: %q", clause, err)
	}

	page := filepath.Join(moduleRoot(t), "docs", "reference.md")
	raw, rerr := os.ReadFile(page)
	if rerr != nil {
		t.Fatalf("reading %s: %v", page, rerr)
	}
	if !strings.Contains(strings.Join(strings.Fields(string(raw)), " "), clause) {
		t.Errorf("docs/reference.md does not carry the clause the refusal prints, so the page and "+
			"the binary describe two different messages again.\n  clause: %q", clause)
	}
}

func TestIpamPoolID_TheTwoKeyRefusalDoesNotShadowThePrefixRefusal(t *testing.T) {
	const pool = "192.168.99.4/31"

	t.Run("both keys are refused before any address is asked for", func(t *testing.T) {
		both := map[string]string{"parent": "eth0", "bridge": "br-lan"}
		_, err := ipamPoolID(ipamLocalAddressSpace, pool, both)
		if !errors.Is(err, util.ErrIPAM) {
			t.Fatalf("both keys on a %s pool: got %v, want a %v refusal", pool, err, util.ErrIPAM)
		}
		if !strings.Contains(err.Error(), "given together") {
			t.Errorf("the refusal is %q and does not say the two keys were given together", err)
		}
	})

	t.Run("one key still reaches the prefix refusal", func(t *testing.T) {
		p, _ := ipamFixture(t)
		p.ipamIndex = newIPAMIndex()
		id, err := ipamPoolID(ipamLocalAddressSpace, pool, map[string]string{"parent": "eth0"})
		if err != nil {
			t.Fatalf("one key on a %s pool was refused at create: %v", pool, err)
		}
		_, err = p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID:  id,
			Options: map[string]string{ipamOptRequestAddressType: ipamOptGateway},
		})
		if err == nil {
			t.Fatalf("a gateway address was invented on a %s pool. Every address in it can "+
				"be leased to a container, and #1010's refusal upstream must not have taken "+
				"this request out of #1019's reach", pool)
		}
		if !strings.Contains(err.Error(), "--gateway") {
			t.Errorf("the refusal is %q and does not name the option that supplies a gateway", err)
		}
	})
}

// At create libnetwork sends the pool as typed, and at the start-up replay it sends the pool this driver returned,
// so both must derive one identity (#110).
func TestIpamPoolID_CreateAndReplayDeriveOneIdentity(t *testing.T) {
	cases := []struct{ name, typed, canonical string }{
		{"host bits set", "192.168.100.7/24", "192.168.100.0/24"},
		{"already masked", "192.168.100.0/24", "192.168.100.0/24"},
		{"no subnet typed", "", ipamAnyPool},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := map[string]string{"parent": "eth0"}
			atCreate, err := ipamPoolID(ipamLocalAddressSpace, c.typed, opts)
			if err != nil {
				t.Fatalf("create derivation: %v", err)
			}
			// The driver's answer, which libnetwork stores and sends back.
			returned, err := ipamCanonicalPool(c.typed)
			if err != nil {
				t.Fatalf("ipamCanonicalPool: %v", err)
			}
			if returned != c.canonical {
				t.Fatalf("the driver answers %q for a typed %q; libnetwork stores that answer "+
					"and replays it, and the daemon's own pool check reads it, so an answer "+
					"carrying host bits is a pool nothing else agrees with. Want %q.",
					returned, c.typed, c.canonical)
			}
			atReplay, err := ipamPoolID(ipamLocalAddressSpace, c.canonical, opts)
			if err != nil {
				t.Fatalf("replay derivation: %v", err)
			}
			if atCreate != atReplay {
				t.Errorf("create derived %q and the daemon-start replay derives %q; the "+
					"daemon stores the first and replays it, so every endpoint on this "+
					"network would miss", atCreate, atReplay)
			}
		})
	}
}

func TestIpamPoolID_RefusesWhatItCannotTellApart(t *testing.T) {
	cases := []struct {
		name  string
		space string
		pool  string
		opts  map[string]string
	}{
		{"an address space that is not ours", "LocalDefault", "192.168.100.0/24", nil},
		{"an unknown ipam-opt", ipamLocalAddressSpace, "192.168.100.0/24", map[string]string{"vlan": "7"}},
		{"an ipam-opt with no value", ipamLocalAddressSpace, "192.168.100.0/24", map[string]string{"parent": ""}},
		{"a value carrying the separator", ipamLocalAddressSpace, "192.168.100.0/24", map[string]string{"parent": "eth0/1"}},
		{"a value carrying the assignment", ipamLocalAddressSpace, "192.168.100.0/24", map[string]string{"parent": "a=b"}},
		{"a pool that is not a prefix", ipamLocalAddressSpace, "192.168.100.1", nil},
		{"an IPv6 pool", ipamLocalAddressSpace, "2001:db8::/64", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ipamPoolID(c.space, c.pool, c.opts)
			if err == nil {
				t.Fatalf("derived %q; this request had to be refused", got)
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("error %v does not wrap util.ErrIPAM, so the daemon gets a 500 instead of a 400", err)
			}
		})
	}
}

func TestIssuedPoolTTLClearsTheProbeBudget(t *testing.T) {
	gap := preflightProbeBudget + 5*time.Second
	if issuedPoolTTL <= gap {
		t.Fatalf("issuedPoolTTL is %v and the preflight probe can hold the create for %v; "+
			"an issue that expires inside the probe refuses every validate_dhcp network in "+
			"IPAM mode, naming a missing pool rather than a timer", issuedPoolTTL, gap)
	}
	if issuedPoolTTL < 3*gap {
		t.Errorf("issuedPoolTTL is %v, only %.1f times the %v the probe can take. The margin "+
			"is what absorbs a slow server retrying inside that budget; derive it from the "+
			"budget rather than trimming it", issuedPoolTTL, float64(issuedPoolTTL)/float64(gap), gap)
	}
}

func TestIssuedPools_TakePrefersTheSuffixedIssue(t *testing.T) {
	now := time.Now()
	s := newIssuedPools()
	s.add("dhcp/dhcp-local/192.168.100.0/24", ipamLocalAddressSpace, "192.168.100.0/24", "", now)
	s.add("dhcp/dhcp-local/192.168.100.0/24/parent=eth1", ipamLocalAddressSpace, "192.168.100.0/24", "eth1", now)

	got, ok, _ := s.take(ipamLocalAddressSpace, "192.168.100.0/24", "eth1", now)
	if !ok {
		t.Fatal("no issue taken for a network on eth1")
	}
	if got != "dhcp/dhcp-local/192.168.100.0/24/parent=eth1" {
		t.Errorf("took %q; the eth1 network must take the issue whose suffix names eth1, or "+
			"the two networks swap bindings", got)
	}
	got, ok, _ = s.take(ipamLocalAddressSpace, "192.168.100.0/24", "eth0", now)
	if !ok || got != "dhcp/dhcp-local/192.168.100.0/24" {
		t.Errorf("second take = (%q, %v), want the unsuffixed issue", got, ok)
	}
	if n := s.len(); n != 0 {
		t.Errorf("%d issue(s) left; both were consumed", n)
	}
}

func TestIssuedPools_ExpireDropsTheUnconsumed(t *testing.T) {
	now := time.Now()
	s := newIssuedPools()
	s.add("dhcp/dhcp-local/0.0.0.0/0", ipamLocalAddressSpace, ipamAnyPool, "", now)
	if _, ok, _ := s.take(ipamLocalAddressSpace, ipamAnyPool, "", now.Add(issuedPoolTTL+time.Second)); ok {
		t.Error("an issue older than the TTL was still taken; it has outlived the create it belonged to")
	}
}

func TestIpamLeaseTimeoutFloorHolds(t *testing.T) {
	p := &Plugin{}
	budget := ipamReserveBudget()
	if budget <= 0 {
		t.Fatalf("the reserve budget is %v; the cap below would be meaningless", budget)
	}
	if got := p.ipamLeaseTimeout(DHCPNetworkOptions{LeaseTimeout: budget + time.Minute}, "pool"); got != budget {
		t.Errorf("lease_timeout %v capped to %v, want %v", budget+time.Minute, got, budget)
	}
	short := budget / 2
	if got := p.ipamLeaseTimeout(DHCPNetworkOptions{LeaseTimeout: short}, "pool"); got != short {
		t.Errorf("lease_timeout %v became %v; a value inside the budget must not be touched", short, got)
	}
	if err := dhcp.CheckLeaseTimeout(budget, proto.ConflictWait); err != nil {
		t.Errorf("the cap produces %v, which CheckLeaseTimeout refuses: %v.\n"+
			"Two guards disagreeing about one number leave the tighter one unreachable: "+
			"raise the daemon budget, lower the floor, or refuse the crossing loudly.", budget, err)
	}
}

// moby decodes this body by field name (libnetwork/ipams/remote/api), so a rename answers false to both (#110).
func TestApiIpamGetCapabilities_BothCapabilitiesAreOnTheWire(t *testing.T) {
	p := newTestPlugin(t)

	rec := httptest.NewRecorder()
	p.apiIpamGetCapabilities(rec, httptest.NewRequest(http.MethodPost, "/IpamDriver.GetCapabilities", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v (raw=%q)", err, rec.Body.String())
	}
	for _, name := range []string{"RequiresMACAddress", "RequiresRequestReplay"} {
		v, ok := raw[name]
		if !ok {
			t.Errorf("the capabilities body has no %q field; moby matches by name, so the "+
				"capability is off and nothing says so. Body: %s", name, rec.Body.String())
			continue
		}
		if v != true {
			t.Errorf("%s is %v, want true. %s", name, v, ipamCapabilityCost(name))
		}
	}
}

func ipamCapabilityCost(name string) string {
	switch name {
	case "RequiresMACAddress":
		return "RequestAddress then carries no endpoint MAC, so every reservation runs its " +
			"DHCP exchange under an invented address and the lease belongs to nobody."
	default:
		return "libnetwork stops re-asking for stored endpoints' addresses at daemon start, " +
			"so an IPAM-mode network survives a restart with its endpoints unallocated."
	}
}

func TestIpamBindingFor_TheRefusalNamesTheRealReason(t *testing.T) {
	const pool = "192.168.100.0/24"

	newPlugin := func() *Plugin {
		return &Plugin{ipamPools: newIssuedPools(), ipamIndex: newIPAMIndex()}
	}
	data := []*IPAMData{{AddressSpace: ipamLocalAddressSpace, Pool: pool}}

	t.Run("a suffix naming another interface", func(t *testing.T) {
		p := newPlugin()
		if _, err := p.RequestPool(RequestPoolRequest{
			AddressSpace: ipamLocalAddressSpace,
			Pool:         pool,
			Options:      map[string]string{"parent": "eth0"},
		}); err != nil {
			t.Fatalf("RequestPool: %v", err)
		}

		_, err := p.ipamBindingFor("net-1", data, "eth1")
		if err == nil {
			t.Fatal("a network on eth1 bound a pool identity minted for eth0")
		}
		for _, want := range []string{"eth0", "eth1", "--ipam-opt"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal is %q and does not mention %q, so it does not point at "+
					"the two flags that disagree", err, want)
			}
		}
		if strings.Contains(err.Error(), "restarted") {
			t.Errorf("the refusal is %q and blames a plugin restart. Nothing restarted; two "+
				"flags on one command line name different interfaces, and that is what the "+
				"operator has to change.", err)
		}
	})

	t.Run("nothing issued at all", func(t *testing.T) {
		p := newPlugin()
		_, err := p.ipamBindingFor("net-1", data, "eth0")
		if err == nil {
			t.Fatal("a network bound a pool identity this plugin never issued")
		}
		for _, want := range []string{"restarted", "same subnet", "--ipam-opt parent="} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal is %q and does not mention %q", err, want)
			}
		}
	})

	t.Run("the matching suffix still binds", func(t *testing.T) {
		p := newPlugin()
		if _, err := p.RequestPool(RequestPoolRequest{
			AddressSpace: ipamLocalAddressSpace,
			Pool:         pool,
			Options:      map[string]string{"parent": "eth0"},
		}); err != nil {
			t.Fatalf("RequestPool: %v", err)
		}
		b, err := p.ipamBindingFor("net-1", data, "eth0")
		if err != nil {
			t.Fatalf("a network on eth0 could not bind the pool identity minted for eth0: %v. "+
				"The refusals above are about a mismatch; the match must still work or the "+
				"option is useless.", err)
		}
		if b.Pool != pool {
			t.Errorf("bound pool %q, want %q", b.Pool, pool)
		}
	})
}

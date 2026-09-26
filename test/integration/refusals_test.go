// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// createShape is one `docker network create`: mode and driver options, the IPAM driver, and Docker's --ipv6.
type createShape struct {
	mode string
	opts map[string]string
	// ipam is true for this plugin as the IPAM driver, false for --ipam-driver null.
	ipam bool
	// enableIPv6 is Docker's own --ipv6, which is not a driver option.
	enableIPv6 bool
}

// createWith sends the shape to Docker and returns its answer; a network it creates is removed before returning.
func createWith(t *testing.T, ctx context.Context, cli *docker.Client, name string, s createShape) error {
	t.Helper()
	opts := map[string]string{"mode": s.mode}
	switch s.mode {
	case "macvlan":
		opts["parent"] = harness.HostVeth
	case "ipvlan":
		opts["parent"] = harness.IpvlanParent
	case "bridge":
		opts["bridge"] = harness.BridgeName
	}
	for k, v := range s.opts {
		opts[k] = v
	}
	ipam := &network.IPAM{Driver: "null"}
	if s.ipam {
		ipam = &network.IPAM{Driver: harness.DriverName}
	}
	req := network.CreateOptions{Driver: harness.DriverName, IPAM: ipam, Options: opts}
	if s.enableIPv6 {
		on := true
		req.EnableIPv6 = &on
	}
	res, err := cli.NetworkCreate(ctx, name, req)
	if err == nil {
		if rmErr := cli.NetworkRemove(context.Background(), res.ID); rmErr != nil {
			t.Errorf("NetworkRemove(%s) after an accepted create: %v", name, rmErr)
		}
	}
	return err
}

// networksNamed returns the networks Docker lists under exactly name; the API's name filter matches substrings.
func networksNamed(t *testing.T, ctx context.Context, cli *docker.Client, name string) []string {
	t.Helper()
	list, err := cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
	if err != nil {
		t.Fatalf("NetworkList(name=%s): %v", name, err)
	}
	var ids []string
	for _, n := range list {
		if n.Name == name {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

// refusalCase is one refusal the docs state, the sentences Docker must relay for it, and the accepted shapes beside it.
type refusalCase struct {
	name    string
	refused createShape
	// want is every fragment of the refusing site's own sentence: the option, its value, the mode, the reason.
	want     []string
	controls map[string]createShape
}

// The rows are the "is refused" sentences of docs/reference.md's ipv6, ipv6_mode, host_ifname, require_mac,
// macvlan_mode, ipvlan_mode and vlan rows and its IPAM section (#1016, #1036, #905, #902); ErrIPAM and
// ErrModeMismatch prefix every one of them, so no row matches on the prefix alone. A vlan control sits on the ipvlan
// parent with id 1, since every fixture parent with a 3-digit id is over the kernel's 15 bytes.
var refusalCases = []refusalCase{
	{
		name:    "vlan is refused on bridge and the message names the mode",
		refused: createShape{mode: "bridge", opts: map[string]string{"vlan": "100"}},
		want:    []string{"vlan cannot be set in mode=bridge"},
		controls: map[string]createShape{
			"bridge without it is accepted":  {mode: "bridge"},
			"ipvlan with vlan=1 is accepted": {mode: "ipvlan", opts: map[string]string{"vlan": "1"}},
		},
	},
	{
		name:    "a vlan ID outside 1 to 4094 is refused",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"vlan": "4095"}},
		want:    []string{`vlan "4095" is not a VLAN ID from 1 to 4094`},
		controls: map[string]createShape{
			"vlan=1 is accepted": {mode: "ipvlan", opts: map[string]string{"vlan": "1"}},
		},
	},
	{
		name:    "release_lease on a vlan sub-interface the plugin makes is refused",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"vlan": "1", "release_lease": "on_stop"}},
		want:    []string{"release_lease=on_stop is refused on " + harness.IpvlanParent + ".1", "the host has none there"},
		controls: map[string]createShape{
			"release_lease without vlan is accepted": {mode: "ipvlan", opts: map[string]string{"release_lease": "on_stop"}},
		},
	},
	{
		name:    "a vlan sub-interface name over 15 bytes is refused",
		refused: createShape{mode: "macvlan", opts: map[string]string{"vlan": "100"}},
		want:    []string{`"` + harness.HostVeth + `.100"`, "at most 15 bytes"},
		controls: map[string]createShape{
			"a parent short enough is accepted": {mode: "macvlan", opts: map[string]string{"parent": harness.IpvlanParent, "vlan": "1"}},
		},
	},
	{
		name:    "host_ifname is refused on macvlan and the message names the mode",
		refused: createShape{mode: "macvlan", opts: map[string]string{"host_ifname": "container_name"}},
		want:    []string{"host_ifname cannot be set in mode=macvlan", "leaves nothing on the host to name"},
		controls: map[string]createShape{
			"the same value is accepted on bridge": {mode: "bridge", opts: map[string]string{"host_ifname": "container_name"}},
			"macvlan without it is accepted":       {mode: "macvlan"},
		},
	},
	{
		name:    "host_ifname is refused on ipvlan and the message names the mode",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"host_ifname": "hostname"}},
		want:    []string{"host_ifname cannot be set in mode=ipvlan", "leaves nothing on the host to name"},
		controls: map[string]createShape{
			"the same value is accepted on bridge": {mode: "bridge", opts: map[string]string{"host_ifname": "hostname"}},
			"ipvlan without it is accepted":        {mode: "ipvlan"},
		},
	},
	{
		name:    "require_mac is refused on ipvlan and the message says why",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"require_mac": "true"}},
		want:    []string{"require_mac cannot be set in mode=ipvlan", "share the parent's MAC"},
		controls: map[string]createShape{
			"the same value is accepted on macvlan": {mode: "macvlan", opts: map[string]string{"require_mac": "true"}},
			"ipvlan with it off is accepted":        {mode: "ipvlan", opts: map[string]string{"require_mac": "false"}},
		},
	},
	{
		name:    "require_mac with a value that is not a boolean is refused naming the option",
		refused: createShape{mode: "bridge", opts: map[string]string{"require_mac": "yes"}},
		want:    []string{"cannot parse 'require_mac' as bool", `parsing "yes"`},
		controls: map[string]createShape{
			"require_mac=true is accepted on bridge": {mode: "bridge", opts: map[string]string{"require_mac": "true"}},
		},
	},
	{
		name:    "ipv6=true beside ipv6_mode=off is refused naming both",
		refused: createShape{mode: "macvlan", opts: map[string]string{"ipv6": "true", "ipv6_mode": "off"}},
		want:    []string{"ipv6=true and ipv6_mode=off contradict each other", "Set ipv6_mode to dhcp, slaac or auto, or drop ipv6"},
		controls: map[string]createShape{
			"ipv6=true beside ipv6_mode=dhcp is accepted": {mode: "macvlan", opts: map[string]string{"ipv6": "true", "ipv6_mode": "dhcp"}},
		},
	},
	{
		name:    "ipv6=false beside ipv6_mode=dhcp is refused naming both",
		refused: createShape{mode: "macvlan", opts: map[string]string{"ipv6": "false", "ipv6_mode": "dhcp"}},
		want:    []string{"ipv6=false and ipv6_mode=dhcp contradict each other", "Set ipv6_mode=off, or drop ipv6"},
		controls: map[string]createShape{
			"ipv6=false beside ipv6_mode=off is accepted": {mode: "macvlan", opts: map[string]string{"ipv6": "false", "ipv6_mode": "off"}},
			"ipv6_mode=dhcp alone is accepted":            {mode: "macvlan", opts: map[string]string{"ipv6_mode": "dhcp"}},
		},
	},
	{
		name:    "ipv6=false beside ipv6_mode=slaac is refused naming both",
		refused: createShape{mode: "macvlan", opts: map[string]string{"ipv6": "false", "ipv6_mode": "slaac"}},
		want:    []string{"ipv6=false and ipv6_mode=slaac contradict each other"},
		controls: map[string]createShape{
			"ipv6_mode=slaac alone is accepted": {mode: "macvlan", opts: map[string]string{"ipv6_mode": "slaac"}},
		},
	},
	{
		name:    "ipv6=false beside ipv6_mode=auto is refused naming both",
		refused: createShape{mode: "macvlan", opts: map[string]string{"ipv6": "false", "ipv6_mode": "auto"}},
		want:    []string{"ipv6=false and ipv6_mode=auto contradict each other"},
		controls: map[string]createShape{
			"ipv6_mode=auto alone is accepted": {mode: "macvlan", opts: map[string]string{"ipv6_mode": "auto"}},
		},
	},
	{
		name:    "an unknown ipv6_mode is refused with the accepted set",
		refused: createShape{mode: "macvlan", opts: map[string]string{"ipv6_mode": "stateful"}},
		want:    []string{"is not one of " + strings.Join(dhcp.IPv6Modes(), ", ")},
		controls: map[string]createShape{
			"ipv6_mode=off is accepted": {mode: "macvlan", opts: map[string]string{"ipv6_mode": "off"}},
		},
	},
	{
		name:    "ipv6_mode=slaac is refused on ipvlan naming the mode and the way out",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"ipv6_mode": "slaac"}},
		want:    []string{"ipv6_mode=slaac is not supported in mode=ipvlan", "RFC 4291 appendix A", "Use ipv6_mode=dhcp on ipvlan"},
		controls: map[string]createShape{
			"ipv6_mode=slaac is accepted on macvlan": {mode: "macvlan", opts: map[string]string{"ipv6_mode": "slaac"}},
			"ipv6_mode=dhcp is accepted on ipvlan":   {mode: "ipvlan", opts: map[string]string{"ipv6_mode": "dhcp"}},
		},
	},
	{
		name:    "ipv6_mode=auto is refused on ipvlan naming the mode and the way out",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"ipv6_mode": "auto"}},
		want:    []string{"ipv6_mode=auto is not supported in mode=ipvlan", "RFC 4291 appendix A", "Use ipv6_mode=dhcp on ipvlan"},
		controls: map[string]createShape{
			"ipv6_mode=auto is accepted on macvlan": {mode: "macvlan", opts: map[string]string{"ipv6_mode": "auto"}},
		},
	},
	{
		name:    "--ipv6 is refused in IPAM mode because no IPv6 pool is allocated",
		refused: createShape{mode: "macvlan", ipam: true, enableIPv6: true},
		want: []string{"--ipv6 is refused on a network that uses this plugin as its IPAM driver",
			"the plugin allocates no IPv6 pool", "drop --ipv6", "`-o ipv6=true` or `-o ipv6_mode=<mode>`"},
		controls: map[string]createShape{
			"IPAM mode without --ipv6 is accepted":        {mode: "macvlan", ipam: true},
			"-o ipv6=true is accepted in IPAM mode":       {mode: "macvlan", ipam: true, opts: map[string]string{"ipv6": "true"}},
			"-o ipv6_mode=slaac is accepted in IPAM mode": {mode: "macvlan", ipam: true, opts: map[string]string{"ipv6_mode": "slaac"}},
			"IPAM mode with ipv6_mode=off is accepted":    {mode: "macvlan", ipam: true, opts: map[string]string{"ipv6_mode": "off"}},
			"-o ipv6=true is accepted on null IPAM":       {mode: "macvlan", opts: map[string]string{"ipv6": "true"}},
		},
	},
	{
		name:    "an unknown macvlan_mode is refused with the accepted set",
		refused: createShape{mode: "macvlan", opts: map[string]string{"macvlan_mode": "brigde"}},
		want:    []string{`macvlan_mode "brigde" is not one of bridge, vepa, private, passthru`},
		controls: map[string]createShape{
			"macvlan_mode=vepa is accepted": {mode: "macvlan", opts: map[string]string{"macvlan_mode": "vepa"}},
		},
	},
	{
		name:    "ipvlan_mode=l3 is refused with the measured reason",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"ipvlan_mode": "l3"}},
		want:    []string{"ipvlan_mode=l3 is refused", "reaches no DHCP server and no relay", "The accepted value is l2"},
		controls: map[string]createShape{
			"ipvlan_mode=l2 is accepted": {mode: "ipvlan", opts: map[string]string{"ipvlan_mode": "l2"}},
		},
	},
	{
		name:    "ipvlan_mode=l3s is refused with the measured reason",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"ipvlan_mode": "l3s"}},
		want:    []string{"ipvlan_mode=l3s is refused", "reaches no DHCP server and no relay"},
		controls: map[string]createShape{
			"ipvlan with no ipvlan_mode is accepted": {mode: "ipvlan"},
		},
	},
	{
		name:    "an unknown ipvlan_mode is refused naming the accepted value",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"ipvlan_mode": "L2"}},
		want:    []string{`ipvlan_mode "L2" is not accepted; the accepted value is l2`},
		controls: map[string]createShape{
			"ipvlan_mode=l2 is accepted": {mode: "ipvlan", opts: map[string]string{"ipvlan_mode": "l2"}},
		},
	},
	{
		name:    "macvlan_mode is refused on ipvlan naming the mode",
		refused: createShape{mode: "ipvlan", opts: map[string]string{"macvlan_mode": "bridge"}},
		want:    []string{"macvlan_mode cannot be set in mode=ipvlan"},
		controls: map[string]createShape{
			"the same value is accepted on macvlan": {mode: "macvlan", opts: map[string]string{"macvlan_mode": "bridge"}},
		},
	},
	{
		name:    "ipvlan_mode is refused on macvlan naming the mode",
		refused: createShape{mode: "macvlan", opts: map[string]string{"ipvlan_mode": "l2"}},
		want:    []string{"ipvlan_mode cannot be set in mode=macvlan"},
		controls: map[string]createShape{
			"the same value is accepted on ipvlan": {mode: "ipvlan", opts: map[string]string{"ipvlan_mode": "l2"}},
		},
	},
	{
		name:    "macvlan_mode is refused on bridge naming the mode",
		refused: createShape{mode: "bridge", opts: map[string]string{"macvlan_mode": "private"}},
		want:    []string{"macvlan_mode cannot be set in mode=bridge"},
		controls: map[string]createShape{
			"bridge without it is accepted": {mode: "bridge"},
		},
	},
	{
		name:    "require_mac beside macvlan_mode=passthru is refused saying why",
		refused: createShape{mode: "macvlan", opts: map[string]string{"require_mac": "true", "macvlan_mode": "passthru"}},
		want:    []string{"require_mac cannot be set with macvlan_mode=passthru", "wears the parent's MAC"},
		controls: map[string]createShape{
			"require_mac beside macvlan_mode=private is accepted": {mode: "macvlan",
				opts: map[string]string{"require_mac": "true", "macvlan_mode": "private"}},
		},
	},
	{
		name:    "macvlan_mode=passthru is refused in IPAM mode",
		refused: createShape{mode: "macvlan", ipam: true, opts: map[string]string{"macvlan_mode": "passthru"}},
		want:    []string{"macvlan_mode=passthru networks cannot use this plugin as an IPAM driver", "--ipam-driver null"},
		controls: map[string]createShape{
			"IPAM mode with macvlan_mode=vepa is accepted": {mode: "macvlan", ipam: true,
				opts: map[string]string{"macvlan_mode": "vepa"}},
		},
	},
}

// A create is answered before any DHCP exchange except the preflight probe, whose budget is 8 s (#368); the daemon's
// plugin-call deadline is 30 s, so one create is bounded by that and the whole table by one per shape.

// TestCreateRefusals_EachNamesTheOptionAndTheReasonAndLeavesNoNetwork checks each documented create-time refusal against Docker's relayed error, its network list and an accepted neighbour (#1016).
func TestCreateRefusals_EachNamesTheOptionAndTheReasonAndLeavesNoNetwork(t *testing.T) {
	const perCreate = 30 * time.Second
	shapes := 0
	for _, c := range refusalCases {
		shapes += 1 + len(c.controls)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(shapes)*perCreate)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	for i, c := range refusalCases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.controls) == 0 {
				t.Fatal("a refusal row with no accepted neighbour passes against a plugin that refuses everything")
			}
			netName := "dh-itest-refuse-" + string(rune('a'+i))
			if ids := networksNamed(t, ctx, cli, netName); len(ids) != 0 {
				t.Fatalf("a network named %s already exists (%v), so the absence read after the refusal "+
					"would prove nothing", netName, ids)
			}

			err := createWith(t, ctx, cli, netName, c.refused)
			if err == nil {
				t.Fatalf("docker network create was ACCEPTED for %+v; docs/reference.md says it is refused", c.refused)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal Docker relayed does not carry %q, the part of the sentence that "+
						"names the option and the reason for this case:\n%v", w, err)
				}
			}
			if ids := networksNamed(t, ctx, cli, netName); len(ids) != 0 {
				t.Errorf("the refused create left network %s behind: %v", netName, ids)
			}

			// A control runs after the refusal, so an IPAM control also proves the refused create gave its pool back.
			for what, shape := range c.controls {
				if err := createWith(t, ctx, cli, netName+"-ok", shape); err != nil {
					t.Errorf("control %q: docker network create was refused for %+v, so the refusal above "+
						"may be the plugin refusing everything:\n%v", what, shape, err)
				}
			}
		})
	}
}

// TestLeaseTimeout_AnEmptyValueIsTheDefaultAndTheContainerLeases checks that `-o lease_timeout=` is read as unset, as docs/reference.md states since v2.2.0 (#817, #1016).
func TestLeaseTimeout_AnEmptyValueIsTheDefaultAndTheContainerLeases(t *testing.T) {
	// The default is derived from the library's RFC 2131 and RFC 5227 constants, as the plugin derives it.
	defaultTimeout := dhcp.ConflictRecoveryWindow(proto.DefaultParams(nil))
	ctx, cancel := context.WithTimeout(context.Background(), 4*defaultTimeout)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	for what, v := range map[string]string{
		"a malformed value is refused":              "bogus",
		"a value under the probe window is refused": "1s",
	} {
		err := createWith(t, ctx, cli, "dh-itest-leasetimeout-bad", createShape{mode: "macvlan",
			opts: map[string]string{"lease_timeout": v}})
		if err == nil || !strings.Contains(err.Error(), "lease_timeout") {
			t.Fatalf("control %q: lease_timeout=%s was not refused naming lease_timeout, so an accepted empty "+
				"value below would not show the option reached the parser: %v", what, v, err)
		}
	}

	const netName = "dh-itest-leasetimeout-empty"
	mark := harness.MarkPluginLog(t, ctx)
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"lease_timeout": ""})

	start := time.Now()
	_, ipv4, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	took := time.Since(start)
	harness.AssertIP(t, ipv4)
	if took > defaultTimeout {
		t.Errorf("the container took %s to lease, longer than the %s default lease_timeout", took, defaultTimeout)
	}

	logData, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read the fixture's log: %v", err)
	}
	if acked, acks := harness.ACKedTo(logData, ipv4, mac); !acked {
		t.Errorf("the server never ACKed %s to %s; ACKs for the address: %v", ipv4, mac, acks)
	}

	// The lane runs LOG_LEVEL=trace, whose CreateNetwork options line names every key, so only a complaint counts.
	for _, line := range strings.Split(harness.ReadPluginLogSince(t, ctx, mark), "\n") {
		complaint := strings.Contains(line, "level=warning") || strings.Contains(line, "level=error")
		if complaint && strings.Contains(line, "lease_timeout") {
			t.Errorf("the plugin complained about lease_timeout on a network that left it empty:\n%s", line)
		}
	}

	// An IPAM reservation logs the lease_timeout it was handed whenever that exceeds its 26 s budget, which 34 s does,
	// so that line shows the budget the empty value became; 40s shows the field carries the network's own value (#1016).
	const capMarker = "Capping lease_timeout"
	// The daemon's plugin-call wait, `docker plugin enable --timeout` default 30 s (moby plugin/manager_linux.go).
	const ipamCallDeadline = 30 * time.Second
	leaseTimeoutField := regexp.MustCompile(`(?:^| )lease_timeout=(\S+)`)
	for _, c := range []struct{ name, value, want string }{
		{"dh-itest-leasetimeout-ipam-empty", "", defaultTimeout.String()},
		{"dh-itest-leasetimeout-ipam-40s", "40s", "40s"},
	} {
		t.Run("ipam lease_timeout="+c.value, func(t *testing.T) {
			mark := harness.MarkPluginLog(t, ctx)
			harness.CreateNetworkIPAM(t, ctx, c.name, "macvlan", harness.SubnetCIDR, nil,
				map[string]string{"lease_timeout": c.value})
			_, ipv4, _ := harness.RunContainer(t, ctx, c.name, c.name+"-ctr")
			harness.AssertIP(t, ipv4)
			// The line precedes the reservation, which the daemon waits 30 s for, as TestDHCPv6's read bounds it (#868).
			window := harness.AwaitPluginLogSince(t, ctx, mark, ipamCallDeadline,
				func(w string) bool { return strings.Contains(w, capMarker) })
			var got []string
			for _, line := range strings.Split(window, "\n") {
				if strings.Contains(line, capMarker) {
					m := leaseTimeoutField.FindStringSubmatch(line)
					if m == nil {
						t.Fatalf("a %q line carries no lease_timeout field:\n%s", capMarker, line)
					}
					got = append(got, m[1])
				}
			}
			if len(got) == 0 {
				t.Fatalf("no %q line for a reservation on a network created with lease_timeout=%q, so the "+
					"budget it ran with is unseen", capMarker, c.value)
			}
			for _, v := range got {
				if v != c.want {
					t.Errorf("a reservation on the network created with lease_timeout=%q ran with lease_timeout=%s, "+
						"want %s", c.value, v, c.want)
				}
			}
		})
	}
}

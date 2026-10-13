// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// joinRouteProblems names what the container's first process did not see: the default route via the router's
// link-local address, exactly one route to each prefix in want and at least one to the prefix in held (the address's
// own, which the kernel may route itself), none of them via a gateway (#1125, #1316).
func joinRouteProblems(out, held string, want ...string) []string {
	lines := pluginV6Routes(out)
	var problems []string
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "default via fe80::") }) {
		problems = append(problems, "no IPv6 default route via the advertising router")
	}
	if held != "" {
		if n := countRoutesTo(lines, held); n < 1 {
			problems = append(problems, fmt.Sprintf("no route to the address's own prefix %s", held))
		} else if line, _ := routeTo(lines, held); strings.Contains(line, " via ") {
			problems = append(problems, fmt.Sprintf("the own prefix %s is routed via a gateway: %q", held, line))
		}
	}
	for _, cidr := range want {
		if n := countRoutesTo(lines, cidr); n != 1 {
			problems = append(problems, fmt.Sprintf("%d routes to the advertised on-link prefix %s, want 1", n, cidr))
		} else if line, _ := routeTo(lines, cidr); strings.Contains(line, " via ") {
			problems = append(problems, fmt.Sprintf("the on-link prefix %s is routed via a gateway: %q", cidr, line))
		}
	}
	return problems
}

// TestJoin_TheAdvertisedRoutesAreInTheContainerAtStart_ParentAttached is the bridge test's twin on macvlan and ipvlan,
// once per IPv6 mode and spelling the shape accepts: slaac and auto are refused on ipvlan (docs/reference.md, ipv6_mode) (#1316).
func TestJoin_TheAdvertisedRoutesAreInTheContainerAtStart_ParentAttached(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	own, second := harness.V6SubnetV6CIDR, harness.AdvertOnLinkPrefix
	// formed: the address comes from the advertisement, so it is no /128 and the kernel routes its prefix itself;
	// managed: the M flag with a DHCPv6 pool, else the first prefix is autonomous (#1316).
	cases := []struct {
		name, net       string
		at              v6Attach
		opts            map[string]string
		formed, managed bool
	}{
		{"macvlan ipv6=true", "dh-itest-jra1", onV6Macvlan, map[string]string{"ipv6": "true"}, false, true},
		{"macvlan ipv6_mode=dhcp alone", "dh-itest-jra7", onV6Macvlan, map[string]string{"ipv6": "", "ipv6_mode": "dhcp"}, false, true},
		{"macvlan ipv6_mode=slaac", "dh-itest-jra2", onV6Macvlan, map[string]string{"ipv6": "", "ipv6_mode": "slaac"}, true, false},
		{"macvlan ipv6_mode=auto, M flag", "dh-itest-jra3", onV6Macvlan, map[string]string{"ipv6": "", "ipv6_mode": "auto"}, false, true},
		{"macvlan ipv6_mode=auto, no M flag", "dh-itest-jra4", onV6Macvlan, map[string]string{"ipv6": "", "ipv6_mode": "auto"}, true, false},
		{"ipvlan ipv6=true", "dh-itest-jra5", onV6Ipvlan, map[string]string{"ipv6": "true"}, false, true},
		{"ipvlan ipv6_mode=dhcp", "dh-itest-jra6", onV6Ipvlan, map[string]string{"ipv6": "", "ipv6_mode": "dhcp"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := harness.RASpec{
				Managed:        c.managed,
				RouterLifetime: harness.AdvertRouteLifetime,
				Prefixes: []harness.RAPrefix{
					advertPrefix(t, own, !c.managed, 1800, 1800),
					advertPrefix(t, second, false, 1800, 1800),
				},
			}
			rangeArgs := harness.V6PoolWithoutRAArgs()
			if !c.managed {
				rangeArgs = harness.RangeArgsFor(harness.V6NoRA)
			}
			f, sender := startSenderSegment(t, rangeArgs, spec)
			answered := answerRouterSolicits(t, f.Bridge(), sender, spec)

			c.at.createNet(t, ctx, c.net, v6SegmentOpts(f, c.at, c.opts))
			out, err := startWithRoutesAtStart(t, ctx, cli, c.net)
			if err != nil {
				t.Fatalf("ContainerStart: %v (router solicitations answered so far: %d)", err, answered.Load())
			}

			ins, err := cli.ContainerInspect(ctx, c.net+"-ctr", docker.ContainerInspectOptions{})
			if err != nil {
				t.Fatalf("ContainerInspect(%s-ctr): %v", c.net, err)
			}
			assertAttachedAs(t, ctx, f, ins.Container.ID, c.at)

			want, held := []string{own, second}, ""
			if c.formed {
				want, held = []string{second}, own
			}
			for _, p := range joinRouteProblems(out, held, want...) {
				t.Errorf("%s when the container started (router solicitations answered: %d):\n%s",
					p, answered.Load(), out)
			}
		})
	}
}

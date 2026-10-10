// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/claymore666/dhcp-golib/proto"
	dNetwork "github.com/moby/moby/api/types/network"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// engineEnableIPv4Key is where Docker 28 and newer put `--ipv4=false`, beside an empty IPv4Data (#1135).
const engineEnableIPv4Key = "com.docker.network.enable_ipv4"

// ipv4Enabled is false on a network created with `--ipv4=false`, which has no IPv4 pool and runs no DHCPv4 (#1135).
func (o DHCPNetworkOptions) ipv4Enabled() bool {
	return !o.IPv4Off
}

// engineIPv4Off reads the engine's flag: absent means on, and a JSON bool or the string forms of one are read (#1135).
func engineIPv4Off(options map[string]interface{}) (bool, error) {
	v, ok := options[engineEnableIPv4Key]
	if !ok {
		return false, nil
	}
	switch t := v.(type) {
	case bool:
		return !t, nil
	case string:
		b, err := strconv.ParseBool(t)
		if err == nil {
			return !b, nil
		}
	}
	return false, fmt.Errorf("%w: the engine sent %s=%v, which is not a boolean", util.ErrIPAM, engineEnableIPv4Key, v)
}

// refuseIPv4Option refuses the flag written as a plugin option: the engine would still allocate an IPv4 pool and
// believe IPv4 is on while the plugin ran no DHCPv4, two truths on one network (#1135).
func refuseIPv4Option(generic interface{}) error {
	m, ok := generic.(map[string]interface{})
	if !ok {
		return nil
	}
	for k := range m {
		switch strings.ToLower(k) {
		case engineEnableIPv4Key, "ipv4_off", "ipv4":
			return fmt.Errorf("%w: %q is not a network option: switch IPv4 off with `docker network create --ipv4=false` "+
				"(Docker 28 or newer), so the engine allocates no IPv4 pool either. See issue #1135", util.ErrIPAM, k)
		}
	}
	return nil
}

// validateIPv4Off refuses a v4-off network that would leave an endpoint with no address, and one whose IPv6 mode
// cannot stand alone; the ipvlan rule for slaac and auto is validateIPv6Options', which runs first (#1135, #817).
func validateIPv4Off(opts DHCPNetworkOptions) error {
	if opts.ipv4Enabled() {
		return nil
	}
	mode, err := opts.ipv6Mode()
	if err != nil {
		return err
	}
	if mode == proto.Mode6Off {
		return fmt.Errorf("%w: --ipv4=false leaves this network no IPv4 and ipv6_mode is off, so an endpoint would get "+
			"no address at all. Set -o ipv6_mode=dhcp, slaac or auto (ipvlan takes only dhcp), or drop --ipv4=false. "+
			"See issue #1135", util.ErrIPAM)
	}
	return nil
}

// refuseExplicitV4 refuses `--ip` on a v4-off network, where the engine holds no pool to have checked it (#1135).
func refuseExplicitV4(opts DHCPNetworkOptions, explicitV4 string) error {
	if opts.ipv4Enabled() || explicitV4 == "" {
		return nil
	}
	return fmt.Errorf("%w: %s was asked for, and this network was created with --ipv4=false, so it has no IPv4 at all. "+
		"Drop --ip, or create the network without --ipv4=false. See issue #1135", util.ErrIPAM, explicitV4)
}

const ipv4OffStateName = "ipv4_off"

// inspectIPv4Off reads the flag back from Docker when the stored options are gone. An engine below 28 does not report
// the field, so its absence there is no evidence, and an engine that never answered gives none either (#1135).
func (p *Plugin) inspectIPv4Off(n dNetwork.Inspect) bool {
	below, known := engineBelowFloor(p.engineSnapshot().Version, MinEngineV6GatewayWaitVersion)
	return known && !below && !n.EnableIPv4
}

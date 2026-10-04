// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"crypto/sha256"
	"net"
)

// macForName is a locally administered unicast address derived from a link name: udev rewrites a veth's random address by
// name within about 250 ms on the hosted runner image and leaves one set at creation alone (#1147).
func macForName(name string) net.HardwareAddr {
	sum := sha256.Sum256([]byte(name))
	mac := make(net.HardwareAddr, 6)
	mac[0] = 0x02
	copy(mac[1:], sum[:5])
	return mac
}

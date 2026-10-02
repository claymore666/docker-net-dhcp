// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import "fmt"

// FourMessageExchange accepts (DISCOVER OFFER)+ REQUEST ACK as the start of kinds and ignores whatever follows the ACK.
// A client may repeat its DISCOVER (a late link-up on a loaded runner does) and the server answers each (#1154).
func FourMessageExchange(kinds []string) error {
	i := 0
	for i+1 < len(kinds) && kinds[i] == "DISCOVER" && kinds[i+1] == "OFFER" {
		i += 2
	}
	if i > 0 && i+1 < len(kinds) && kinds[i] == "REQUEST" && kinds[i+1] == "ACK" {
		return nil
	}
	return fmt.Errorf("the server logged %v, want one or more DISCOVER OFFER pairs, then REQUEST, ACK", kinds)
}

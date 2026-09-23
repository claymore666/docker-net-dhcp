// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"errors"
	"net/http"
)

var (
	// ErrIPAM is every refusal about address allocation; the wrapping site supplies the specific sentence (#110).
	ErrIPAM = errors.New("this network's address allocation was refused")
	// ErrBridgeRequired indicates a network bridge was not provided for network creation
	ErrBridgeRequired = errors.New("bridge required (mode=bridge)")
	// ErrNotBridge indicates that the provided network interface is not a bridge
	ErrNotBridge = errors.New("network interface is not a bridge")
	// ErrBridgeUsed indicates that a bridge is already in use
	ErrBridgeUsed = errors.New("bridge already in use by Docker")
	// ErrInvalidMode indicates an unsupported value was passed for the `mode` option
	ErrInvalidMode = errors.New("invalid mode (must be 'bridge', 'macvlan' or 'ipvlan')")
	// ErrParentRequired indicates `parent` was not provided when mode=macvlan
	ErrParentRequired = errors.New("parent required (mode=macvlan)")
	// ErrParentInvalid indicates the parent interface cannot host macvlan children
	ErrParentInvalid = errors.New("parent interface is unsuitable for macvlan (bridge or macvlan)")
	// ErrParentDown indicates the parent interface is administratively down
	ErrParentDown = errors.New("parent interface is down")
	// ErrModeMismatch indicates an option that doesn't apply to the chosen mode was set
	ErrModeMismatch = errors.New("option does not apply to selected mode")
	// ErrInvalidServerList indicates dhcp_servers or dhcp_deny_servers could not be parsed, or the two contradict.
	ErrInvalidServerList = errors.New("invalid DHCP server list")
	// ErrMACAddress indicates an invalid MAC address
	ErrMACAddress = errors.New("invalid MAC address")
	// ErrNoLease indicates a DHCP lease was not obtained from dhcpcd
	ErrNoLease = errors.New("no lease was acquired")

	// ErrNoHint indicates missing state from the CreateEndpoint stage in Join
	ErrNoHint = errors.New("missing CreateEndpoint hints")
	// ErrNotVEth indicates a host link was unexpectedly not a veth interface
	ErrNotVEth = errors.New("host link is not a veth interface")
	// ErrNoContainer indicates a container was unexpectedly not found
	ErrNoContainer = errors.New("couldn't find container by endpoint on the network")
	// ErrNoSandbox indicates missing state from the Join stage
	ErrNoSandbox = errors.New("missing joined endpoint state")
)

// libnetwork treats every 5xx alike; the distinct codes are for direct API users, logs and dashboards (#40).

// ErrToStatus maps a sentinel error to its HTTP status, 500 for anything not listed.
func ErrToStatus(err error) int {
	switch {
	case errors.Is(err, ErrIPAM), errors.Is(err, ErrBridgeRequired), errors.Is(err, ErrNotBridge),
		errors.Is(err, ErrBridgeUsed), errors.Is(err, ErrMACAddress),
		errors.Is(err, ErrInvalidMode), errors.Is(err, ErrParentRequired),
		errors.Is(err, ErrParentInvalid), errors.Is(err, ErrParentDown),
		errors.Is(err, ErrModeMismatch), errors.Is(err, ErrInvalidServerList):
		return http.StatusBadRequest

	case errors.Is(err, ErrNoLease):
		return http.StatusBadGateway

	case errors.Is(err, ErrNoContainer), errors.Is(err, ErrNoSandbox):
		return http.StatusServiceUnavailable

	case errors.Is(err, ErrNoHint), errors.Is(err, ErrNotVEth):
		return http.StatusConflict

	default:
		return http.StatusInternalServerError
	}
}

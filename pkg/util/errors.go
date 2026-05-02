package util

import (
	"errors"
	"net/http"
)

var (
	// ErrIPAM indicates an unsupported IPAM driver was used
	ErrIPAM = errors.New("only the null IPAM driver is supported")
	// ErrBridgeRequired indicates a network bridge was not provided for network creation
	ErrBridgeRequired = errors.New("bridge required (mode=bridge)")
	// ErrNotBridge indicates that the provided network interface is not a bridge
	ErrNotBridge = errors.New("network interface is not a bridge")
	// ErrBridgeUsed indicates that a bridge is already in use
	ErrBridgeUsed = errors.New("bridge already in use by Docker")
	// ErrInvalidMode indicates an unsupported value was passed for the `mode` option
	ErrInvalidMode = errors.New("invalid mode (must be 'bridge' or 'macvlan')")
	// ErrParentRequired indicates `parent` was not provided when mode=macvlan
	ErrParentRequired = errors.New("parent required (mode=macvlan)")
	// ErrParentInvalid indicates the parent interface cannot host macvlan children
	ErrParentInvalid = errors.New("parent interface is unsuitable for macvlan (bridge or macvlan)")
	// ErrParentDown indicates the parent interface is administratively down
	ErrParentDown = errors.New("parent interface is down")
	// ErrModeMismatch indicates an option that doesn't apply to the chosen mode was set
	ErrModeMismatch = errors.New("option does not apply to selected mode")
	// ErrMACAddress indicates an invalid MAC address
	ErrMACAddress = errors.New("invalid MAC address")
	// ErrNoLease indicates a DHCP lease was not obtained from udhcpc
	ErrNoLease = errors.New("udhcpc did not output a lease")
	// ErrNoHint indicates missing state from the CreateEndpoint stage in Join
	ErrNoHint = errors.New("missing CreateEndpoint hints")
	// ErrNotVEth indicates a host link was unexpectedly not a veth interface
	ErrNotVEth = errors.New("host link is not a veth interface")
	// ErrNoContainer indicates a container was unexpectedly not found
	ErrNoContainer = errors.New("couldn't find container by endpoint on the network")
	// ErrNoSandbox indicates missing state from the Join stage
	ErrNoSandbox = errors.New("missing joined endpoint state")
)

func ErrToStatus(err error) int {
	switch {
	case errors.Is(err, ErrIPAM), errors.Is(err, ErrBridgeRequired), errors.Is(err, ErrNotBridge),
		errors.Is(err, ErrBridgeUsed), errors.Is(err, ErrMACAddress),
		errors.Is(err, ErrInvalidMode), errors.Is(err, ErrParentRequired),
		errors.Is(err, ErrParentInvalid), errors.Is(err, ErrParentDown),
		errors.Is(err, ErrModeMismatch):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

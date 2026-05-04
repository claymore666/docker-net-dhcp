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

// ErrToStatus maps a sentinel error to its HTTP status. Validation
// errors (caller-supplied bad input) produce 400. Upstream-DHCP and
// retryable Docker-state-transition errors produce 502 / 503 / 409
// so the wire shape is meaningful to non-libnetwork consumers; the
// libnetwork integration treats all 5xx the same so this is purely
// a clarity win for direct API users / logs / dashboards.
//
// Anything not enumerated here falls through to 500 — those are
// either internal plumbing failures (netlink, fs) or unexpected
// daemon errors, where 500 is the honest answer.
func ErrToStatus(err error) int {
	switch {
	// Caller-supplied validation failures.
	case errors.Is(err, ErrIPAM), errors.Is(err, ErrBridgeRequired), errors.Is(err, ErrNotBridge),
		errors.Is(err, ErrBridgeUsed), errors.Is(err, ErrMACAddress),
		errors.Is(err, ErrInvalidMode), errors.Is(err, ErrParentRequired),
		errors.Is(err, ErrParentInvalid), errors.Is(err, ErrParentDown),
		errors.Is(err, ErrModeMismatch):
		return http.StatusBadRequest

	// Upstream DHCP server didn't respond — not our fault, not the
	// caller's. 502 (Bad Gateway) matches the "we depend on an
	// upstream that misbehaved" semantics.
	case errors.Is(err, ErrNoLease):
		return http.StatusBadGateway

	// Docker is in a transient state where our prerequisite isn't
	// available yet (the container or sandbox is being torn up/down).
	// 503 (Service Unavailable) signals "retry later".
	case errors.Is(err, ErrNoContainer), errors.Is(err, ErrNoSandbox):
		return http.StatusServiceUnavailable

	// Stage state mismatch — Join arrived without CreateEndpoint
	// hints, or DeleteEndpoint without a registered fingerprint.
	// 409 (Conflict) matches "the resource isn't in a state that
	// permits this operation".
	case errors.Is(err, ErrNoHint), errors.Is(err, ErrNotVEth):
		return http.StatusConflict

	default:
		return http.StatusInternalServerError
	}
}

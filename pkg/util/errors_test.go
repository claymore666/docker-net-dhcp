package util

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestErrToStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"IPAM", ErrIPAM, http.StatusBadRequest},
		{"BridgeRequired", ErrBridgeRequired, http.StatusBadRequest},
		{"NotBridge", ErrNotBridge, http.StatusBadRequest},
		{"BridgeUsed", ErrBridgeUsed, http.StatusBadRequest},
		{"MACAddress", ErrMACAddress, http.StatusBadRequest},
		{"InvalidMode", ErrInvalidMode, http.StatusBadRequest},
		{"ParentRequired", ErrParentRequired, http.StatusBadRequest},
		{"ParentInvalid", ErrParentInvalid, http.StatusBadRequest},
		{"ParentDown", ErrParentDown, http.StatusBadRequest},
		{"ModeMismatch", ErrModeMismatch, http.StatusBadRequest},

		// Upstream-misbehaviour: DHCP server didn't reply.
		{"NoLease", ErrNoLease, http.StatusBadGateway},

		// Transient Docker state — retryable.
		{"NoContainer", ErrNoContainer, http.StatusServiceUnavailable},
		{"NoSandbox", ErrNoSandbox, http.StatusServiceUnavailable},

		// Stage state mismatch — request arrived in the wrong order.
		{"NoHint", ErrNoHint, http.StatusConflict},
		{"NotVEth", ErrNotVEth, http.StatusConflict},

		// Anything we don't know about is a 500
		{"unknown", errors.New("something else"), http.StatusInternalServerError},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ErrToStatus(c.err); got != c.want {
				t.Errorf("ErrToStatus(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// TestErrToStatus_Wrapped verifies that errors.Is unwrapping works —
// callers commonly wrap our sentinel errors with fmt.Errorf("...: %w", err)
// and we still need them to map to the right HTTP status.
func TestErrToStatus_Wrapped(t *testing.T) {
	wrapped := fmt.Errorf("validation context: %w", ErrParentRequired)
	if got := ErrToStatus(wrapped); got != http.StatusBadRequest {
		t.Errorf("wrapped ErrParentRequired should map to 400, got %d", got)
	}
	// Also exercise the new non-400 mappings under wrapping.
	if got := ErrToStatus(fmt.Errorf("upstream: %w", ErrNoLease)); got != http.StatusBadGateway {
		t.Errorf("wrapped ErrNoLease should map to 502, got %d", got)
	}
	if got := ErrToStatus(fmt.Errorf("teardown race: %w", ErrNoSandbox)); got != http.StatusServiceUnavailable {
		t.Errorf("wrapped ErrNoSandbox should map to 503, got %d", got)
	}
	if got := ErrToStatus(fmt.Errorf("missing: %w", ErrNoHint)); got != http.StatusConflict {
		t.Errorf("wrapped ErrNoHint should map to 409, got %d", got)
	}
}

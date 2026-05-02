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

		// Internal errors fall through to 500
		{"NoLease", ErrNoLease, http.StatusInternalServerError},
		{"NoHint", ErrNoHint, http.StatusInternalServerError},
		{"NotVEth", ErrNotVEth, http.StatusInternalServerError},
		{"NoContainer", ErrNoContainer, http.StatusInternalServerError},
		{"NoSandbox", ErrNoSandbox, http.StatusInternalServerError},

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
}

// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

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

		{"NoLease", ErrNoLease, http.StatusBadGateway},

		{"NoContainer", ErrNoContainer, http.StatusServiceUnavailable},
		{"NoSandbox", ErrNoSandbox, http.StatusServiceUnavailable},

		{"NoHint", ErrNoHint, http.StatusConflict},
		{"NotVEth", ErrNotVEth, http.StatusConflict},

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

func TestErrToStatus_Wrapped(t *testing.T) {
	wrapped := fmt.Errorf("validation context: %w", ErrParentRequired)
	if got := ErrToStatus(wrapped); got != http.StatusBadRequest {
		t.Errorf("wrapped ErrParentRequired should map to 400, got %d", got)
	}
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

// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// TestErrIPv6Unsupported_NamesTheLineAndWhereIPv6IsTracked pins the two
// halves of the refusal an operator gets from `-o ipv6=true`.
//
// It is a test about the TEXT, which is unusual and is the point: this
// error is the whole of the IPv6 story for a 2.0 operator. Reading
// "invalid option" they cannot tell a typo from a feature that is
// coming back, and that answer decides whether they stay on 1.x. So
// the message owes them two facts -- which line refuses, and where the
// work is tracked -- and each is asserted on its own, because a
// message that kept one and lost the other would pass a single
// substring check.
//
// The integration suite asserts the same property end to end
// (TestIPv6_RefusedAtCreateWithTheMilestoneNamed, against what `docker
// network create` actually returns). This is the half that runs
// without a daemon, so a rewrite of the string is caught in the unit
// lane rather than at the next integration run.
func TestErrIPv6Unsupported_NamesTheLineAndWhereIPv6IsTracked(t *testing.T) {
	for _, c := range []struct {
		what string
		err  error
	}{
		{"the CreateNetwork refusal", ErrIPv6Unsupported},
	} {
		got := c.err.Error()
		for _, want := range []string{"2.0 line", "#911"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s does not name %q, so an operator cannot tell a rejected "+
					"option from a feature that is coming back: %s", c.what, want, got)
			}
		}
	}
}

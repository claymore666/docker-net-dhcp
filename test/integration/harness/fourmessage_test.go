// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"strings"
	"testing"
)

func TestFourMessageExchange(t *testing.T) {
	tests := []struct {
		name  string
		kinds string
		want  bool
	}{
		{"the plain exchange", "DISCOVER OFFER REQUEST ACK", true},
		{"the flake: a repeated DISCOVER, each answered", "DISCOVER OFFER DISCOVER OFFER REQUEST ACK", true},
		{"three pairs", "DISCOVER OFFER DISCOVER OFFER DISCOVER OFFER REQUEST ACK", true},
		{"a trailing message is ignored", "DISCOVER OFFER REQUEST ACK REQUEST ACK", true},
		{"a trailing partial message is ignored", "DISCOVER OFFER REQUEST ACK DISCOVER", true},
		{"rapid commit: DISCOVER ACK", "DISCOVER ACK", false},
		{"no REQUEST before the ACK", "DISCOVER OFFER ACK", false},
		{"no OFFER", "DISCOVER REQUEST ACK", false},
		{"starts with an OFFER", "OFFER DISCOVER OFFER REQUEST ACK", false},
		{"starts with a REQUEST", "REQUEST ACK", false},
		{"empty", "", false},
		{"no ACK yet", "DISCOVER OFFER REQUEST", false},
		{"no REQUEST yet", "DISCOVER OFFER", false},
		{"a DISCOVER alone", "DISCOVER", false},
		{"a pair after the REQUEST", "DISCOVER OFFER REQUEST DISCOVER OFFER ACK", false},
		{"a repeated DISCOVER whose OFFER never came is outside the grammar", "DISCOVER DISCOVER OFFER REQUEST ACK", false},
		{"a repeated REQUEST is outside the grammar", "DISCOVER OFFER REQUEST REQUEST ACK", false},
		{"rapid commit after a plain pair", "DISCOVER OFFER DISCOVER ACK", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := FourMessageExchange(strings.Fields(tc.kinds))
			if (err == nil) != tc.want {
				t.Fatalf("FourMessageExchange(%q) = %v, want ok=%v", tc.kinds, err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "[") {
				t.Errorf("the error %q does not name the kinds", err)
			}
		})
	}
}

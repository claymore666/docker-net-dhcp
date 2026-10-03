// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"testing"
	"time"
)

func TestDurationSetting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       *string
		allowZero bool
		want      time.Duration
		wantSet   bool
		wantErr   bool
	}{
		{name: "unset leaves the plugin default", raw: nil, wantSet: false},
		{name: "empty leaves the plugin default", raw: ptr(""), wantSet: false},
		{name: "a positive value", raw: ptr("15s"), want: 15 * time.Second, wantSet: true},
		{name: "zero is refused where zero means nothing", raw: ptr("0"), wantErr: true},
		{name: "zero is kept where it means off", raw: ptr("0"), allowZero: true, want: 0, wantSet: true},
		{name: "zero with a unit is kept where it means off", raw: ptr("0s"), allowZero: true, want: 0, wantSet: true},
		{name: "negative is refused where zero is allowed", raw: ptr("-1m"), allowZero: true, wantErr: true},
		{name: "negative is refused", raw: ptr("-1s"), wantErr: true},
		{name: "a malformed value is refused", raw: ptr("ten minutes"), allowZero: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(string) (string, bool) {
				if tc.raw == nil {
					return "", false
				}
				return *tc.raw, true
			}
			got, set, err := durationSetting(lookup, "DHCPV6_ABSENCE_MEMORY", tc.allowZero)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if got != tc.want || set != tc.wantSet {
				t.Errorf("got (%v, set %v), want (%v, set %v)", got, set, tc.want, tc.wantSet)
			}
		})
	}
}

func ptr(s string) *string { return &s }

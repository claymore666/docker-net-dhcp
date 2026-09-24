// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"testing"
)

type fakeMetricsListener struct {
	calls []string
	err   error
}

func (f *fakeMetricsListener) ListenMetrics(addr string) error {
	f.calls = append(f.calls, addr)
	return f.err
}

func TestListenMetricsFromEnv_UnsetOpensNothing(t *testing.T) {
	f := &fakeMetricsListener{err: errors.New("must not be called")}

	if err := listenMetricsFromEnv(f, ""); err != nil {
		t.Fatalf("empty METRICS_ADDR returned %v, want nil", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("empty METRICS_ADDR called ListenMetrics with %q", f.calls)
	}
}

func TestListenMetricsFromEnv_SetPassesTheAddressThrough(t *testing.T) {
	f := &fakeMetricsListener{}

	if err := listenMetricsFromEnv(f, "127.0.0.1:9099"); err != nil {
		t.Fatalf("ListenMetrics returned %v, want nil", err)
	}
	if len(f.calls) != 1 || f.calls[0] != "127.0.0.1:9099" {
		t.Fatalf("ListenMetrics got %q, want one call with 127.0.0.1:9099", f.calls)
	}
}

func TestListenMetricsFromEnv_BindFailureReachesTheCaller(t *testing.T) {
	want := errors.New("address already in use")
	f := &fakeMetricsListener{err: want}

	err := listenMetricsFromEnv(f, "127.0.0.1:9099")
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPLimits_SocketWriteTimeoutCannotCutAHandlerShort(t *testing.T) {
	worst := socketWorstCaseHandler()
	if worst <= 0 {
		t.Fatal("the worst-case budget computed to zero; the constants it reads have moved")
	}
	if socketWriteTimeout != 0 && socketWriteTimeout <= worst {
		t.Errorf("socketWriteTimeout = %v, which does not exceed the worst legitimate handler (%v); it would cut CreateEndpoint short", socketWriteTimeout, worst)
	}
	if socketReadHeaderTimeout <= 0 || socketReadTimeout <= 0 || socketIdleTimeout <= 0 {
		t.Error("a socket read/idle timeout is unset; a client that never completes a request still pins a goroutine")
	}
	if socketReadTimeout >= worst {
		t.Errorf("socketReadTimeout = %v is not clearly a request-read bound next to a %v handler", socketReadTimeout, worst)
	}
}

func TestHTTPLimits_MetricsServerIsFullyBounded(t *testing.T) {
	for name, d := range map[string]interface{ String() string }{
		"metricsReadHeaderTimeout": metricsReadHeaderTimeout,
		"metricsReadTimeout":       metricsReadTimeout,
		"metricsWriteTimeout":      metricsWriteTimeout,
		"metricsIdleTimeout":       metricsIdleTimeout,
	} {
		if d.String() == "0s" {
			t.Errorf("%s is unset; /metrics is a listening TCP port", name)
		}
	}
}

func TestLimitBody_CapsTheRequestBody(t *testing.T) {
	var readErr error
	h := limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))

	t.Run("an ordinary body passes", func(t *testing.T) {
		readErr = nil
		req := httptest.NewRequest(http.MethodPost, "/NetworkDriver.CreateNetwork", strings.NewReader(`{"NetworkID":"x"}`))
		h.ServeHTTP(httptest.NewRecorder(), req)
		if readErr != nil {
			t.Errorf("a small body was refused: %v", readErr)
		}
	})

	t.Run("an oversized body is refused", func(t *testing.T) {
		readErr = nil
		req := httptest.NewRequest(http.MethodPost, "/NetworkDriver.CreateNetwork", strings.NewReader(strings.Repeat("a", socketMaxBodyBytes+1)))
		h.ServeHTTP(httptest.NewRecorder(), req)
		if readErr == nil {
			t.Errorf("a body of %d bytes was read in full; the cap did not apply", socketMaxBodyBytes+1)
		}
	})
}

func TestWarnOnWildcardMetricsBind(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{":9090", true},
		{"0.0.0.0:9090", true},
		{"[::]:9090", true},
		{"127.0.0.1:9090", false},
		{"[::1]:9090", false},
		{"192.168.0.10:9090", false},
		{"not-an-address", false},
	}
	for _, tt := range tests {
		if got := warnOnWildcardMetricsBind(tt.addr); got != tt.want {
			t.Errorf("warnOnWildcardMetricsBind(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

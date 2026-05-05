package plugin

import (
	"strings"
	"testing"
)

// TestBuildResolvConf_SearchListPrecedence pins the v0.9.0 / T2-2
// rendering: option-119 (multi-entry search list) takes precedence
// over option-15 (single domain) when both are present, falls back
// to option-15 when only that is set, and emits no `search` line
// when neither is present. RFC 3397 specifies this precedence.
func TestBuildResolvConf_SearchListPrecedence(t *testing.T) {
	dns := []string{"192.0.2.1"}

	t.Run("option_119_wins_over_option_15", func(t *testing.T) {
		got := string(buildResolvConf(dns, []string{"a.example", "b.example"}, "fallback.example"))
		wantSearch := "search a.example b.example\n"
		if !strings.Contains(got, wantSearch) {
			t.Errorf("expected %q in output; got:\n%s", wantSearch, got)
		}
		if strings.Contains(got, "fallback.example") {
			t.Errorf("option-15 fallback leaked into output despite option-119 being set:\n%s", got)
		}
	})

	t.Run("option_15_used_when_119_empty", func(t *testing.T) {
		got := string(buildResolvConf(dns, nil, "single.example"))
		wantSearch := "search single.example\n"
		if !strings.Contains(got, wantSearch) {
			t.Errorf("expected %q in output; got:\n%s", wantSearch, got)
		}
	})

	t.Run("no_search_line_when_both_empty", func(t *testing.T) {
		got := string(buildResolvConf(dns, nil, ""))
		if strings.Contains(got, "search ") {
			t.Errorf("unexpected search line when both options empty:\n%s", got)
		}
		// Sanity: nameserver line still present.
		if !strings.Contains(got, "nameserver 192.0.2.1") {
			t.Errorf("nameserver missing from output:\n%s", got)
		}
	})

	t.Run("multiple_nameservers_render_in_order", func(t *testing.T) {
		got := string(buildResolvConf([]string{"192.0.2.1", "192.0.2.2"}, nil, ""))
		idx1 := strings.Index(got, "nameserver 192.0.2.1")
		idx2 := strings.Index(got, "nameserver 192.0.2.2")
		if idx1 == -1 || idx2 == -1 || idx1 >= idx2 {
			t.Errorf("expected nameservers in DHCP-supplied order; got:\n%s", got)
		}
	})
}

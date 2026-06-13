package plugin

import (
	"strings"
	"testing"
)

// TestParseIfnameOption pins the validation contract for the
// interface_name endpoint option (#125): absent is fine, valid names
// pass through, and anything the kernel's dev_valid_name would reject
// fails the Join loudly instead of surfacing as a rename error deep
// inside libnetwork.
func TestParseIfnameOption(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]interface{}
		want    string
		wantErr bool
	}{
		{"absent", map[string]interface{}{}, "", false},
		{"valid", map[string]interface{}{ifnameOption: "lan0"}, "lan0", false},
		{"valid 15 bytes", map[string]interface{}{ifnameOption: "abcdefghijklmno"}, "abcdefghijklmno", false},
		{"empty string", map[string]interface{}{ifnameOption: ""}, "", true},
		{"non-string", map[string]interface{}{ifnameOption: 42}, "", true},
		{"16 bytes", map[string]interface{}{ifnameOption: "abcdefghijklmnop"}, "", true},
		{"slash", map[string]interface{}{ifnameOption: "lan/0"}, "", true},
		{"space", map[string]interface{}{ifnameOption: "lan 0"}, "", true},
		{"tab", map[string]interface{}{ifnameOption: "lan\t0"}, "", true},
		{"dot", map[string]interface{}{ifnameOption: "."}, "", true},
		{"dotdot", map[string]interface{}{ifnameOption: ".."}, "", true},
		{"unrelated keys ignored", map[string]interface{}{"ip": "10.0.0.5"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseIfnameOption(c.options)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				if !strings.Contains(err.Error(), "interface_name") && !strings.Contains(err.Error(), ifnameOption) {
					t.Errorf("error %q does not name the offending option", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

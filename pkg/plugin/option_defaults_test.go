// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

type documentedDefault struct {
	key, value string
	modes      []string
}

var defaultRowRE = regexp.MustCompile("^\\| `([a-z0-9_]+)` \\| ([^|]*) \\| `([^`|]+)` \\|")

// documentedDefaults reads the option table's default column, so a new documented default joins the test (#1315).
func documentedDefaults(t *testing.T) []documentedDefault {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "docs", "reference.md"))
	if err != nil {
		t.Fatalf("reading the reference: %v", err)
	}
	var out []documentedDefault
	inTable := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| option | modes | default |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		m := defaultRowRE.FindStringSubmatch(line)
		if m == nil {
			if cells := strings.Split(line, "|"); len(cells) > 4 && strings.Contains(cells[3], "`") {
				t.Errorf("a default cell names a value the parse cannot read: %s", line)
			}
			continue
		}
		modes := []string{ModeBridge, ModeMacvlan, ModeIPvlan}
		if cell := strings.TrimSpace(m[2]); cell != "all" {
			modes = strings.Split(strings.ReplaceAll(cell, " ", ""), ",")
		}
		out = append(out, documentedDefault{key: m[1], value: m[3], modes: modes})
	}
	return out
}

// optionBase is the smallest valid option set for a mode.
func optionBase(mode string) map[string]interface{} {
	if mode == ModeBridge {
		return map[string]interface{}{"bridge": "br0"}
	}
	return map[string]interface{}{"mode": mode, "parent": "eth0"}
}

// resolvedOptions is what the plugin acts on: the defaultable fields through the accessors that read them, and every
// other field as decoded (#1315).
type resolvedOptions struct {
	Rest                             DHCPNetworkOptions
	Mode, Macvlan, Ipvlan, IPv6, IID string
	IPv6On                           bool
	Conflict, Release, LeaseBudget   string
	Refusals                         string
}

func resolveOptions(t *testing.T, opts DHCPNetworkOptions, set map[string]bool) resolvedOptions {
	t.Helper()
	r := resolvedOptions{Mode: opts.effectiveMode(), IPv6On: opts.ipv6Enabled()}
	mac, macErr := parseMacvlanMode(opts.MacvlanMode)
	ipvlanM, ipvlanErr := parseIPvlanMode(opts.IPvlanMode)
	m6, m6Err := opts.ipv6Mode()
	iid, iidErr := opts.ipv6IID()
	cc, ccErr := opts.conflictCheckMode()
	rl, rlErr := parseReleaseLease(opts.ReleaseLease)
	r.Macvlan, r.Ipvlan = fmt.Sprint(mac, macErr), fmt.Sprint(ipvlanM, ipvlanErr)
	r.IPv6, r.IID = fmt.Sprint(m6, m6Err), fmt.Sprint(iid, iidErr)
	r.Conflict, r.Release = fmt.Sprint(cc, ccErr), fmt.Sprint(rl, rlErr)
	r.LeaseBudget = leaseTimeoutFor(opts).String()

	p := &Plugin{}
	r.Refusals = fmt.Sprint(validateModeOptions(opts), "|", validateIPv6Options(opts, set), "|", validateIPv4Off(opts),
		"|", p.checkStoredOptions("n1", opts))

	r.Rest = opts
	r.Rest.Mode, r.Rest.MacvlanMode, r.Rest.IPvlanMode, r.Rest.IPv6Mode, r.Rest.IPv6IID = "", "", "", "", ""
	r.Rest.ConflictCheck, r.Rest.ReleaseLease, r.Rest.VendorClass, r.Rest.LeaseTimeout = "", "", "", 0
	return r
}

// A default written out must behave like the omitted option in every mode its row lists (#1315).
func TestDecodeOpts_AWrittenOutDocumentedDefaultEqualsTheOmittedOption(t *testing.T) {
	rows := documentedDefaults(t)
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.key] = true
	}
	if len(rows) < 23 {
		t.Fatalf("the option table yielded %d defaults, want at least the 23 documented", len(rows))
	}
	for _, k := range []string{"mode", "lease_timeout", "vendor_class", "require_mac"} {
		if !seen[k] {
			t.Fatalf("the option table yielded no default for %q; the parse found %d rows", k, len(rows))
		}
	}
	for _, row := range rows {
		modes := row.modes
		if row.key == "mode" {
			// The value written is the mode itself, so only the bridge base can carry it.
			modes = []string{ModeBridge}
		}
		for _, mode := range modes {
			t.Run(row.key+"="+row.value+" in "+mode, func(t *testing.T) {
				if row.key == "vendor_class" && row.value != dhcp.VendorID {
					t.Errorf("the documented vendor_class default %q is not the one sent, %q", row.value, dhcp.VendorID)
				}
				base := optionBase(mode)
				omitted, omittedSet, err := decodeOptsSet(base)
				if err != nil {
					t.Fatalf("omitted: %v", err)
				}
				written := map[string]interface{}{row.key: row.value}
				for k, v := range base {
					written[k] = v
				}
				got, gotSet, err := decodeOptsSet(written)
				if err != nil {
					t.Fatalf("%s=%s was refused at decode: %v", row.key, row.value, err)
				}
				want := resolveOptions(t, omitted, omittedSet)
				if !strings.HasPrefix(want.Refusals, "<nil>|<nil>|<nil>|<nil>") {
					t.Fatalf("the omitted base is not valid: %s", want.Refusals)
				}
				if have := resolveOptions(t, got, gotSet); !reflect.DeepEqual(have, want) {
					t.Errorf("%s=%s differs from the omitted option\n written: %+v\n omitted: %+v",
						row.key, row.value, have, want)
				}
			})
		}
	}
}

// The two refusals a written-out default can add, each stated in docs/reference.md; omitted passes (#1315, #817, #1347).
func TestDecodeOpts_AWrittenOutDefaultIsRefusedWhereTheReferenceSaysSo(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		omitted  map[string]interface{}
		written  map[string]interface{}
		mentions string
	}{
		{"ipv6=false beside ipv6_mode=dhcp", ModeBridge, map[string]interface{}{"ipv6_mode": "dhcp"}, map[string]interface{}{"ipv6_mode": "dhcp", "ipv6": "false"}, "ipv6=false"},
		{"ipv6=false beside ipv6_mode=slaac", ModeBridge, map[string]interface{}{"ipv6_mode": "slaac"}, map[string]interface{}{"ipv6_mode": "slaac", "ipv6": "false"}, "ipv6=false"},
		{"ipv6=false beside ipv6_mode=auto", ModeMacvlan, map[string]interface{}{"ipv6_mode": "auto"}, map[string]interface{}{"ipv6_mode": "auto", "ipv6": "false"}, "ipv6=false"},
		{"macvlan_mode=bridge outside macvlan", ModeBridge, nil, map[string]interface{}{"macvlan_mode": "bridge"}, "macvlan_mode"},
		{"ipvlan_mode=l2 outside ipvlan", ModeMacvlan, nil, map[string]interface{}{"ipvlan_mode": "l2"}, "ipvlan_mode"},
		{"conflict_check=wait on ipvlan", ModeIPvlan, nil, map[string]interface{}{"conflict_check": "wait"}, "conflict_check=wait"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check := func(extra map[string]interface{}) string {
				in := optionBase(c.base)
				for k, v := range extra {
					in[k] = v
				}
				opts, set, err := decodeOptsSet(in)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				return resolveOptions(t, opts, set).Refusals
			}
			if got := check(c.omitted); !strings.HasPrefix(got, "<nil>|<nil>|<nil>|<nil>") {
				t.Errorf("the option set without the written default was refused: %s", got)
			}
			if got := check(c.written); !strings.Contains(got, c.mentions) || strings.HasPrefix(got, "<nil>|<nil>|<nil>|<nil>") {
				t.Errorf("writing the default out was not refused naming %q: %s", c.mentions, got)
			}
		})
	}
}

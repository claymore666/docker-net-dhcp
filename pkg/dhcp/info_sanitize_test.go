// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// forgery is harmless as a flat token and becomes a second log line the
// moment it is rendered unquoted.
const forgery = "legit\ntime=\"2026-01-01T00:00:00Z\" level=error msg=\"FORGED\""

// TestSanitizeInfo_NoFieldEscapesTheFilter is written by REFLECTION on
// purpose, for the same reason
// TestRenderConfig_NoValueCanIntroduceADirective is: a hand-listed set
// of fields leaves the NEXT option added to Info uncovered, and the four
// string options this issue found had been unfiltered since they were
// added.
//
// It also catches the other forgetting mode: an Info field of a KIND
// sanitizeValue does not handle (a map, a nested pointer) fails here
// rather than being silently skipped.
func TestSanitizeInfo_NoFieldEscapesTheFilter(t *testing.T) {
	typ := reflect.TypeOf(Info{})

	covered := 0
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		var info Info
		v := reflect.ValueOf(&info).Elem()

		switch field.Type.Kind() {
		case reflect.String:
			v.Field(i).SetString(forgery)
		case reflect.Slice:
			switch field.Type.Elem().Kind() {
			case reflect.String:
				v.Field(i).Set(reflect.ValueOf([]string{forgery}))
			case reflect.Struct:
				// []Route: poison every string inside one element.
				elem := reflect.New(field.Type.Elem()).Elem()
				for j := 0; j < elem.NumField(); j++ {
					if elem.Field(j).Kind() == reflect.String {
						elem.Field(j).SetString(forgery)
					}
				}
				v.Field(i).Set(reflect.Append(reflect.MakeSlice(field.Type, 0, 1), elem))
			default:
				t.Fatalf("Info.%s is a slice of %s, which sanitizeValue does not handle; extend it and this test", field.Name, field.Type.Elem().Kind())
			}
		case reflect.Int, reflect.Bool:
			// Cannot carry a control character.
			continue
		default:
			t.Fatalf("Info.%s is a %s, which sanitizeValue does not handle; extend it and this test", field.Name, field.Type.Kind())
		}

		covered++
		if dropped := sanitizeInfo(&info); dropped == 0 {
			t.Errorf("Info.%s: sanitizeInfo dropped nothing from a value carrying a newline", field.Name)
		}
		if strings.Contains(dumpStrings(reflect.ValueOf(info)), "FORGED") {
			t.Errorf("Info.%s: the forged value survived sanitizeInfo", field.Name)
		}
	}

	if covered == 0 {
		t.Fatal("no Info fields were exercised; the walk is broken, not the filter")
	}
}

// dumpStrings concatenates every string reachable inside v, so the
// assertion above does not have to know where a field lives.
func dumpStrings(v reflect.Value) string {
	var b strings.Builder
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			b.WriteString(v.String())
			b.WriteByte('\n')
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i))
			}
		}
	}
	walk(v)
	return b.String()
}

// TestSanitizeInfo_LeavesLegitimateValuesAlone is the other direction:
// an over-eager filter breaks TFTP boot and timezone propagation for
// deployments doing nothing wrong.
func TestSanitizeInfo_LeavesLegitimateValuesAlone(t *testing.T) {
	info := Info{
		IP:            "192.168.99.10/24",
		Gateway:       "192.168.99.1",
		Domain:        "corp.example",
		DNSServers:    []string{"192.168.99.53", "192.168.99.54"},
		SearchList:    []string{"corp.example", "eng.corp.example"},
		TFTPServer:    "boot.corp.example",
		BootFile:      "pxelinux.0",
		WPAD:          "http://wpad.corp.example/wpad.dat",
		PosixTimezone: "CET-1CEST,M3.5.0,M10.5.0/3",
		TZDBTimezone:  "Europe/Berlin",
		TimeOffset:    "3600",
		Routes:        []Route{{Destination: "10.0.0.0/8", Gateway: "192.168.99.1"}},
	}
	want := info

	if dropped := sanitizeInfo(&info); dropped != 0 {
		t.Errorf("sanitizeInfo dropped %d legitimate values", dropped)
	}
	if !reflect.DeepEqual(info, want) {
		t.Errorf("sanitizeInfo changed a legitimate Info:\n got %+v\nwant %+v", info, want)
	}
}

// TestBuildEvent_FiltersStringOptions drives the real hook path with the
// values dhcpcd was measured to pass through unchanged. Removing the
// sanitizeInfo call in BuildEvent turns this red.
func TestBuildEvent_FiltersStringOptions(t *testing.T) {
	env := map[string]string{
		"new_ip_address":       "192.168.99.10",
		"new_subnet_mask":      "255.255.255.0",
		"new_routers":          "192.168.99.1",
		"new_tftp_server_name": "boot\nduid 00:03:00:01:de:ad:be:ef:00:01",
		"new_bootfile_name":    "pxelinux.0\rCR",
		"new_wpad":             "http://wpad/\nblacklist 192.168.99.1",
		"new_posix_timezone":   "CET\n",
		"new_tzdb_timezone":    "Europe/Berlin\x01",
	}
	ev, ok := BuildEvent("BOUND", func(k string) string { return env[k] })
	if !ok {
		t.Fatal("BuildEvent did not emit the event")
	}

	if ev.UnsafeValuesDropped != 5 {
		t.Errorf("UnsafeValuesDropped = %d, want 5", ev.UnsafeValuesDropped)
	}
	for name, got := range map[string]string{
		"TFTPServer":    ev.Data.TFTPServer,
		"BootFile":      ev.Data.BootFile,
		"WPAD":          ev.Data.WPAD,
		"PosixTimezone": ev.Data.PosixTimezone,
		"TZDBTimezone":  ev.Data.TZDBTimezone,
	} {
		if got != "" {
			t.Errorf("Info.%s = %q, want it dropped", name, got)
		}
	}
	// The lease itself must survive: a hostile option must not cost the
	// container its address.
	if ev.Data.IP != "192.168.99.10/24" {
		t.Errorf("Info.IP = %q; the lease was lost along with the bad options", ev.Data.IP)
	}
}

// TestLogRendering_StaysOnOneLine is the SECOND layer, and the reason it
// exists is that the first one was accidental: nothing pinned the
// formatter, so "these values are harmless" rested on a default that any
// configuration change could take away.
//
// It renders a poisoned field through the logger the plugin actually
// installs and asserts the record occupies exactly one line.
func TestLogRendering_StaysOnOneLine(t *testing.T) {
	var buf bytes.Buffer
	l := log.New()
	l.SetOutput(&buf)
	l.SetFormatter(&log.TextFormatter{DisableColors: true, DisableTimestamp: true})

	l.WithField("tftp", forgery).Warn("DHCP options received")

	out := strings.TrimRight(buf.String(), "\n")
	if lines := strings.Count(out, "\n") + 1; lines != 1 {
		t.Errorf("one log record rendered as %d lines:\n%s", lines, out)
	}
	// And the newline is escaped rather than dropped: the check above
	// must be passing because the formatter quoted the value, not
	// because the value never arrived.
	if !strings.Contains(out, `\n`) {
		t.Errorf("the newline was not rendered as an escape; this test is not proving what it claims:\n%s", out)
	}
}

func TestFirstSearchDomain(t *testing.T) {
	tests := []struct {
		in        string
		want      string
		truncated bool
	}{
		{"corp.example", "corp.example", false},
		{"", "", false},
		// The measured attack: dhcpcd's option-15 dname validation
		// accepts this, and `search %s` renders both.
		{"a.attacker.test b.attacker.test", "a.attacker.test", true},
		{"a.attacker.test\tb.attacker.test", "a.attacker.test", true},
		{" corp.example", "corp.example", true},
		{"corp.example ", "corp.example", true},
		{"   ", "", true},
	}
	for _, tt := range tests {
		got, truncated := FirstSearchDomain(tt.in)
		if got != tt.want || truncated != tt.truncated {
			t.Errorf("FirstSearchDomain(%q) = (%q, %v), want (%q, %v)", tt.in, got, truncated, tt.want, tt.truncated)
		}
	}
}

// TestBuildEvent_TruncatesMultiDomain drives it through the hook path
// and counts it. Removing the FirstSearchDomain call turns this red.
func TestBuildEvent_TruncatesMultiDomain(t *testing.T) {
	env := map[string]string{
		"new_ip_address":  "192.168.99.10",
		"new_subnet_mask": "255.255.255.0",
		"new_domain_name": "a.attacker.test b.attacker.test corp.example",
	}
	ev, ok := BuildEvent("BOUND", func(k string) string { return env[k] })
	if !ok {
		t.Fatal("BuildEvent did not emit the event")
	}
	if ev.Data.Domain != "a.attacker.test" {
		t.Errorf("Info.Domain = %q, want only the first domain", ev.Data.Domain)
	}
	if ev.UnsafeValuesDropped != 1 {
		t.Errorf("UnsafeValuesDropped = %d, want 1", ev.UnsafeValuesDropped)
	}
}

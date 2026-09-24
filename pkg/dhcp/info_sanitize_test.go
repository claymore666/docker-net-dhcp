// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
	log "github.com/sirupsen/logrus"
)

// forgery becomes a second log line the moment it is rendered unquoted (#699).
const forgery = "legit\ntime=\"2026-01-01T00:00:00Z\" level=error msg=\"FORGED\""

// Reflection covers the next Info field too; four string options had gone unfiltered since they were added (#699).

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

// dumpStrings concatenates every string reachable inside v.
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

func TestInfoFromLease_FiltersStringOptions(t *testing.T) {
	l := lease.Lease{
		Addr:    netip.MustParsePrefix("192.168.99.10/24"),
		Gateway: netip.MustParseAddr("192.168.99.1"),
		Options: wire.Options{
			wire.OptTFTPServer:    []byte("boot\nduid 00:03:00:01:de:ad:be:ef:00:01"),
			wire.OptBootfileName:  []byte("pxelinux.0\rCR"),
			wire.OptWPAD:          []byte("http://wpad/\nblacklist 192.168.99.1"),
			wire.OptPosixTimezone: []byte("CET\n"),
			wire.OptTZDatabase:    []byte("Europe/Berlin\x01"),
		},
	}

	info, dropped := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if dropped != 5 {
		t.Errorf("dropped = %d, want 5", dropped)
	}
	for name, got := range map[string]string{
		"TFTPServer":    info.TFTPServer,
		"BootFile":      info.BootFile,
		"WPAD":          info.WPAD,
		"PosixTimezone": info.PosixTimezone,
		"TZDBTimezone":  info.TZDBTimezone,
	} {
		if got != "" {
			t.Errorf("Info.%s = %q, want it dropped", name, got)
		}
	}
	if info.IP != "192.168.99.10/24" {
		t.Errorf("Info.IP = %q; the lease was lost along with the bad options", info.IP)
	}
	if info.Gateway != "192.168.99.1" {
		t.Errorf("Info.Gateway = %q; the lease was lost along with the bad options", info.Gateway)
	}
}

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
		// Measured: dhcpcd's option-15 dname validation accepts this, and `search %s` renders both (#699).
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

func TestInfoFromLease_TruncatesMultiDomain(t *testing.T) {
	l := lease.Lease{
		Addr:   netip.MustParsePrefix("192.168.99.10/24"),
		Domain: "a.attacker.test b.attacker.test corp.example",
	}
	info, dropped := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})
	if info.Domain != "a.attacker.test" {
		t.Errorf("Info.Domain = %q, want only the first domain", info.Domain)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

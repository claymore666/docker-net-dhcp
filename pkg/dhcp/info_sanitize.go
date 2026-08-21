// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"reflect"
	"strings"

	log "github.com/sirupsen/logrus"
)

// sanitizeInfo drops every string value inside an Info that carries a
// control character, returning how many it dropped.
//
// # WHY THIS EXISTS AT ALL, GIVEN NOTHING IS EXPLOITABLE TODAY
//
// Five options reach Info as raw server-chosen strings —
// new_tftp_server_name, new_bootfile_name, new_wpad, new_posix_timezone
// and new_tzdb_timezone. dhcpcd validates only its `dname`-typed options
// (12, 15, 66); these are `string`-typed and it passes \n and \r through
// verbatim, measured. From Info they go straight into logrus fields.
//
// Today that is safe by accident and by one thing only: logrus's default
// TextFormatter quotes the value, so a forged `level=error msg=...` stays
// inside one field. Nothing pins that formatter. Set a JSONFormatter and
// it stays safe; add a second sink, a custom formatter, or write any of
// these values to a file, and log forgery into the daemon log, -logfile
// and the integration fault census is live the same day. #703.
//
// So there are two independent layers, because one accidental layer is
// not a layer: this filter at the boundary, and a test that pins
// single-line rendering. Neither is load-bearing alone.
//
// REFLECTION IS DELIBERATE. A hand-listed set of fields is exactly the
// shape this repo keeps watching rot — the same argument
// TestRenderConfig_NoValueCanIntroduceADirective makes. A new Info field
// wired up next year is covered the day it is added, and an Info field
// of a KIND this function does not handle fails
// TestSanitizeInfo_NoFieldEscapesTheFilter rather than passing silently.
//
// Dropping rather than escaping, as everywhere else on this path: the
// sinks (dhcpcd.conf, resolv.conf, a log line) have no escaping in
// common, so the only answer that holds for all of them is not to carry
// the value.
func sanitizeInfo(info *Info) int {
	return sanitizeValue(reflect.ValueOf(info).Elem())
}

// sanitizeValue is sanitizeInfo's recursive worker. Kinds it does not
// understand are left alone deliberately: numbers and bools cannot carry
// a control character, and a kind that CAN — a future map or nested
// pointer — is caught by the reflection test rather than silently
// skipped here.
func sanitizeValue(v reflect.Value) int {
	dropped := 0
	switch v.Kind() {
	case reflect.String:
		if s := v.String(); s != "" && !SafeDirectiveValue(s) {
			log.WithField("value", quoteForLog(s)).
				Warn("Dropping DHCP-supplied option value: it carries a control character")
			v.SetString("")
			return 1
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String {
			kept := v.Slice(0, 0)
			for i := 0; i < v.Len(); i++ {
				s := v.Index(i).String()
				if s != "" && !SafeDirectiveValue(s) {
					log.WithField("value", quoteForLog(s)).
						Warn("Dropping DHCP-supplied option value: it carries a control character")
					dropped++
					continue
				}
				kept = reflect.Append(kept, v.Index(i))
			}
			v.Set(kept)
			return dropped
		}
		for i := 0; i < v.Len(); i++ {
			dropped += sanitizeValue(v.Index(i))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !v.Field(i).CanSet() {
				continue
			}
			dropped += sanitizeValue(v.Field(i))
		}
	}
	return dropped
}

// quoteForLog renders a value with its control characters escaped, so
// the WARNING about a forgery attempt is not itself the forgery. %q on a
// logrus field would be quoted twice by the TextFormatter and not at all
// by a JSONFormatter; doing it here does not depend on which one is
// installed.
func quoteForLog(s string) string {
	out := make([]rune, 0, len(s)+8)
	for _, r := range s {
		switch {
		case r == '\n':
			out = append(out, '\\', 'n')
		case r == '\r':
			out = append(out, '\\', 'r')
		case r == '\t':
			out = append(out, '\\', 't')
		case r < 0x20 || r == 0x7f:
			const hex = "0123456789abcdef"
			out = append(out, '\\', 'x', rune(hex[byte(r)>>4]), rune(hex[byte(r)&0xf]))
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// FirstSearchDomain keeps only the first whitespace-separated token of
// an option-15 domain, reporting whether it had to cut anything.
//
// SafeDirectiveValue CANNOT do this job, and the reason is worth
// writing down: it rejects r < 0x20 || r == 0x7f, and 0x20 -- the space
// -- is precisely the field separator of the sink it protects. So a
// space passes the filter, `search %s` renders it verbatim, and one
// search domain becomes several. Measured end to end: dhcpcd's option-15
// dname validation accepts "a.attacker.test b.attacker.test", and the
// generated file carried both.
//
// This is the completeness gap #689 recorded one character short of
// closing. DNSServers and SearchList are structurally safe because they
// reach us through strings.Fields; Domain is taken whole, and that
// asymmetry is the whole defect. Impact is low -- it needs
// propagate_dns, where the same server already owns `nameserver` via
// option 6 -- but it lets the attacker put his domain FIRST in the
// search order, which changes which host a bare name resolves to (#704).
//
// Exported for the same reason as SafeDirectiveValue: the filter that
// counts runs at the BuildEvent boundary, and the renderer keeps the
// same rule as an uncounted backstop, so both need it.
func FirstSearchDomain(domain string) (string, bool) {
	fields := strings.Fields(domain)
	switch len(fields) {
	case 0:
		// Either empty or whitespace only; neither is a search domain.
		return "", domain != ""
	case 1:
		// Still report a change when the token was surrounded by
		// whitespace: `search " x"` is not the line we were asked for.
		return fields[0], fields[0] != domain
	default:
		return fields[0], true
	}
}

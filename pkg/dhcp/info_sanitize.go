// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"reflect"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Options 67, 100, 101 and 252 reach Info as raw server-chosen strings, and logrus's TextFormatter quoting is the only
// other layer, which nothing pins (#703). Reflection covers a field added later, and a kind it does not handle fails
// TestSanitizeInfo_NoFieldEscapesTheFilter; dropping is chosen over escaping because the sinks share no escaping.

// sanitizeInfo drops every string value in an Info that carries a control character and returns how many it dropped.
func sanitizeInfo(info *Info) int {
	return sanitizeValue(reflect.ValueOf(info).Elem())
}

// sanitizeValue is sanitizeInfo's recursive worker; a kind that could carry a string is caught by the reflection test.
func sanitizeValue(v reflect.Value) int {
	dropped := 0
	switch v.Kind() {
	case reflect.String:
		if s := v.String(); s != "" && !SafeValue(s) {
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
				if s != "" && !SafeValue(s) {
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

// quoteForLog escapes control characters here, so the forgery warning is not itself a forgery under any formatter
// (#703).
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

// SafeValue passes 0x20, the space that separates `search` entries, so one domain could become several and put an
// attacker's first in the search order (#704, #689).

// FirstSearchDomain keeps the first whitespace-separated token of an option-15 domain and reports a cut.
func FirstSearchDomain(domain string) (string, bool) {
	fields := strings.Fields(domain)
	switch len(fields) {
	case 0:
		return "", domain != ""
	case 1:
		// Surrounding whitespace is a change too: `search " x"` is not the line asked for (#699).
		return fields[0], fields[0] != domain
	default:
		return fields[0], true
	}
}

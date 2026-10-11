// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"reflect"
	"strings"
	"testing"
)

// boolOptionKeys maps every bool option key to its field, so a new boolean joins the table unedited (#1314).
func boolOptionKeys(t *testing.T) map[string]string {
	t.Helper()
	keys := map[string]string{}
	typ := reflect.TypeOf(DHCPNetworkOptions{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Bool {
			continue
		}
		key := f.Tag.Get("mapstructure")
		if key == "" {
			key = strings.ToLower(f.Name)
		}
		keys[key] = f.Name
	}
	return keys
}

// The accepted spellings are strconv.ParseBool's set, as docs/reference.md lists (#1314).
func TestDecodeOpts_EveryBooleanOptionTakesEveryDocumentedSpelling(t *testing.T) {
	keys := boolOptionKeys(t)
	for _, k := range []string{"ipv6", "require_mac"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("the reflection found no boolean option %q (an untagged and a tagged field): %v", k, keys)
		}
	}
	for key, field := range keys {
		for spelling, want := range map[string]bool{
			"true": true, "1": true, "t": true, "T": true, "TRUE": true, "True": true,
			"false": false, "0": false, "f": false, "F": false, "FALSE": false, "False": false,
		} {
			t.Run(key+"="+spelling, func(t *testing.T) {
				opts, set, err := decodeOptsSet(map[string]interface{}{key: spelling})
				if err != nil {
					t.Fatalf("%s=%s was refused: %v", key, spelling, err)
				}
				if got := reflect.ValueOf(opts).FieldByName(field).Bool(); got != want {
					t.Errorf("%s=%s decoded to %v, want %v", key, spelling, got, want)
				}
				if !set[field] {
					t.Errorf("%s=%s is not in the set of options the input carried: %v", key, spelling, set)
				}
			})
		}
		for _, word := range []string{"yes", "on", "enabled", "2", "01", " true", "true "} {
			t.Run(key+"="+word+" is refused", func(t *testing.T) {
				if _, err := decodeOpts(map[string]interface{}{key: word}); err == nil {
					t.Errorf("%s=%s was accepted; a word that is not a boolean is refused at create (#1314)", key, word)
				}
			})
		}
	}
}

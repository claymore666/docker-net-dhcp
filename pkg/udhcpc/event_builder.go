package udhcpc

import (
	"net"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Getenv reads one environment variable. The handler binary supplies
// os.Getenv at runtime; tests inject a closure over a fixed map so
// they can exercise every branch of BuildEvent without setenv churn.
type Getenv func(string) string

// BuildEvent assembles a udhcpc Event from the eventType + the env
// variables busybox udhcpc exposes to its handler script. Returns
// (event, true) when the caller should emit the event downstream;
// (zero, false) when the input is malformed (e.g. udhcpc6 reported
// an unparseable IPv6) or the type is unknown — the consumer treats
// the silent skip as "best-effort: never block the DHCP exchange on
// our own bug."
//
// Lifecycle events (deconfig / leasefail / nak) carry no data but
// are emitted with just Type set, so the plugin's persistent client
// goroutine can match on them. Unknown types log a warning and
// suppress emission — preserves the original handler's behaviour.
//
// Split out of cmd/udhcpc-handler/main.go in v0.9.0 so the env-var
// parsing has unit-test coverage independent of os.Getenv / os.Exit
// (#107 / coverage cleanup).
func BuildEvent(eventType string, getenv Getenv) (Event, bool) {
	event := Event{Type: eventType}

	switch eventType {
	case "bound", "renew":
		// busybox udhcpc6 sets `ipv6` instead of the v4 set; we use
		// LookupEnv-equivalent semantics by treating empty as "not
		// set". Tests pass either an empty string or a missing key —
		// both routes through the v4 branch — to keep the contract
		// simple for callers.
		if v6 := getenv("ipv6"); v6 != "" {
			// Defensive: strip any /mask if a future busybox emits CIDR
			// form, then canonicalise via ParseCIDR (udhcpc6 emits a
			// _lot_ of zeros).
			v6Bare := strings.SplitN(v6, "/", 2)[0]
			_, netV6, err := net.ParseCIDR(v6Bare + "/128")
			if err != nil {
				log.WithError(err).WithField("ipv6", v6).Error("Failed to parse IPv6 address; skipping event")
				return Event{}, false
			}
			event.Data.IP = netV6.String()
			// dns6 is busybox udhcpc6's env-var name for option 23.
			if dns := getenv("dns6"); dns != "" {
				event.Data.DNSServers = strings.Fields(dns)
			}
		} else {
			event.Data.IP = getenv("ip") + "/" + getenv("mask")
			event.Data.Gateway = getenv("router")
			event.Data.Domain = getenv("domain")
			// dns is busybox udhcpc's env-var name for option 6.
			if dns := getenv("dns"); dns != "" {
				event.Data.DNSServers = strings.Fields(dns)
			}
			// Option 42 (NTP servers) — env var `ntpsrv`.
			if ntp := getenv("ntpsrv"); ntp != "" {
				event.Data.NTPServers = strings.Fields(ntp)
			}
			// Option 119 (DNS Search List) — env var `search`.
			// busybox formats multi-entry lists as space-separated.
			if search := getenv("search"); search != "" {
				event.Data.SearchList = strings.Fields(search)
			}
			// Option 66 (TFTP server name) — env var `tftp`.
			event.Data.TFTPServer = getenv("tftp")
			// Option 67 (Boot file name) — env var `bootfile`.
			event.Data.BootFile = getenv("bootfile")
		}
		// MTU (option 26) applies to both v4 and v6 — udhcpc /
		// udhcpc6 both expose it as `mtu` when the server sends it.
		// Skip on parse error rather than failing the whole event;
		// the consumer treats 0 as "no MTU info".
		if raw := getenv("mtu"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				event.Data.MTU = n
			} else {
				log.WithField("mtu", raw).Warn("Failed to parse mtu env; skipping MTU propagation for this event")
			}
		}
	case "deconfig", "leasefail", "nak":
		// Nothing to populate; emit Type only.
	default:
		log.Warnf("Ignoring unknown event type `%v`", eventType)
		return Event{}, false
	}

	return event, true
}

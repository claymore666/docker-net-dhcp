package main

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("Usage: %v <event type>", os.Args[0])
		return
	}

	event := udhcpc.Event{
		Type: os.Args[1],
	}

	switch event.Type {
	case "bound", "renew":
		if v6, ok := os.LookupEnv("ipv6"); ok {
			if v6 == "" {
				log.Warn("udhcpc6 emitted empty ipv6; skipping event")
				return
			}
			// Defensive: strip any /mask if a future busybox emits CIDR form,
			// then canonicalise via ParseCIDR (udhcpc6 emits a _lot_ of zeros).
			v6Bare := strings.SplitN(v6, "/", 2)[0]
			_, netV6, err := net.ParseCIDR(v6Bare + "/128")
			if err != nil {
				log.WithError(err).WithField("ipv6", v6).Error("Failed to parse IPv6 address; skipping event")
				return
			}
			event.Data.IP = netV6.String()
			// dns6 is busybox udhcpc6's env-var name for option 23.
			if dns := os.Getenv("dns6"); dns != "" {
				event.Data.DNSServers = strings.Fields(dns)
			}
		} else {
			event.Data.IP = os.Getenv("ip") + "/" + os.Getenv("mask")
			event.Data.Gateway = os.Getenv("router")
			event.Data.Domain = os.Getenv("domain")
			// dns is busybox udhcpc's env-var name for option 6.
			if dns := os.Getenv("dns"); dns != "" {
				event.Data.DNSServers = strings.Fields(dns)
			}
			// Option 42 (NTP servers) — env var `ntpsrv`.
			if ntp := os.Getenv("ntpsrv"); ntp != "" {
				event.Data.NTPServers = strings.Fields(ntp)
			}
			// Option 119 (DNS Search List) — env var `search`.
			// busybox formats multi-entry lists as space-separated.
			if search := os.Getenv("search"); search != "" {
				event.Data.SearchList = strings.Fields(search)
			}
			// Option 66 (TFTP server name) — env var `tftp`.
			event.Data.TFTPServer = os.Getenv("tftp")
			// Option 67 (Boot file name) — env var `bootfile`.
			event.Data.BootFile = os.Getenv("bootfile")
		}
		// MTU (option 26) applies to both v4 and v6 — udhcpc / udhcpc6
		// both expose it as `mtu` when the server sends it. Skip on
		// parse error rather than failing the whole event; the caller
		// treats 0 as "no MTU info".
		if raw := os.Getenv("mtu"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				event.Data.MTU = n
			} else {
				log.WithField("mtu", raw).Warn("Failed to parse mtu env; skipping MTU propagation for this event")
			}
		}
	case "deconfig", "leasefail", "nak":
	default:
		log.Warnf("Ignoring unknown event type `%v`", event.Type)
		return
	}

	if err := json.NewEncoder(os.Stdout).Encode(event); err != nil {
		log.Fatalf("Failed to encode udhcpc event: %v", err)
	}
}

package main

import (
	"encoding/json"
	"net"
	"os"
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
		} else {
			event.Data.IP = os.Getenv("ip") + "/" + os.Getenv("mask")
			event.Data.Gateway = os.Getenv("router")
			event.Data.Domain = os.Getenv("domain")
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

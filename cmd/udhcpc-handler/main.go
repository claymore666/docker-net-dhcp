package main

import (
	"encoding/json"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
)

// main is a thin shim invoked by dhcpcd as its --script hook on every
// network event. dhcpcd passes the event in the $reason environment
// variable (not argv) along with the lease as new_*/old_* variables;
// the env-parsing logic that turns those into a structured Event lives
// in pkg/udhcpc.BuildEvent so it can be unit-tested without involving
// os.Setenv / os.Exit. Anything more than reason lookup, getenv
// passthrough, and JSON encoding belongs in the library.
//
// Migration note (#152): this used to be the busybox udhcpc/udhcpc6
// handler (event type in argv[1]); it now speaks dhcpcd's hook
// contract. The emitted newline-delimited JSON the plugin reads back is
// unchanged.
func main() {
	reason := os.Getenv("reason")
	if reason == "" {
		log.Fatal("dhcpcd hook invoked without $reason")
		return
	}

	event, emit := udhcpc.BuildEvent(reason, os.Getenv)
	if !emit {
		return
	}

	if err := json.NewEncoder(os.Stdout).Encode(event); err != nil {
		log.Fatalf("Failed to encode dhcpcd event: %v", err)
	}
}

package main

import (
	"encoding/json"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
)

// main is a thin shim: the env-parsing logic that turns busybox
// udhcpc's environment variables into a structured Event lives in
// pkg/udhcpc.BuildEvent so it can be unit-tested without involving
// os.Setenv / os.Exit. Anything more than argv validation, getenv
// passthrough, and JSON encoding belongs in the library.
func main() {
	if len(os.Args) != 2 {
		log.Fatalf("Usage: %v <event type>", os.Args[0])
		return
	}

	event, emit := udhcpc.BuildEvent(os.Args[1], os.Getenv)
	if !emit {
		return
	}

	if err := json.NewEncoder(os.Stdout).Encode(event); err != nil {
		log.Fatalf("Failed to encode udhcpc event: %v", err)
	}
}

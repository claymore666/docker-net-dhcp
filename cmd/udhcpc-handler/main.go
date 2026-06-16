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
// passthrough, FIFO selection, and JSON encoding belongs in the library.
//
// Migration note (#152): this used to be the busybox udhcpc/udhcpc6
// handler (event type in argv[1]) writing JSON to stdout; it now speaks
// dhcpcd's hook contract. dhcpcd's hook stdout is not a usable data
// channel (it's /dev/null once dhcpcd daemonises and is interleaved
// with dhcpcd's own log in foreground), so events go to the FIFO whose
// path the plugin passes in via the dhcpcd `env` directive
// (udhcpc.EventFIFOEnv). When that var is unset we fall back to stdout
// so the handler stays runnable by hand for debugging.
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

	out := os.Stdout
	if fifo := os.Getenv(udhcpc.EventFIFOEnv); fifo != "" {
		// The plugin holds the read end open (O_RDWR), so this open
		// succeeds immediately. One short JSON line per event; close
		// after writing so the line is flushed and we don't hold the
		// FIFO open across the (short-lived) hook process's lifetime.
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			log.Fatalf("Failed to open event FIFO %q: %v", fifo, err)
		}
		defer f.Close()
		out = f
	}

	if err := json.NewEncoder(out).Encode(event); err != nil {
		log.Fatalf("Failed to encode dhcpcd event: %v", err)
	}
}

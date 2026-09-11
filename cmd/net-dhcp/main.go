// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/pkg/plugin"
)

var (
	logLevel = flag.String("log", "", "log level")
	logFile  = flag.String("logfile", "", "log file")
	bindSock = flag.String("sock", "/run/docker/plugins/net-dhcp.sock", "bind unix socket")
)

func main() {
	flag.Parse()

	// logFileMu guards the SIGHUP reopen path. Without it, a HUP
	// arriving while logrus is mid-write could swap Out from under
	// the writer; the lock makes "current fd is the one we just
	// installed" hold. closeLogFile / fatalCleanup also reach for it.
	var logFileMu sync.Mutex
	var currentLogFd *os.File
	closeLogFile := func() {
		logFileMu.Lock()
		defer logFileMu.Unlock()
		if currentLogFd != nil {
			_ = currentLogFd.Close()
			currentLogFd = nil
		}
	}
	// fatalCleanup mirrors log.WithError(err).Fatal but flushes and
	// closes the log file first, so the final error line reaches disk.
	// log.Fatal calls os.Exit(1) directly, which skips deferred Closes
	// — without this helper the last logged line can be lost in the
	// stdio buffer under -logfile.
	fatalCleanup := func(err error, msg string) {
		log.WithError(err).Error(msg)
		closeLogFile()
		os.Exit(1)
	}

	if *logLevel == "" {
		if *logLevel = os.Getenv("LOG_LEVEL"); *logLevel == "" {
			*logLevel = "info"
		}
	}

	level, err := log.ParseLevel(*logLevel)
	if err != nil {
		fatalCleanup(err, "Failed to parse log level")
	}
	log.SetLevel(level)

	// The log goes to BOTH the file and stdout (#420).
	//
	// The file lives in the plugin rootfs, which Docker destroys and
	// recreates on every `docker plugin rm` / `install` — the supported
	// upgrade path. So every upgrade has silently taken all of
	// production's plugin history with it, and the moment an operator
	// most wants the previous version's log is exactly the moment it
	// stops existing. That is not hypothetical: a v1.4.0 production
	// upgrade lost the outgoing plugin's evidence before anyone could
	// read it.
	//
	// Stdout of a managed plugin is captured by dockerd, so it lands in
	// the daemon's log on the HOST filesystem and survives the plugin
	// being removed entirely.
	//
	// Dropping -logfile instead was the obvious-looking fix and is
	// wrong: harness.PluginLog reads that file, and it is the input to
	// the whole-run fault census that gates every integration run
	// (#385). Removing it would delete the suite's only instrument that
	// spans a plugin restart. Both outputs, not one.
	openLogFile := func() error {
		// 0644 and O_NOFOLLOW. Operators do read this file, so it stays
		// world-READABLE -- but a root-written log at 0666 is
		// gratuitous, and re-opening a path on SIGHUP without
		// O_NOFOLLOW means a symlink swapped in between opens decides
		// where root appends. The path is operator-supplied inside a
		// root-owned rootfs, so neither is a privilege boundary; both
		// cost nothing (#708).
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND|unix.O_NOFOLLOW, 0644)
		if err != nil {
			return err
		}
		logFileMu.Lock()
		old := currentLogFd
		currentLogFd = f
		// SetOutput (not a bare `.Out =`) takes logrus's own mutex, which
		// every write also holds: the swap can't race a concurrent log
		// write, and once it returns no writer can still be mid-write on
		// the old fd — making the Close below safe. A direct field
		// assignment had both problems (data race on Out; a SIGHUP close
		// could yank the fd out from under an in-flight write).
		// Rebuilt on every reopen, so a SIGHUP rotation re-points the
		// file half without ever detaching stdout.
		log.StandardLogger().SetOutput(pluginLogWriter(os.Stdout, f))
		logFileMu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		return nil
	}

	if *logFile != "" {
		if err := openLogFile(); err != nil {
			fatalCleanup(err, "Failed to open log file for writing")
		}
		defer closeLogFile()

		// SIGHUP reopens the log file so logrotate (move-then-signal,
		// or copytruncate followed by HUP) doesn't leave us writing
		// into a unlinked or truncated fd. logrotate's `postrotate`
		// is the conventional place to send HUP; this handler matches
		// the common daemon behaviour.
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, unix.SIGHUP)
		go func() {
			for range hup {
				if err := openLogFile(); err != nil {
					log.WithError(err).Warn("Failed to reopen log file on SIGHUP")
				} else {
					log.Info("Reopened log file on SIGHUP")
				}
			}
		}()
	}

	// Each knob is left at zero when unset so plugin.NewPlugin applies
	// the documented default — the defaults live there, not here, and
	// config.json's declared values must match them.
	var opts plugin.Options
	durationEnv := func(name string, into *time.Duration) {
		raw, ok := os.LookupEnv(name)
		if !ok || raw == "" {
			return
		}
		d, perr := time.ParseDuration(raw)
		if perr != nil {
			fatalCleanup(perr, "Failed to parse "+name)
		}
		if d <= 0 {
			fatalCleanup(fmt.Errorf("%s must be positive, got %s", name, raw), "Invalid "+name)
		}
		*into = d
	}
	durationEnv("AWAIT_TIMEOUT", &opts.AwaitTimeout)

	// Request capture (#644). Test instrumentation for regenerating the
	// replay fixtures; declared in config-cover.json only, so reaching
	// this on a shipped plugin means someone set it deliberately.
	// captureHandler warns again on its own, but an operator reading
	// startup rather than steady-state logs should see it here too.
	opts.RequestCaptureDir = os.Getenv("REQUEST_CAPTURE_DIR")

	p, err := plugin.NewPlugin(opts)
	if err != nil {
		fatalCleanup(err, "Failed to create plugin")
	}

	// Optional Prometheus scrape target (#651). /metrics is always on
	// the plugin socket; this opens it on TCP as well. Off unless set,
	// because the plugin runs privileged on the host network namespace
	// — see (*Plugin).ListenMetrics. Bound before the socket server
	// starts so a bad address is a startup failure an operator sees,
	// not a silent absence they discover from a missing dashboard.
	if err := listenMetricsFromEnv(p, os.Getenv("METRICS_ADDR")); err != nil {
		fatalCleanup(err, "Failed to start metrics listener")
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, unix.SIGINT, unix.SIGTERM)

	go func() {
		log.Info("Starting server...")
		// http.Server.Serve returns http.ErrServerClosed on a clean
		// Close — that's the success path on SIGTERM, not a failure.
		// Without this guard the goroutine logs ERROR and os.Exit(1)s
		// while the main goroutine is still finishing its own clean
		// shutdown, racing the exit code to 1.
		if err := p.Listen(*bindSock); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalCleanup(err, "Failed to start plugin")
		}
	}()

	<-sigs
	log.Info("Shutting down...")
	if err := p.Close(); err != nil {
		fatalCleanup(err, "Failed to stop plugin")
	}
}

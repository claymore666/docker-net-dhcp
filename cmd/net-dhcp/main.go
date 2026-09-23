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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/plugin"
)

var (
	logLevel = flag.String("log", "", "log level")
	logFile  = flag.String("logfile", "", "log file")
	bindSock = flag.String("sock", "/run/docker/plugins/net-dhcp.sock", "bind unix socket")
)

func main() {
	flag.Parse()

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
	// log.Fatal exits without running defers, so under -logfile its last line could stay unflushed.
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

	// The log goes to the file and to stdout, for the reasons on pluginLogWriter (#420).
	openLogFile := func() error {
		// 0644 because operators read it; O_NOFOLLOW so a symlink swapped in before a SIGHUP reopen does not decide
		// where root appends (#708).
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND|unix.O_NOFOLLOW, 0644)
		if err != nil {
			return err
		}
		logFileMu.Lock()
		old := currentLogFd
		currentLogFd = f
		// SetOutput takes the mutex every logrus write holds, so no write is still on old when it is closed (#330).
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

		// SIGHUP reopens the file after logrotate moves or copytruncates it and signals from postrotate.
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

	// An unset knob stays zero so plugin.NewPlugin applies its default, which config.json must declare.
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

	// Replay-fixture capture, declared in config-cover.json only (#644).
	opts.RequestCaptureDir = os.Getenv("REQUEST_CAPTURE_DIR")

	p, err := plugin.NewPlugin(opts)
	if err != nil {
		fatalCleanup(err, "Failed to create plugin")
	}

	if err := listenMetricsFromEnv(p, os.Getenv("METRICS_ADDR")); err != nil {
		fatalCleanup(err, "Failed to start metrics listener")
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, unix.SIGINT, unix.SIGTERM)

	go func() {
		log.Info("Starting server...")
		// Serve returns http.ErrServerClosed on a clean Close, the SIGTERM path (#71).
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

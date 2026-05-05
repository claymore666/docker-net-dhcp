package main

import (
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/devplayer0/docker-net-dhcp/pkg/plugin"
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

	openLogFile := func() error {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			return err
		}
		logFileMu.Lock()
		old := currentLogFd
		currentLogFd = f
		log.StandardLogger().Out = f
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

	awaitTimeout := 10 * time.Second // matches config.json default
	if t, ok := os.LookupEnv("AWAIT_TIMEOUT"); ok {
		awaitTimeout, err = time.ParseDuration(t)
		if err != nil {
			fatalCleanup(err, "Failed to parse await timeout")
		}
	}

	p, err := plugin.NewPlugin(awaitTimeout)
	if err != nil {
		fatalCleanup(err, "Failed to create plugin")
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

// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// nfs-watchdog pets the SoC hardware watchdog only while the root
// filesystem still answers, so an arm64 CI host whose NFS server
// disappears resets itself instead of wedging (#632).
//
// WHY SYSTEMD'S WATCHDOG IS NOT ENOUGH
//
// The OS image already arms this device: it ships
// /usr/lib/systemd/system.conf.d/40-rpi-enable-watchdog.conf with
// RuntimeWatchdogSec=1m, and PID 1 holds /dev/watchdog0 with a 60s
// hardware timeout. It was armed during the outage that motivated this
// and the board still had to be power-cycled.
//
// The reason is the failure itself. systemd is resident in memory and
// its event loop never touches the root filesystem, so it keeps petting
// while every process that needs I/O blocks forever on a hard mount.
// From the watchdog's point of view the board is healthy — which is
// exactly why such a host answers ping, accepts TCP on 22, and never
// produces an ssh banner: sshd cannot re-exec its own binary off the
// share that just went away.
//
// So the petting has to be conditional on I/O that actually reaches the
// server. That is the whole idea here.
//
// # HOW A BLOCKED PROBE IS THE SIGNAL, NOT A PROBLEM
//
// The probe runs in its own goroutine and publishes a timestamp; the
// petting loop reads that timestamp and never calls into the filesystem
// itself. On a hard NFS mount a probe does not fail, it blocks
// indefinitely — so a stuck probe simply stops refreshing the timestamp
// and looks identical to a failing one. That is correct, and it is why
// there is no timeout plumbed into the probe call: the staleness
// deadline IS the timeout.
//
// # WHY statfs
//
// It issues an FSSTAT RPC, so it reaches the server. Reading a file
// would not: the page cache answers from RAM long after the server is
// gone, which would keep the watchdog fed through the exact outage it
// is meant to catch.
//
// # WHY THE PROCESS PINS ITS OWN MEMORY
//
// Clean file-backed pages stay evictable during the outage, and faulting
// them back in blocks on the dead share. A petter that gets paged out is
// a petter that stops petting for the wrong reason — and, worse, one
// that would have reset a healthy box. mlockall keeps this process whole.
//
// WHY IT LOGS TO /dev/kmsg
//
// journald can block a writer when its buffers fill, and its own storage
// is on the share. The kernel ring buffer is memory and never blocks.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Watchdog ioctls. The magic-close byte 'V' is written before a
// deliberate close so stopping this service does not arm a reset.
const magicClose = "V"

type config struct {
	device        string
	probePath     string
	petInterval   time.Duration
	probeInterval time.Duration
	staleAfter    time.Duration
	hwTimeout     time.Duration
}

// validate refuses a configuration that cannot do its job, rather than
// running a watchdog whose numbers quietly make it a no-op or a
// hair-trigger. Each of these has a failure direction worth naming.
func (c config) validate() error {
	if c.staleAfter <= 0 || c.petInterval <= 0 || c.probeInterval <= 0 {
		return errors.New("pet-interval, probe-interval and stale-after must all be positive")
	}
	// The hardware bites hwTimeout after the last pet. If we tolerate
	// staleness for longer than that, the board resets while we still
	// consider the filesystem healthy — the reset would be real but our
	// reason for it would be a lie, and the log would say nothing.
	if c.staleAfter >= c.hwTimeout {
		return fmt.Errorf("stale-after (%s) must be shorter than the hardware timeout (%s), "+
			"otherwise the board resets before this process ever decides anything", c.staleAfter, c.hwTimeout)
	}
	// A probe that runs less often than we tolerate staleness can never
	// refresh the timestamp in time: the watchdog would fire on a
	// perfectly healthy host.
	if c.probeInterval >= c.staleAfter {
		return fmt.Errorf("probe-interval (%s) must be shorter than stale-after (%s), "+
			"otherwise a healthy host still goes stale between probes", c.probeInterval, c.staleAfter)
	}
	// Same argument one level down: pet at least twice per hardware
	// timeout so a single missed tick is not a reset.
	if c.petInterval*2 >= c.hwTimeout {
		return fmt.Errorf("pet-interval (%s) must be under half the hardware timeout (%s), "+
			"otherwise one missed tick resets the board", c.petInterval, c.hwTimeout)
	}
	return nil
}

// shouldPet is the whole decision, kept separate from the clock and the
// device so it can be tested directly.
func shouldPet(now, lastGood time.Time, staleAfter time.Duration) bool {
	// Not redundant, though a mutation test cannot tell: the zero Time
	// would also fall out of the comparison below, but only because
	// time.Sub clamps an overflowing difference to the maximum Duration.
	// Relying on that would make "a host that has never had a working
	// root must not be petted" an accident of arithmetic rather than a
	// decision. TestShouldPet pins the behaviour either way.
	if lastGood.IsZero() {
		return false
	}
	return now.Sub(lastGood) <= staleAfter
}

// prober republishes "the filesystem answered at T" forever. It never
// returns; a blocked statfs simply stops it publishing, which is the
// signal.
type prober struct {
	path     string
	interval time.Duration
	statfs   func(string) error // seam for tests
	last     atomic.Int64       // UnixNano of the last successful probe
}

func (p *prober) lastGood() time.Time {
	ns := p.last.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (p *prober) run(stop <-chan struct{}) {
	for {
		if err := p.statfs(p.path); err == nil {
			p.last.Store(time.Now().UnixNano())
		}
		select {
		case <-stop:
			return
		case <-time.After(p.interval):
		}
	}
}

func statfsProbe(path string) error {
	var st syscall.Statfs_t
	return syscall.Statfs(path, &st)
}

// hwTimeoutFromSysfs reads the timeout the device is actually running
// with. Asking the kernel beats configuring a number and hoping: the
// driver may clamp what it is given, and every safety margin below is
// only meaningful against the real value. Returns 0 when it cannot be
// read, and the caller falls back to the configured one.
func hwTimeoutFromSysfs(device string) time.Duration {
	name := filepath.Base(device)
	b, err := os.ReadFile(filepath.Join("/sys/class/watchdog", name, "timeout"))
	if err != nil {
		return 0
	}
	secs, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// kmsg is the log sink: the kernel ring buffer is memory, so it cannot
// block on the filesystem this process exists to distrust. Falls back to
// stderr when /dev/kmsg is not writable (a container, a test).
func openKmsg() io.Writer {
	f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
	if err != nil {
		return os.Stderr
	}
	return f
}

type watchdog struct {
	f   *os.File
	log io.Writer
}

func (w *watchdog) pet() error {
	_, err := w.f.Write([]byte{0})
	return err
}

// disarm writes the magic-close byte so a deliberate stop does not leave
// a timer running. Without it, stopping this service to debug something
// would reset the box a minute later.
func (w *watchdog) disarm() {
	_, _ = w.f.WriteString(magicClose)
	_ = w.f.Close()
}

func main() {
	var c config
	flag.StringVar(&c.device, "device", envOr("WATCHDOG_DEVICE", "/dev/watchdog0"), "watchdog character device")
	flag.StringVar(&c.probePath, "probe-path", envOr("PROBE_PATH", "/"), "path on the filesystem whose reachability gates petting")
	flag.DurationVar(&c.petInterval, "pet-interval", durOr("PET_INTERVAL", 10*time.Second), "how often to pet while healthy")
	flag.DurationVar(&c.probeInterval, "probe-interval", durOr("PROBE_INTERVAL", 10*time.Second), "how often to probe the filesystem")
	flag.DurationVar(&c.staleAfter, "stale-after", durOr("STALE_AFTER", 45*time.Second), "stop petting once the last successful probe is older than this")
	flag.DurationVar(&c.hwTimeout, "hw-timeout", durOr("HW_TIMEOUT", 60*time.Second), "assumed hardware timeout; overridden by the device's own value when sysfs reports one")
	flag.Parse()

	log := openKmsg()
	logf := func(format string, a ...any) {
		fmt.Fprintf(log, "nfs-watchdog: "+format+"\n", a...)
	}

	// Validate against the timeout the device really has, not the one
	// we were told to assume.
	if real := hwTimeoutFromSysfs(c.device); real > 0 && real != c.hwTimeout {
		logf("device reports a %s hardware timeout (configured %s); using the device's", real, c.hwTimeout)
		c.hwTimeout = real
	}
	if err := c.validate(); err != nil {
		logf("FATAL: %v", err)
		os.Exit(2)
	}

	// Pin every page before touching the device. If this fails we are
	// not in a position to promise the thing this program is for, so it
	// is fatal rather than a warning: a petter that can be paged out
	// would eventually reset a healthy machine.
	if err := syscall.Mlockall(syscall.MCL_CURRENT | syscall.MCL_FUTURE); err != nil {
		logf("FATAL: mlockall: %v — refusing to run unpinned, this process would "+
			"block on the same share it is watching and reset a healthy host", err)
		os.Exit(2)
	}

	f, err := os.OpenFile(c.device, os.O_WRONLY, 0)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			logf("FATAL: %s is busy — systemd still owns it. Set RuntimeWatchdogSec=0 "+
				"so this service can take the device; only one process may hold it.", c.device)
			os.Exit(2)
		}
		logf("FATAL: opening %s: %v", c.device, err)
		os.Exit(2)
	}
	w := &watchdog{f: f, log: log}

	p := &prober{path: c.probePath, interval: c.probeInterval, statfs: statfsProbe}
	stop := make(chan struct{})
	go p.run(stop)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	logf("watching %s via statfs every %s; petting %s every %s; stop petting after %s without a successful probe",
		c.probePath, c.probeInterval, c.device, c.petInterval, c.staleAfter)

	run(w, p, c, sig, stop, logf)
}

// run is the petting loop, separated from main so a test can drive it
// with a fake device and a fake clock source.
func run(w *watchdog, p *prober, c config, sig <-chan os.Signal, stop chan struct{}, logf func(string, ...any)) {
	tick := time.NewTicker(c.petInterval)
	defer tick.Stop()

	starving := false
	for {
		select {
		case <-sig:
			close(stop)
			logf("stopping on signal; disarming the watchdog so this does not reset the host")
			w.disarm()
			return
		case now := <-tick.C:
			last := p.lastGood()
			if shouldPet(now, last, c.staleAfter) {
				if starving {
					// Said out loud because the alternative is a host
					// that came within seconds of a reset and nothing
					// anywhere records that it happened.
					logf("filesystem answering again after %s of silence; resuming", now.Sub(last).Round(time.Second))
					starving = false
				}
				if err := w.pet(); err != nil {
					logf("WARNING: writing to the watchdog failed: %v", err)
				}
				continue
			}
			if !starving {
				starving = true
				age := "never"
				if !last.IsZero() {
					age = now.Sub(last).Round(time.Second).String()
				}
				logf("NOT petting: last successful probe of %s was %s ago (limit %s). "+
					"The hardware will reset this host shortly — that is deliberate.",
					c.probePath, age, c.staleAfter)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// nfs-watchdog pets the SoC hardware watchdog only while the root filesystem still answers, so an arm64 CI host
// whose NFS server disappears resets itself instead of wedging (#632).
package main

// systemd already pets /dev/watchdog0 (RuntimeWatchdogSec=1m, which the kernel clamps to this SoC's 15s), but it is
// resident and never touches the root filesystem, so it keeps petting while every process blocks on the hard mount:
// the host answers ping and TCP 22 and never sends an ssh banner. A probe blocked on the mount stops refreshing the
// timestamp the petting loop reads, so the staleness deadline is the probe's timeout. Logs go to /dev/kmsg because
// journald can block a writer and stores on the share (#632).

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

// magicClose written before close makes the kernel stop the timer.
const magicClose = "V"

type config struct {
	device        string
	probePath     string
	petInterval   time.Duration
	probeInterval time.Duration
	staleAfter    time.Duration
	hwTimeout     time.Duration
}

// validate refuses timings that would make the watchdog a no-op or a hair-trigger.
func (c config) validate() error {
	if c.staleAfter <= 0 || c.petInterval <= 0 || c.probeInterval <= 0 {
		return errors.New("pet-interval, probe-interval and stale-after must all be positive")
	}
	if c.staleAfter >= c.hwTimeout {
		return fmt.Errorf("stale-after (%s) must be shorter than the hardware timeout (%s), "+
			"otherwise the board resets before this process ever decides anything", c.staleAfter, c.hwTimeout)
	}
	if c.probeInterval >= c.staleAfter {
		return fmt.Errorf("probe-interval (%s) must be shorter than stale-after (%s), "+
			"otherwise a healthy host still goes stale between probes", c.probeInterval, c.staleAfter)
	}
	if c.petInterval*2 >= c.hwTimeout {
		return fmt.Errorf("pet-interval (%s) must be under half the hardware timeout (%s), "+
			"otherwise one missed tick resets the board", c.petInterval, c.hwTimeout)
	}
	return nil
}

// The BCM2835 on the arm64 CI board caps at 15s and clamps silently, so the 60s defaults all failed validate, the
// process exited after PID 1 had handed the device over, and the board ran unwatched (#661). Pet at a fifth of the
// timeout and go stale at three fifths: 3s/3s/9s on that board, proven by hand. An explicit value is never moved.

// fitToHardware rescales the timings the operator did not set to fit the device's timeout.
func fitToHardware(c config, explicit map[string]bool) (config, []string) {
	if c.validate() == nil {
		return c, nil
	}
	pet, probe, stale := c.hwTimeout/5, c.hwTimeout/5, c.hwTimeout*3/5
	var changed []string
	if !explicit["pet-interval"] && c.petInterval != pet {
		c.petInterval = pet
		changed = append(changed, "pet-interval")
	}
	if !explicit["probe-interval"] && c.probeInterval != probe {
		c.probeInterval = probe
		changed = append(changed, "probe-interval")
	}
	if !explicit["stale-after"] && c.staleAfter != stale {
		c.staleAfter = stale
		changed = append(changed, "stale-after")
	}
	return c, changed
}

// shouldPet is the petting decision, separate from the clock and the device.
func shouldPet(now, lastGood time.Time, staleAfter time.Duration) bool {
	// Explicit: the zero time also fails below, but only because time.Sub clamps an overflow to the maximum Duration.
	if lastGood.IsZero() {
		return false
	}
	return now.Sub(lastGood) <= staleAfter
}

// prober publishes the time of the last successful probe; a blocked statfs stops publishing, which is the signal.
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

// statfs sends an FSSTAT RPC to the server; a file read is answered from the page cache after the server is gone
// (#632).
func statfsProbe(path string) error {
	var st syscall.Statfs_t
	return syscall.Statfs(path, &st)
}

// hwTimeoutFromSysfs reads the timeout the driver actually runs with, since it may clamp the configured one, or 0.
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

// openKmsg opens the kernel ring buffer as the log sink, or stderr where it is not writable.
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

// disarm writes the magic-close byte and closes, stopping the timer; only correct while the filesystem answers.
func (w *watchdog) disarm() {
	_, _ = w.f.WriteString(magicClose)
	_ = w.f.Close()
}

// release closes without the magic byte, which the watchdog core treats as unexpected: the timer keeps running and
// the board resets one hardware timeout later (#684).
func (w *watchdog) release() {
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

	if real := hwTimeoutFromSysfs(c.device); real > 0 && real != c.hwTimeout {
		logf("device reports a %s hardware timeout (configured %s); using the device's", real, c.hwTimeout)
		c.hwTimeout = real
	}
	if fitted, changed := fitToHardware(c, explicitTimings()); len(changed) > 0 {
		logf("the default %s do not fit a %s hardware timeout; scaled to %s/%s/%s "+
			"(pet/probe/stale) rather than refusing to run and leaving the board unwatched",
			strings.Join(changed, ", "), c.hwTimeout,
			fitted.petInterval, fitted.probeInterval, fitted.staleAfter)
		c = fitted
	}
	if err := c.validate(); err != nil {
		logf("FATAL: %v", err)
		os.Exit(2)
	}

	// Clean file-backed pages stay evictable and faulting them back blocks on the dead share, so an unpinned petter
	// could stop petting a healthy host (#632).
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

// run is the petting loop on a real ticker, separated from main so a test can drive it with a fake device.
func run(w *watchdog, p *prober, c config, sig <-chan os.Signal, stop chan struct{}, logf func(string, ...any)) {
	tick := time.NewTicker(c.petInterval)
	defer tick.Stop()
	petLoop(w, p, c, sig, stop, tick.C, logf)
}

// petLoop decides once per tick, taking the time from the tick itself.
func petLoop(w *watchdog, p *prober, c config, sig <-chan os.Signal, stop chan struct{}, ticks <-chan time.Time, logf func(string, ...any)) {
	starving := false
	for {
		select {
		case <-sig:
			close(stop)
			// systemd sends SIGTERM on an operator stop and on shutdown; a shutdown blocks on a dead share, so the timer
			// stays armed unless the filesystem still answers (#684).
			last := p.lastGood()
			if shouldPet(time.Now(), last, c.staleAfter) {
				logf("stopping on signal with %s still answering; disarming so this does not reset the host", c.probePath)
				w.disarm()
				return
			}
			age := "never"
			if !last.IsZero() {
				age = time.Since(last).Round(time.Second).String()
			}
			logf("stopping on signal, but the last successful probe of %s was %s ago (limit %s): "+
				"LEAVING THE WATCHDOG ARMED. A shutdown that blocks on the dead share "+
				"will be ended by the hardware — that is deliberate.", c.probePath, age, c.staleAfter)
			w.release()
			return
		case now := <-ticks:
			last := p.lastGood()
			if shouldPet(now, last, c.staleAfter) {
				if starving {
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

// explicitTimings names the timings set by flag or environment, which fitToHardware leaves alone.
func explicitTimings() map[string]bool {
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for name, env := range map[string]string{
		"pet-interval":   "PET_INTERVAL",
		"probe-interval": "PROBE_INTERVAL",
		"stale-after":    "STALE_AFTER",
	} {
		if _, ok := os.LookupEnv(env); ok {
			explicit[name] = true
		}
	}
	return explicit
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

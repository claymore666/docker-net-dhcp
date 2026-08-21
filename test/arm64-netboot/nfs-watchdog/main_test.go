// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The decision this program exists to make, in both directions.
func TestShouldPet(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, tc := range []struct {
		name       string
		lastGood   time.Time
		staleAfter time.Duration
		want       bool
	}{
		// A host that has never had a successful probe must NOT be
		// petted. Starting up is not evidence the filesystem works, and
		// treating it as such would keep a host alive that never had a
		// working root.
		{"never probed", time.Time{}, 45 * time.Second, false},
		{"just probed", now, 45 * time.Second, true},
		{"within the limit", now.Add(-44 * time.Second), 45 * time.Second, true},
		{"exactly at the limit", now.Add(-45 * time.Second), 45 * time.Second, true},
		{"one second over", now.Add(-46 * time.Second), 45 * time.Second, false},
		{"long gone", now.Add(-10 * time.Minute), 45 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldPet(now, tc.lastGood, tc.staleAfter); got != tc.want {
				t.Fatalf("shouldPet(last=%v, stale=%v) = %v, want %v", tc.lastGood, tc.staleAfter, got, tc.want)
			}
		})
	}
}

// Every rejected configuration is one that would make this a no-op or a
// hair-trigger, which is worse than not running it at all.
func TestConfigValidate(t *testing.T) {
	ok := config{
		petInterval:   10 * time.Second,
		probeInterval: 10 * time.Second,
		staleAfter:    45 * time.Second,
		hwTimeout:     60 * time.Second,
	}
	if err := ok.validate(); err != nil {
		t.Fatalf("the shipped defaults must validate: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*config)
		want string
	}{
		{
			// The board would reset while we still believed the
			// filesystem was fine, so nothing would ever log a reason.
			"stale-after at or over the hardware timeout",
			func(c *config) { c.staleAfter = 60 * time.Second },
			"shorter than the hardware timeout",
		},
		{
			// A healthy host goes stale between probes and resets.
			"probe-interval at or over stale-after",
			func(c *config) { c.probeInterval = 45 * time.Second },
			"shorter than stale-after",
		},
		{
			// One missed tick would be a reset.
			"pet-interval over half the hardware timeout",
			func(c *config) { c.petInterval = 31 * time.Second },
			"under half the hardware timeout",
		},
		{"zero stale-after", func(c *config) { c.staleAfter = 0 }, "must all be positive"},
		{"zero pet-interval", func(c *config) { c.petInterval = 0 }, "must all be positive"},
		{"zero probe-interval", func(c *config) { c.probeInterval = 0 }, "must all be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.mut(&c)
			err := c.validate()
			if err == nil {
				t.Fatalf("want a rejection mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %q", tc.want, err)
			}
		})
	}
}

// A probe that BLOCKS must look exactly like a probe that fails. On a
// hard NFS mount that is the real shape of an outage — statfs never
// returns — and a design that only handled errors would keep petting
// forever through it.
func TestProber_BlockedProbeGoesStale(t *testing.T) {
	release := make(chan struct{})
	p := &prober{
		path:     "/irrelevant",
		interval: time.Millisecond,
		statfs: func(string) error {
			<-release // never returns until the test says so
			return nil
		},
	}
	stop := make(chan struct{})
	go p.run(stop)
	defer func() { close(release); close(stop) }()

	time.Sleep(20 * time.Millisecond)
	if got := p.lastGood(); !got.IsZero() {
		t.Fatalf("a blocked probe must never publish a timestamp, got %v", got)
	}
	if shouldPet(time.Now(), p.lastGood(), time.Second) {
		t.Fatal("a blocked probe must not keep the watchdog fed")
	}
}

func TestProber_FailingProbeStopsPublishing(t *testing.T) {
	var fail atomic.Bool
	p := &prober{
		path:     "/irrelevant",
		interval: time.Millisecond,
		statfs: func(string) error {
			if fail.Load() {
				return errors.New("stale file handle")
			}
			return nil
		},
	}
	stop := make(chan struct{})
	go p.run(stop)
	defer close(stop)

	deadline := time.Now().Add(2 * time.Second)
	for p.lastGood().IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("a succeeding probe never published a timestamp")
		}
		time.Sleep(time.Millisecond)
	}

	fail.Store(true)
	frozen := p.lastGood()
	time.Sleep(50 * time.Millisecond)
	if got := p.lastGood(); !got.Equal(frozen) {
		t.Fatalf("a failing probe must not refresh the timestamp: %v -> %v", frozen, got)
	}
}

// statfs, not a file read: the page cache answers a read from RAM long
// after the server is gone, which would feed the watchdog straight
// through the outage it exists to catch. Asserted against a real path so
// the call is the real one.
func TestStatfsProbe(t *testing.T) {
	if err := statfsProbe(t.TempDir()); err != nil {
		t.Fatalf("statfs on a real directory must succeed: %v", err)
	}
	if err := statfsProbe("/definitely/not/here"); err == nil {
		t.Fatal("statfs on a missing path must fail, otherwise the probe proves nothing")
	}
}

// End to end against a file standing in for the device: petting starts,
// and STOPS once the probe goes stale.
func TestRun_StopsPettingWhenTheProbeGoesStale(t *testing.T) {
	dev, err := os.CreateTemp(t.TempDir(), "watchdog")
	if err != nil {
		t.Fatal(err)
	}
	w := &watchdog{f: dev}

	p := &prober{path: "/irrelevant", interval: time.Hour, statfs: statfsProbe}
	p.last.Store(time.Now().UnixNano()) // healthy to begin with

	c := config{petInterval: 2 * time.Millisecond, probeInterval: time.Millisecond,
		staleAfter: 40 * time.Millisecond, hwTimeout: time.Second}

	sig := make(chan os.Signal, 1)
	stop := make(chan struct{})
	var logged []string
	done := make(chan struct{})
	go func() {
		run(w, p, c, sig, stop, func(f string, a ...any) { logged = append(logged, f) })
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	petsWhileHealthy := size(t, dev.Name())
	if petsWhileHealthy == 0 {
		t.Fatal("nothing was written to the device while the filesystem was healthy")
	}

	// The prober's interval is an hour, so the timestamp now ages out
	// and nothing refreshes it — the outage, simulated honestly.
	time.Sleep(80 * time.Millisecond)
	petsAfterStale := size(t, dev.Name())

	time.Sleep(40 * time.Millisecond)
	if got := size(t, dev.Name()); got != petsAfterStale {
		t.Fatalf("the watchdog was still being petted after the probe went stale: %d -> %d", petsAfterStale, got)
	}

	sig <- os.Interrupt
	<-done

	// Stopping while the share is silent must NOT disarm. This is the
	// shutdown path: systemd stops units before it unmounts, so the
	// SIGTERM that ends this process arrives BEFORE the unmount that is
	// going to hang. Disarming here removes the only thing left that
	// could end that hang, which is what the board did for 14 minutes
	// on 2026-08-20.
	b, err := os.ReadFile(dev.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), magicClose) {
		t.Fatalf("the watchdog was disarmed on a stop taken while the filesystem was already gone; tail is %q", tail(string(b)))
	}

	var sawRefusal bool
	for _, l := range logged {
		if strings.Contains(l, "NOT petting") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatal("stopping to pet must be logged; a silent reset is indistinguishable from a crash")
	}
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func tail(s string) string {
	if len(s) > 8 {
		return s[len(s)-8:]
	}
	return s
}

// The board this program runs on has a 15s hardware watchdog, while the
// defaults describe a 60s one. Before fitToHardware that combination
// was fatal: the process refused to start, and because PID 1 had
// already released the device the board then ran unwatched. Refusing to
// run is the one outcome a watchdog must never choose, so each way back
// into it is driven here.
func TestFitToHardware(t *testing.T) {
	defaults := func(hw time.Duration) config {
		return config{
			petInterval:   10 * time.Second,
			probeInterval: 10 * time.Second,
			staleAfter:    45 * time.Second,
			hwTimeout:     hw,
		}
	}

	t.Run("a 60s device keeps the tuned defaults", func(t *testing.T) {
		c := defaults(60 * time.Second)
		got, changed := fitToHardware(c, nil)
		if len(changed) != 0 {
			t.Fatalf("rescaled a config that already fits: %v", changed)
		}
		if got != c {
			t.Fatalf("config changed: %+v -> %+v", c, got)
		}
	})

	t.Run("a 15s device is scaled instead of rejected", func(t *testing.T) {
		got, changed := fitToHardware(defaults(15*time.Second), nil)
		if err := got.validate(); err != nil {
			t.Fatalf("scaled config still invalid: %v", err)
		}
		if len(changed) != 3 {
			t.Fatalf("want all three timings scaled, got %v", changed)
		}
		// The values proven by hand on the board.
		if got.petInterval != 3*time.Second ||
			got.probeInterval != 3*time.Second ||
			got.staleAfter != 9*time.Second {
			t.Fatalf("want 3s/3s/9s, got %s/%s/%s",
				got.petInterval, got.probeInterval, got.staleAfter)
		}
	})

	t.Run("an explicit timing is never silently overridden", func(t *testing.T) {
		explicit := map[string]bool{"stale-after": true}
		got, changed := fitToHardware(defaults(15*time.Second), explicit)
		if got.staleAfter != 45*time.Second {
			t.Fatalf("overrode an operator's own number: stale-after = %s", got.staleAfter)
		}
		for _, name := range changed {
			if name == "stale-after" {
				t.Fatal("reported scaling a value it was told not to touch")
			}
		}
		// And the contradiction still has to surface rather than being
		// papered over by the values around it.
		if err := got.validate(); err == nil {
			t.Fatal("an unusable explicit stale-after validated clean")
		}
	})

	t.Run("the scaled timings validate across the plausible range", func(t *testing.T) {
		// Whatever device this lands on next, the ratios have to hold.
		for hw := 5 * time.Second; hw <= 120*time.Second; hw += time.Second {
			got, _ := fitToHardware(defaults(hw), nil)
			if err := got.validate(); err != nil {
				t.Fatalf("hw=%s scaled to %s/%s/%s which is invalid: %v",
					hw, got.petInterval, got.probeInterval, got.staleAfter, err)
			}
		}
	})

	t.Run("a hardware timeout too small to be usable stays fatal", func(t *testing.T) {
		// Scaling must not manufacture a zero or sub-second petter and
		// call it healthy: below a usable timeout the honest answer is
		// still the error.
		got, _ := fitToHardware(defaults(2*time.Second), nil)
		if got.petInterval <= 0 || got.probeInterval <= 0 || got.staleAfter <= 0 {
			if err := got.validate(); err == nil {
				t.Fatal("a degenerate scaling validated clean")
			}
		}
	})
}

// The stop path has to tell two identical SIGTERMs apart, and it gets
// exactly one piece of evidence: whether the filesystem still answers.
// Both directions are driven here because they fail in opposite ways —
// disarming on the shutdown path leaves a wedged board nothing can end,
// and staying armed on an operator's stop resets a healthy host seconds
// after somebody deliberately stopped the service to look at it.
func TestRun_DisarmsOnlyWhileTheFilesystemAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lastGood   time.Duration // age of the last successful probe
		wantDisarm bool
	}{
		{"an operator stopping the service on a healthy board disarms", 0, true},
		{"a stop taken while the share is silent stays armed", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, err := os.CreateTemp(t.TempDir(), "watchdog")
			if err != nil {
				t.Fatal(err)
			}
			w := &watchdog{f: dev}

			p := &prober{path: "/irrelevant", interval: time.Hour, statfs: statfsProbe}
			p.last.Store(time.Now().Add(-tc.lastGood).UnixNano())

			c := config{petInterval: time.Hour, probeInterval: time.Millisecond,
				staleAfter: 40 * time.Millisecond, hwTimeout: time.Second,
				probePath: "/irrelevant"}

			sig := make(chan os.Signal, 1)
			sig <- os.Interrupt
			run(w, p, c, sig, make(chan struct{}), func(string, ...any) {})

			b, err := os.ReadFile(dev.Name())
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(b), magicClose); got != tc.wantDisarm {
				t.Fatalf("disarmed=%v, want %v (device holds %q)", got, tc.wantDisarm, string(b))
			}
		})
	}
}

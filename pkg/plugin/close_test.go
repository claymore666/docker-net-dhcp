package plugin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Close's shutdown ordering (#338 item 2). Before this, Close called
// server.Close, which by contract returns WITHOUT waiting for in-flight
// handlers, so a Join already past the listener could register a
// manager after the registry had been drained. That was mitigated with
// a second speculative sweep; these tests pin the real guarantee that
// replaced it — Shutdown waits for handlers to return, and because
// registerDHCPManager runs synchronously inside the Join handler, "no
// handler running" means "the registry is final".

// shrinkShutdownTimeout shortens the whole-shutdown budget for a single
// test and restores it afterwards. The forced and timeout paths are
// only reachable by letting the budget expire.
func shrinkShutdownTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := pluginShutdownTimeout
	pluginShutdownTimeout = d
	t.Cleanup(func() { pluginShutdownTimeout = prev })
}

// instantManager returns a manager whose Stop is a no-op: Stop waits on
// startedCh and short-circuits when startErr is set, so a closed
// channel plus a recorded failure means it never touches dhcpcd or a
// netlink handle.
func instantManager() *dhcpManager {
	m := &dhcpManager{startedCh: make(chan struct{}), startErr: errors.New("start failed")}
	close(m.startedCh)
	return m
}

// servedPlugin wires a plugin's HTTP server to handler and serves it on
// a unix socket, returning a client bound to that socket. Mirrors what
// Listen does, minus the blocking Serve.
func servedPlugin(t *testing.T, p *Plugin, handler http.Handler) *http.Client {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "test.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.server = http.Server{Handler: handler}
	go func() { _ = p.server.Serve(l) }()

	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

func TestClose_WaitsForInFlightHandlerBeforeDraining(t *testing.T) {
	// The regression this pins: a handler that is mid-Join when
	// shutdown starts must finish, and the manager it registers must
	// still be swept. With server.Close the handler kept running past
	// both sweeps and its manager was left behind — a live dhcpcd with
	// no owner, holding a lease nobody would ever release.
	p := newTestPlugin(t)
	p.docker = &fakeDocker{}

	handlerEntered := make(chan struct{})
	release := make(chan struct{})
	client := servedPlugin(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-release
		// Stands in for Join's synchronous registration.
		p.registerDHCPManager("late-endpoint", instantManager())
		w.WriteHeader(http.StatusOK)
	}))

	go func() {
		resp, err := client.Post("http://unix/NetworkDriver.Join", "application/json", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-handlerEntered

	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()

	// Close must still be blocked: the handler has not returned.
	select {
	case err := <-closed:
		t.Fatalf("Close returned while a handler was still in flight (err=%v) — the registry it drained was not final", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the handler finished")
	}

	p.mu.Lock()
	left := len(p.persistentDHCP)
	p.mu.Unlock()
	if left != 0 {
		t.Errorf("%d manager(s) left in the registry after Close; the late registration was not swept", left)
	}
}

func TestClose_WedgedHandlerStillBoundsShutdown(t *testing.T) {
	// The graceful guarantee has a limit, and the limit is the point:
	// a handler that never returns must not hold shutdown open. Close
	// falls back to forcing connections closed and re-sweeping.
	shrinkShutdownTimeout(t, 200*time.Millisecond)

	p := newTestPlugin(t)
	p.docker = &fakeDocker{}

	handlerEntered := make(chan struct{})
	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	client := servedPlugin(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-wedged
	}))

	go func() {
		resp, err := client.Post("http://unix/NetworkDriver.Join", "application/json", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-handlerEntered

	start := time.Now()
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()

	select {
	case err := <-closed:
		// Forcing the listener closed after a graceful attempt is the
		// expected outcome, not an error to report upward.
		if err != nil {
			t.Errorf("Close on the forced path returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close never returned with a wedged handler — the shutdown budget is not bounding it")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Close took %v against a 200ms budget", elapsed)
	}
}

func TestClose_PhasesShareOneBudget(t *testing.T) {
	// Three phases now wait on a deadline (HTTP grace, client fan-out,
	// displaced-stop drain). They share ONE budget deliberately: a
	// per-phase timeout would multiply the wall-clock an operator sits
	// through on `docker plugin disable` every time a phase is added.
	const budget = 300 * time.Millisecond
	shrinkShutdownTimeout(t, budget)

	p := newTestPlugin(t)
	p.docker = &fakeDocker{}

	// Wedge the HTTP phase.
	handlerEntered := make(chan struct{})
	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	client := servedPlugin(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-wedged
	}))
	go func() {
		resp, err := client.Post("http://unix/NetworkDriver.Join", "application/json", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-handlerEntered

	// And wedge the displaced-stop phase.
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	p.displacedStops.Add(1)
	go func() {
		defer p.displacedStops.Done()
		<-stuck
	}()

	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)

	// Two wedged phases against one budget: comfortably under what
	// per-phase timeouts would cost, with slack for a loaded runner.
	if elapsed > 4*budget {
		t.Errorf("Close took %v with a %v budget — phases look like they are each getting their own timeout", elapsed, budget)
	}
}

func TestClose_DrainsDisplacedManagerStops(t *testing.T) {
	// #338 item 3. Join stops a displaced manager in a goroutine so it
	// doesn't block on the dhcpcd release cycle. Close has to account
	// for those: cutting one short at process exit means no
	// DHCPRELEASE, and the server holds a phantom lease against that
	// MAC until it expires on its own.
	p := newTestPlugin(t)
	p.docker = &fakeDocker{}

	releasing := make(chan struct{})
	stopped := make(chan struct{})
	p.displacedStops.Add(1)
	go func() {
		defer p.displacedStops.Done()
		<-releasing
		close(stopped)
	}()

	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()

	select {
	case <-closed:
		t.Fatal("Close returned while a displaced manager was still releasing its lease")
	case <-time.After(150 * time.Millisecond):
	}

	close(releasing)
	<-stopped

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the displaced stop finished")
	}
}

func TestWaitBounded(t *testing.T) {
	var done sync.WaitGroup
	if !waitBounded(&done, time.Second) {
		t.Error("waitBounded on an empty group reported a timeout")
	}

	var blocked sync.WaitGroup
	blocked.Add(1)
	t.Cleanup(blocked.Done)
	if waitBounded(&blocked, 50*time.Millisecond) {
		t.Error("waitBounded reported completion for a group that never finished")
	}
}

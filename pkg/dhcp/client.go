// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

const (
	DefaultHandler = "/usr/lib/net-dhcp/dhcp-handler"
	VendorID       = "docker-net-dhcp"

	// dhcpcdStateDir is dhcpcd's compile-time database directory (DUID +
	// lease files). dhcpcd offers no runtime override and keys files by
	// interface name, so two containers whose container-side link is the
	// default `eth0` would collide on the host-shared directory. Each
	// client therefore runs in a private mount namespace with a tmpfs
	// mounted here (see Start). Identity (DUID/IAID) is pinned via
	// literal config, so a fresh empty state dir is harmless.
	dhcpcdStateDir = "/var/lib/dhcpcd"

	// procSysPath is the kernel sysctl tree dhcpcd writes during
	// interface setup — net/ipv4/conf/<if>/promote_secondaries (v4,
	// fatal if it fails) and net/ipv6/conf/<if>/{autoconf,accept_ra}
	// (v6). It is mounted read-only in the managed-plugin rootfs (and in
	// stock Docker containers, where runc remounts it ro), so those
	// writes returned EROFS and dhcpcd's if_init aborted with a
	// misleading "interface not found", failing every lease (#247).
	// `--noconfigure` does not suppress the writes — dhcpcd does them
	// regardless of observe-only mode. We remount it read-write inside
	// the client's private mount namespace (see mountPrep), which is
	// invisible to the host and to other containers.
	procSysPath = "/proc/sys"

	// dhcpcdRunDir is dhcpcd's compile-time runtime directory: the
	// per-interface pidfile (<iface>-4.pid) and control sockets
	// (<iface>-4.sock etc.). Like the state dir these are keyed by
	// interface name with NO netns component, and /run is shared across
	// the plugin's mount view — but unlike the state dir the collision
	// here is not merely stale data. dhcpcd's startup first tries the
	// per-interface control socket, and if a live instance answers it
	// FORWARDS its argv to that instance and exits 0 without doing any
	// DHCP work. Two containers whose container-side link is the default
	// `eth0` therefore collide deterministically: the second container's
	// persistent client becomes a no-op (lease never renewed or
	// released) and the first container's dhcpcd is reconfigured with
	// the second's config (verified against dhcpcd 10.x: "sending
	// commands to dhcpcd process"). dhcpcd has no flag to suppress the
	// pidfile/control socket, so this dir gets the same private-tmpfs
	// treatment as the state dir (see mountPrep). Alpine's /var/run is a
	// symlink to /run, so this canonical path covers either compile-time
	// spelling.
	dhcpcdRunDir = "/run/dhcpcd"

	// stderrTailMax caps the dhcpcd stderr retained to fold into a
	// non-zero exit error — enough for the operative diagnostic line(s)
	// without unbounded growth from a chatty trace.
	stderrTailMax = 4 << 10
)

// mountPrep is the shell run inside the `unshare -m` mount namespace
// before exec'ing dhcpcd. It (1) shadows the host-shared dhcpcd state
// dir with a private tmpfs (see dhcpcdStateDir), (2) shadows dhcpcd's
// runtime dir the same way so per-interface pidfiles/control sockets
// can't collide across containers (see dhcpcdRunDir — without this the
// second same-named-interface client forwards its argv into the first
// container's dhcpcd and exits without acquiring anything), and
// (3) flips /proc/sys read-write so dhcpcd's interface-setup sysctl
// writes succeed (see procSysPath, #247). All mounts are local to this
// client's mount namespace. Their stderr is swallowed: each can
// legitimately be a no-op (dir already private, /proc/sys already rw)
// or refused (userns-locked mount) — a genuinely blocked sysctl write
// still surfaces via dhcpcd's own stderr, captured into the exit error.
func mountPrep() string {
	return fmt.Sprintf(
		"mount -t tmpfs tmpfs %s 2>/dev/null; "+
			"mkdir -p %s 2>/dev/null; "+
			"mount -t tmpfs tmpfs %s 2>/dev/null; "+
			"mount -o remount,bind,rw %s 2>/dev/null; "+
			"exec \"$0\" \"$@\"",
		dhcpcdStateDir, dhcpcdRunDir, dhcpcdRunDir, procSysPath)
}

// tailWriter retains the last up-to-max bytes written to it. dhcpcd's
// stderr is teed to the debug log and to one of these, so a non-zero
// exit can surface the real cause (e.g. "Read-only file system")
// instead of a bare "exit status 1" (#247). It is written only by the
// os/exec output-copy goroutine and read only after cmd.Wait returns
// (which joins that goroutine), so no locking is needed.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// condense renders the retained stderr as a compact single line: blank
// lines dropped, the rest joined with "; ", suitable for folding into
// an error message.
func (w *tailWriter) condense() string {
	var lines []string
	for _, ln := range strings.Split(string(w.buf), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
	}
	return strings.Join(lines, "; ")
}

// validIfaceName accepts only a kernel-legal network interface name: 1–15
// characters (IFNAMSIZ-1), starting with an alphanumeric (so it can never
// be mistaken for a dhcpcd flag) and otherwise limited to alphanumerics,
// dot, dash and underscore. The interface name originates from the driver
// request and is interpolated into the dhcpcd argv that runs under
// `unshare -m /bin/sh -c '… exec "$0" "$@"'`; validating it here keeps any
// shell-meaningful or flag-shaped value from ever reaching that command
// (go/command-injection, CWE-78). The `"$@"` quoting already prevents
// re-splitting, so this is defence-in-depth, but it is also simply the
// correct contract — these names are never anything but flat tokens.
var validIfaceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,14}$`).MatchString

type DHCPClientOptions struct {
	Hostname string
	V6       bool
	Once     bool
	// NetNS is the network namespace to spawn dhcpcd in, as an OPEN
	// FILE DESCRIPTOR. nil means "spawn in the caller's namespace".
	//
	// This is deliberately not a path. It used to be one, and the
	// caller built it from a container PID; Start then re-resolved that
	// string independently of the caller's own resolution, so the two
	// could land in different namespaces if the PID was recycled in
	// between (#688's hazard, reaching netlink and a root dhcpcd). A
	// descriptor cannot be re-resolved into something else.
	//
	// The handle is BORROWED: Start enters it and never closes it, so
	// its lifetime belongs to whoever opened it.
	NetNS *netns.NsHandle

	// MAC is the endpoint's (pinned) hardware address. It is the sole
	// input to the DUID-LL and IAID pinned in the generated config, so
	// the one-shot (host netns) and persistent (container netns) clients
	// derive an identical identity and the DHCP server returns a single
	// binding (#152).
	MAC net.HardwareAddr

	// RequestedIP, when non-empty, becomes dhcpcd's `request ADDR`
	// (DHCPv4): the client asks for that specific address, the server
	// ACKs if the lease is still valid and otherwise falls back to a
	// fresh offer. Used on plugin-restart recovery and container restart
	// to keep the same lease. v4 only — for v6 use PreferredV6.
	RequestedIP string

	// PreferredV6, when non-empty, becomes the address in dhcpcd's
	// `ia_na <iaid> / ADDR` (DHCPv6): a preferred-address hint in the
	// pinned IA_NA. v6 only.
	PreferredV6 string

	// AllowServers restricts which DHCPv4 servers this client may accept
	// a lease from (dhcpcd `whitelist`). Empty imposes no restriction.
	// The plugin derives it from the network's dhcp_servers preference
	// list; for a tiered acquisition it holds a single tier, and for the
	// persistent client the whole allowed set so renew/rebind can still
	// reach a surviving server (#111).
	AllowServers []string
	// DenyServers rejects specific DHCPv4 servers (dhcpcd `blacklist`).
	// Must be empty whenever AllowServers is set — dhcpcd ignores a
	// blacklist once a whitelist exists (#669).
	DenyServers []string

	// ClientID, when non-empty, is sent as DHCPv4 option 61 (dhcpcd
	// `clientid`), prefixed with the type-0 ("opaque") byte the busybox
	// path used so existing server reservations keyed on it keep
	// matching. v6 identity is carried by DUID+IAID, so this is ignored
	// for v6.
	ClientID []byte

	// VendorClass overrides DHCPv4 option 60 (dhcpcd `vendorclassid`).
	// Empty falls back to the VendorID constant. v4 only.
	VendorClass string

	// Broadcast requests an L2-broadcast reply (ipvlan-L2, where every
	// slave shares the parent MAC). Emitted as the dhcpcd `broadcast`
	// directive (v4 only) — the busybox `-B` equivalent; see renderConfig
	// and #243.
	Broadcast bool

	// FQDN, when non-empty, sets dhcpcd's `fqdn` directive mode (e.g.
	// "both"), making the client send the DHCP FQDN option (81 v4 / 39 v6)
	// built from Hostname and ask the server to register it in DNS (#261).
	// Empty omits it (the default — DDNS registration is opt-in).
	FQDN string

	HandlerScript string
}

// DHCPClient represents a dhcpcd client managing one interface/family.
type DHCPClient struct {
	Opts *DHCPClientOptions

	cmd      *exec.Cmd
	workDir  string      // per-client temp dir: generated config + event FIFO
	fifoRead *os.File    // read end of the event FIFO (scanner side)
	fifoKeep *os.File    // write keep-alive (O_RDWR) end of the event FIFO
	stderr   *tailWriter // last bytes of dhcpcd stderr, for the exit error
	logPipes []io.Closer // logrus WriterLevel pipe writers; closed by the reaper

	waitErr  error
	waitDone chan struct{} // closed when cmd.Wait() returns
}

// NewDHCPClient creates a dhcpcd client for iface. It allocates a
// per-client working directory, generates the dhcpcd config (pinned
// identity + observe-only + the event FIFO) and the event FIFO itself,
// and builds the (mount-namespace-wrapped) command. Start runs it.
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
	if !validIfaceName(iface) {
		return nil, fmt.Errorf("invalid interface name %q", iface)
	}
	handler := opts.HandlerScript
	if handler == "" {
		handler = DefaultHandler
	}
	vendor := opts.VendorClass
	if vendor == "" && !opts.V6 {
		vendor = VendorID
	}

	workDir, err := os.MkdirTemp("", "net-dhcp-dhcpcd-")
	if err != nil {
		return nil, fmt.Errorf("failed to create dhcpcd work dir: %w", err)
	}

	cleanup := func(e error) (*DHCPClient, error) {
		_ = os.RemoveAll(workDir)
		return nil, e
	}

	fifoPath := filepath.Join(workDir, "events")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		return cleanup(fmt.Errorf("failed to create event FIFO: %w", err))
	}

	configPath := filepath.Join(workDir, "dhcpcd.conf")
	params := dhcpcdParams{
		Iface:        iface,
		MAC:          opts.MAC,
		V6:           opts.V6,
		Once:         opts.Once,
		Hostname:     opts.Hostname,
		FQDN:         opts.FQDN,
		VendorClass:  vendor,
		ClientID:     opts.ClientID,
		RequestedIP:  opts.RequestedIP,
		PreferredV6:  opts.PreferredV6,
		Broadcast:    opts.Broadcast,
		AllowServers: opts.AllowServers,
		DenyServers:  opts.DenyServers,
		Handler:      handler,
		ConfigPath:   configPath,
		EventFIFO:    fifoPath,
		// Forward our own GOCOVERDIR to the hook so its coverage counters
		// survive dhcpcd's environment scrub (cover build only; unset and
		// thus omitted in production). See renderConfig.
		CoverDir: os.Getenv("GOCOVERDIR"),
	}
	if err := os.WriteFile(configPath, []byte(renderConfig(params)), 0o600); err != nil {
		return cleanup(fmt.Errorf("failed to write dhcpcd config: %w", err))
	}

	// The FIFO is opened twice, and the order matters. fifoKeep (O_RDWR,
	// opened first so neither open can block) acts as a permanently-open
	// writer: it keeps the FIFO from reporting EOF between the
	// short-lived hook processes that write to it. fifoRead is the
	// scanner's dedicated read end. On process exit the reaper closes
	// ONLY fifoKeep — the scanner then drains whatever is still buffered
	// in the FIFO and terminates on natural EOF. Closing the read end
	// directly instead would discard any not-yet-scanned event, losing
	// the final `bound` under scheduler contention: dhcpcd -1 exits
	// right after the hook writes it, so the reaper's close raced the
	// scanner's read and CreateEndpoint failed with ErrNoLease (#325).
	fifoKeep, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		return cleanup(fmt.Errorf("failed to open event FIFO: %w", err))
	}
	fifoRead, err := os.OpenFile(fifoPath, os.O_RDONLY, 0)
	if err != nil {
		_ = fifoKeep.Close()
		return cleanup(fmt.Errorf("failed to open event FIFO read end: %w", err))
	}

	// dhcpcd has no runtime state-dir override, so isolate per client in
	// a private mount namespace with a tmpfs over the state dir. unshare
	// execs (no fork) so the resulting process IS dhcpcd — signals and
	// Wait target it directly. `sh -c '... exec "$0" "$@"'` passes the
	// dhcpcd argv as $0/$@, avoiding any quoting of paths.
	dargs := renderArgs(params)
	wrapped := append([]string{"unshare", "-m", "/bin/sh", "-c", mountPrep()}, dargs...)

	c := &DHCPClient{
		Opts:     opts,
		cmd:      exec.Command(wrapped[0], wrapped[1:]...),
		workDir:  workDir,
		fifoRead: fifoRead,
		fifoKeep: fifoKeep,
		stderr:   &tailWriter{max: stderrTailMax},
	}
	// dhcpcd's own logs (stdout/stderr) go to logrus at debug level; the
	// structured events come over the FIFO, not these streams. stderr is
	// additionally tee'd into a bounded tail buffer so a non-zero exit
	// can report dhcpcd's real diagnostic, not just "exit status 1" (#247).
	// WriterLevel spawns a goroutine reading an io.Pipe that only exits
	// when the writer is closed — and exec never closes cmd.Stdout/Stderr
	// writers — so the pipes are retained and closed by the reaper (after
	// Wait has joined exec's copy goroutines, so nothing writes to them
	// anymore). Without that close every dhcpcd run leaked two goroutines
	// and pipe pairs for the daemon's lifetime.
	outPipe := log.StandardLogger().WriterLevel(log.DebugLevel)
	errPipe := log.StandardLogger().WriterLevel(log.DebugLevel)
	c.cmd.Stdout = outPipe
	c.cmd.Stderr = io.MultiWriter(errPipe, c.stderr)
	c.logPipes = []io.Closer{outPipe, errPipe}

	log.WithField("cmd", c.cmd.Args).Trace("new dhcpcd client")
	return c, nil
}

// Start starts dhcpcd and returns a channel of lease events read from
// the FIFO. The channel is closed when the dhcpcd process exits (on its
// own for one-shot, or via Finish for the persistent client).
//
// Concurrency contract: when Opts.NetNS is set, Start enters
// the target netns by locking the calling goroutine to its OS thread,
// switching netns, spawning the child (which inherits the netns), and
// switching back. It is *not* re-entrant on the same goroutine.
// Concurrent Starts on *different* goroutines are safe. On netns-restore
// failure the calling thread is deliberately leaked so the wrong-netns
// state never re-enters Go's thread pool.
func (c *DHCPClient) Start() (chan Event, error) {
	if c.Opts.NetNS != nil {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		origNS, err := netns.Get()
		if err != nil {
			return nil, fmt.Errorf("failed to open current network namespace: %w", err)
		}
		defer func() {
			if err := origNS.Close(); err != nil {
				log.WithError(err).Debug("origNS close failed")
			}
		}()

		// Borrowed, so no Close here -- see DHCPClientOptions.NetNS.
		if err := netns.Set(*c.Opts.NetNS); err != nil {
			return nil, fmt.Errorf("failed to enter network namespace: %w", err)
		}

		// Restore the original netns on return. If restoration fails the
		// goroutine is locked to a thread now in the wrong netns; keep it
		// locked (a second Lock so the deferred Unlock doesn't pair) so
		// the thread dies rather than leak the wrong-netns state.
		defer func() {
			if err := netns.Set(origNS); err != nil {
				log.WithError(err).Error("Failed to restore original netns; pinning thread for kill")
				runtime.LockOSThread()
			}
		}()
	}

	if err := c.cmd.Start(); err != nil {
		c.fifoRead.Close()
		c.fifoKeep.Close()
		c.closeLogPipes()
		_ = os.RemoveAll(c.workDir)
		return nil, err
	}

	c.waitDone = make(chan struct{})
	events := make(chan Event, 16)

	// Scanner: read newline-delimited JSON events off the FIFO and hand
	// them downstream. Owns the events channel (and the FIFO read end):
	// once the reaper closes the keep-alive writer after dhcpcd exits,
	// the scanner drains any still-buffered events, hits EOF, and closes
	// both. A full channel drops events rather than blocking the DHCP
	// exchange.
	go func() {
		defer close(events)
		defer c.fifoRead.Close()
		scanner := bufio.NewScanner(c.fifoRead)
		for scanner.Scan() {
			log.WithField("line", string(scanner.Bytes())).Trace("dhcpcd handler line")
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				log.WithError(err).Warn("Failed to decode dhcpcd event")
				continue
			}
			select {
			case events <- event:
			default:
				log.WithField("event", event.Type).Warn("dhcpcd event dropped: consumer slow or finished")
			}
		}
	}()

	// Reaper: the single owner of cmd.Wait(). When dhcpcd exits it closes
	// the FIFO's keep-alive writer — NOT the read end — so the scanner
	// drains any events still buffered in the FIFO before ending on EOF
	// (#325: closing the read end here raced the scanner and could drop
	// the final bound event), and records the exit status for Finish/Wait.
	go func() {
		werr := c.cmd.Wait()
		// Wait has joined the stderr-copy goroutine, so the tail buffer is
		// complete and safe to read here. Fold dhcpcd's own diagnostic into
		// the error so callers see the real cause (#247).
		if werr != nil {
			if tail := c.stderr.condense(); tail != "" {
				werr = fmt.Errorf("%w: %s", werr, tail)
			}
		}
		c.waitErr = werr
		c.fifoKeep.Close()
		c.closeLogPipes()
		close(c.waitDone)
	}()

	return events, nil
}

// closeLogPipes closes the logrus WriterLevel pipe writers so their
// reader goroutines exit. Called once nothing can write to them anymore:
// by the reaper after cmd.Wait, or on a failed Start.
func (c *DHCPClient) closeLogPipes() {
	for _, p := range c.logPipes {
		_ = p.Close()
	}
}

// Finish stops the client and waits for it to exit. For the persistent
// client it sends SIGTERM (dhcpcd releases its lease and exits); the
// one-shot client exits on its own (-1), so Finish only awaits it.
func (c *DHCPClient) Finish(ctx context.Context) error {
	if c.cmd.Process == nil {
		// Start was never called (or its cmd.Start failed): nothing to
		// signal. await handles the not-started case; without this
		// guard the Signal below would nil-panic for persistent
		// clients. No production path hits this today — it hardens the
		// contract await already advertises.
		return c.await(ctx)
	}
	if !c.Opts.Once {
		if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// The process can self-exit between Start and here (lease
			// failure, parent NIC vanished, netns torn down). Treat the
			// "already done" sentinel as success and just reap.
			if !errors.Is(err, os.ErrProcessDone) {
				return fmt.Errorf("failed to send SIGTERM to dhcpcd: %w", err)
			}
			log.WithField("v6", c.Opts.V6).Debug("dhcpcd already exited before SIGTERM; reaping")
		}
	}
	return c.await(ctx)
}

// Wait reaps the dhcpcd process without signalling it. Use when the
// process has already exited on its own (the consumer noticed the event
// channel close). Bounded by ctx so a stuck exit can't block teardown.
func (c *DHCPClient) Wait(ctx context.Context) error {
	return c.await(ctx)
}

// await blocks until the reaper has reaped the process (or ctx fires, in
// which case it kills the process and still drains the reaper), then
// removes the per-client working directory.
func (c *DHCPClient) await(ctx context.Context) error {
	if c.waitDone == nil {
		// Start never ran / failed; nothing to reap.
		_ = os.RemoveAll(c.workDir)
		return nil
	}
	select {
	case <-c.waitDone:
		_ = os.RemoveAll(c.workDir)
		return c.waitErr
	case <-ctx.Done():
		_ = c.cmd.Process.Kill()
		<-c.waitDone
		_ = os.RemoveAll(c.workDir)
		return ctx.Err()
	}
}

// attemptGetIP runs dhcpcd once (opts must already carry Once=true)
// and returns the lease info obtained. GetIP wraps it in a retry loop;
// the indirection through attemptGetIPFunc lets unit tests exercise
// that loop without running a real dhcpcd.
func attemptGetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
	dummy := Info{}

	client, err := NewDHCPClient(iface, opts)
	if err != nil {
		return dummy, fmt.Errorf("failed to create DHCP client: %w", err)
	}

	events, err := client.Start()
	if err != nil {
		return dummy, fmt.Errorf("failed to start DHCP client: %w", err)
	}

	// ch carries the final lease seen, or stays unsent if no bound/renew
	// event arrived before the events channel closed. Buffered=1 so the
	// goroutine never blocks on send.
	ch := make(chan Info, 1)
	go func() {
		var last *Info
		for event := range events {
			if event.Type == "bound" || event.Type == "renew" {
				v := event.Data
				last = &v
			}
		}
		if last != nil {
			ch <- *last
		}
		close(ch)
	}()

	if err := client.Finish(ctx); err != nil {
		return dummy, err
	}

	select {
	case info, ok := <-ch:
		if !ok {
			return dummy, util.ErrNoLease
		}
		return info, nil
	case <-ctx.Done():
		return dummy, ctx.Err()
	}
}

var attemptGetIPFunc = attemptGetIP

// Retry pacing for GetIP. The base delay keeps a failing endpoint from
// hammering the DHCP server; the jitter de-synchronises the many
// one-shot clients a `docker-compose up` starts at once, so their
// retries don't land in lockstep.
const (
	leaseRetryDelay  = 500 * time.Millisecond
	leaseRetryJitter = 250 * time.Millisecond
)

// isRetryableLeaseErr reports whether an attemptGetIP failure is worth
// retrying: dhcpcd ran and the exchange failed (exited zero with no
// lease event, or exited non-zero — e.g. its own internal timeout).
// Everything else — client construction, exec, netns entry, the
// caller's context firing — is deterministic or terminal, and
// retrying it would only convert a fast, well-diagnosed failure into
// a slow one (#247 diagnostics must surface immediately).
//
// One dhcpcd exit IS terminal: "interface not found" means the link
// was deleted out from under us (DeleteEndpoint racing a slow
// CreateEndpoint). Retrying would spawn dhcpcd against a nonexistent
// interface every cycle until the lease timeout, then blame the DHCP
// server. Matched best-effort on the stderr tail #247 folds into the
// exit error — a miss merely retries as before.
func isRetryableLeaseErr(err error) bool {
	var exitErr *exec.ExitError
	if !errors.Is(err, util.ErrNoLease) && !errors.As(err, &exitErr) {
		return false
	}
	return !strings.Contains(err.Error(), "interface not found")
}

// GetIP obtains a lease via one-shot dhcpcd runs, retrying transient
// acquisition failures until the passed context's deadline. Retries
// exist because a failed exchange is often momentary (#325: lost
// server response under boot-time load, slow upstream) while the
// price of giving up — Docker refusing to start the container — is
// high. Permanent failures are returned immediately, unwrapped, so
// errors.Is/As classification (and the #247 stderr diagnostics) keep
// working; on deadline the last attempt's error is chained with %w
// for the same reason (ErrToStatus's 502, the probe's ErrNoLease
// branch).
//
// The caller's opts is not mutated — we work on a local copy so a
// caller that reuses the options struct between persistent and
// one-shot calls doesn't get its Once flag flipped on.
func GetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
	dummy := Info{}

	optsCopy := *opts
	optsCopy.Once = true

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return dummy, fmt.Errorf("%w (last attempt error: %w)", err, lastErr)
			}
			return dummy, err
		}

		info, err := attemptGetIPFunc(ctx, iface, &optsCopy)
		if err == nil {
			return info, nil
		}
		if !isRetryableLeaseErr(err) {
			// A context error mid-attempt is the deadline, not a new
			// failure — report what we were retrying when it hit.
			if lastErr != nil && ctx.Err() != nil {
				return dummy, fmt.Errorf("%w (last attempt error: %w)", err, lastErr)
			}
			return dummy, err
		}

		lastErr = err
		log.WithError(err).WithField("iface", iface).Warn("DHCP lease acquisition attempt failed; retrying")

		select {
		case <-time.After(leaseRetryDelay + rand.N(leaseRetryJitter)):
		case <-ctx.Done():
			return dummy, fmt.Errorf("%w (last attempt error: %w)", ctx.Err(), lastErr)
		}
	}
}

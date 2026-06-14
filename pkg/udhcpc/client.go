package udhcpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"

	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

const (
	DefaultHandler = "/usr/lib/net-dhcp/udhcpc-handler"
	VendorID       = "docker-net-dhcp"

	// dhcpcdStateDir is dhcpcd's compile-time database directory (DUID +
	// lease files). dhcpcd offers no runtime override and keys files by
	// interface name, so two containers whose container-side link is the
	// default `eth0` would collide on the host-shared directory. Each
	// client therefore runs in a private mount namespace with a tmpfs
	// mounted here (see Start). Identity (DUID/IAID) is pinned via
	// literal config, so a fresh empty state dir is harmless.
	dhcpcdStateDir = "/var/lib/dhcpcd"
)

type DHCPClientOptions struct {
	Hostname  string
	V6        bool
	Once      bool
	Namespace string

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
	// slave shares the parent MAC). NOTE: dhcpcd broadcast handling is
	// not yet wired here — tracked for the ipvlan path; see #152.
	Broadcast bool

	HandlerScript string
}

// DHCPClient represents a dhcpcd client managing one interface/family.
type DHCPClient struct {
	Opts *DHCPClientOptions

	cmd     *exec.Cmd
	workDir string   // per-client temp dir: generated config + event FIFO
	fifo    *os.File // read+keep-alive (O_RDWR) end of the event FIFO

	waitErr  error
	waitDone chan struct{} // closed when cmd.Wait() returns
}

// NewDHCPClient creates a dhcpcd client for iface. It allocates a
// per-client working directory, generates the dhcpcd config (pinned
// identity + observe-only + the event FIFO) and the event FIFO itself,
// and builds the (mount-namespace-wrapped) command. Start runs it.
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
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
		Iface:       iface,
		MAC:         opts.MAC,
		V6:          opts.V6,
		Once:        opts.Once,
		Hostname:    opts.Hostname,
		VendorClass: vendor,
		ClientID:    opts.ClientID,
		RequestedIP: opts.RequestedIP,
		PreferredV6: opts.PreferredV6,
		Handler:     handler,
		ConfigPath:  configPath,
		EventFIFO:   fifoPath,
	}
	if err := os.WriteFile(configPath, []byte(renderConfig(params)), 0o600); err != nil {
		return cleanup(fmt.Errorf("failed to write dhcpcd config: %w", err))
	}

	// Open the read end now (and keep it open with O_RDWR so the FIFO
	// never reports EOF between the short-lived hook processes that write
	// to it). The reaper closes it on process exit to end the scanner.
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		return cleanup(fmt.Errorf("failed to open event FIFO: %w", err))
	}

	// dhcpcd has no runtime state-dir override, so isolate per client in
	// a private mount namespace with a tmpfs over the state dir. unshare
	// execs (no fork) so the resulting process IS dhcpcd — signals and
	// Wait target it directly. `sh -c '... exec "$0" "$@"'` passes the
	// dhcpcd argv as $0/$@, avoiding any quoting of paths.
	dargs := renderArgs(params)
	mountExec := fmt.Sprintf("mount -t tmpfs tmpfs %s 2>/dev/null; exec \"$0\" \"$@\"", dhcpcdStateDir)
	wrapped := append([]string{"unshare", "-m", "/bin/sh", "-c", mountExec}, dargs...)

	c := &DHCPClient{
		Opts:    opts,
		cmd:     exec.Command(wrapped[0], wrapped[1:]...),
		workDir: workDir,
		fifo:    fifo,
	}
	// dhcpcd's own logs (stdout/stderr) go to logrus at debug level; the
	// structured events come over the FIFO, not these streams.
	c.cmd.Stdout = log.StandardLogger().WriterLevel(log.DebugLevel)
	c.cmd.Stderr = log.StandardLogger().WriterLevel(log.DebugLevel)

	log.WithField("cmd", c.cmd.Args).Trace("new dhcpcd client")
	return c, nil
}

// Start starts dhcpcd and returns a channel of lease events read from
// the FIFO. The channel is closed when the dhcpcd process exits (on its
// own for one-shot, or via Finish for the persistent client).
//
// Concurrency contract: when Opts.Namespace is non-empty, Start enters
// the target netns by locking the calling goroutine to its OS thread,
// switching netns, spawning the child (which inherits the netns), and
// switching back. It is *not* re-entrant on the same goroutine.
// Concurrent Starts on *different* goroutines are safe. On netns-restore
// failure the calling thread is deliberately leaked so the wrong-netns
// state never re-enters Go's thread pool.
func (c *DHCPClient) Start() (chan Event, error) {
	if c.Opts.Namespace != "" {
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

		ns, err := netns.GetFromPath(c.Opts.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to open network namespace `%v`: %w", c.Opts.Namespace, err)
		}
		defer func() {
			if err := ns.Close(); err != nil {
				log.WithError(err).Debug("netns close failed")
			}
		}()

		if err := netns.Set(ns); err != nil {
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
		c.fifo.Close()
		_ = os.RemoveAll(c.workDir)
		return nil, err
	}

	c.waitDone = make(chan struct{})
	events := make(chan Event, 16)

	// Scanner: read newline-delimited JSON events off the FIFO and hand
	// them downstream. Owns the events channel: closes it when the FIFO
	// read ends (the reaper closes the FIFO once dhcpcd exits). A full
	// channel drops events rather than blocking the DHCP exchange.
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(c.fifo)
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
	// the FIFO (ending the scanner, which closes events) and records the
	// exit status for Finish/Wait.
	go func() {
		c.waitErr = c.cmd.Wait()
		c.fifo.Close()
		close(c.waitDone)
	}()

	return events, nil
}

// Finish stops the client and waits for it to exit. For the persistent
// client it sends SIGTERM (dhcpcd releases its lease and exits); the
// one-shot client exits on its own (-1), so Finish only awaits it.
func (c *DHCPClient) Finish(ctx context.Context) error {
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

// GetIP runs dhcpcd once and returns the lease info obtained. The
// caller's opts is not mutated — we work on a local copy so a caller
// that reuses the options struct between persistent and one-shot calls
// doesn't get its Once flag flipped on.
func GetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
	dummy := Info{}

	optsCopy := *opts
	optsCopy.Once = true
	client, err := NewDHCPClient(iface, &optsCopy)
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

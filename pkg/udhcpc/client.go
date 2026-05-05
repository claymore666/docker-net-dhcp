package udhcpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"

	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

const (
	DefaultHandler = "/usr/lib/net-dhcp/udhcpc-handler"
	VendorID       = "docker-net-dhcp"
)

type DHCPClientOptions struct {
	Hostname  string
	V6        bool
	Once      bool
	Namespace string

	// RequestedIP, when non-empty, is passed to udhcpc as `-r ADDR`,
	// which makes the client send DHCPREQUEST for that specific IP
	// before falling back to DISCOVER. Used on plugin-restart
	// recovery to keep the same lease the container is already using
	// (the server ACKs if the lease is still valid; on NAK udhcpc
	// transparently falls back to a fresh DISCOVER).
	//
	// Format: a bare dotted-quad IPv4 address. No effect for V6.
	RequestedIP string

	// ClientID, when non-empty, is sent as DHCP option 61 (RFC 2132).
	// Servers that key reservations on client-id (rather than MAC) can
	// then track the same logical client across MAC changes — including
	// the kernel-randomised MACs Docker assigns when a container is
	// recreated, and the shared-MAC scenario inherent to ipvlan L2.
	//
	// The wire format is one type byte followed by an opaque identifier.
	// We use type 0 (per-client opaque) and let the caller pass whatever
	// stable bytes make sense (typically a hash of the Docker EndpointID).
	ClientID []byte

	// Broadcast (v4 only) makes udhcpc set the BROADCAST flag in the
	// DHCPDISCOVER, telling the server to send the OFFER as L2
	// broadcast rather than unicast to chaddr. Required for ipvlan-L2:
	// every slave shares the parent's MAC, so a unicast OFFER lands
	// on the parent and the kernel has no L3 hint to demux it to the
	// right slave (the slave's IP isn't configured yet). For macvlan
	// and bridge modes it's harmless — modern DHCP servers honour the
	// flag and clients receive the broadcast on their own interface.
	// Maps to udhcpc's `-B`.
	Broadcast bool

	HandlerScript string
}

// DHCPClient represents a udhcpc(6) client
type DHCPClient struct {
	Opts *DHCPClientOptions

	cmd       *exec.Cmd
	eventPipe io.ReadCloser
}

// NewDHCPClient creates a new udhcpc(6) client
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
	if opts.HandlerScript == "" {
		opts.HandlerScript = DefaultHandler
	}

	path := "udhcpc"
	if opts.V6 {
		path = "udhcpc6"
	}
	c := &DHCPClient{
		Opts: opts,
		// Foreground, set interface and handler "script"
		cmd: exec.Command(path, "-f", "-i", iface, "-s", opts.HandlerScript),
	}

	stderrPipe, err := c.cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to set up udhcpc stderr pipe: %w", err)
	}
	// Pipe udhcpc stderr (logs) to logrus at debug level. io.Copy returns
	// io.EOF as nil; any non-nil error means the pipe broke unexpectedly.
	go func() {
		if _, err := io.Copy(log.StandardLogger().WriterLevel(log.DebugLevel), stderrPipe); err != nil {
			log.WithError(err).Debug("udhcpc stderr pipe closed with error")
		}
	}()

	if c.eventPipe, err = c.cmd.StdoutPipe(); err != nil {
		return nil, fmt.Errorf("failed to set up udhcpc stdout pipe: %w", err)
	}

	if opts.Once {
		// Exit after obtaining lease
		c.cmd.Args = append(c.cmd.Args, "-q")
	} else {
		// Release IP address on exit
		c.cmd.Args = append(c.cmd.Args, "-R")
	}

	if opts.RequestedIP != "" && !opts.V6 {
		c.cmd.Args = append(c.cmd.Args, "-r", opts.RequestedIP)
	}

	if opts.Broadcast && !opts.V6 {
		c.cmd.Args = append(c.cmd.Args, "-B")
	}

	if opts.Hostname != "" {
		hostnameOpt := "hostname:" + opts.Hostname
		if opts.V6 {
			// TODO: We encode the fqdn for DHCPv6 because udhcpc6 seems to be broken
			var data bytes.Buffer

			// flags: S bit set (see RFC4704). binary.Write to a bytes.Buffer
			// can only fail on out-of-memory, which we'd never recover from.
			_ = binary.Write(&data, binary.BigEndian, uint8(0b0001))
			_ = binary.Write(&data, binary.BigEndian, uint8(len(opts.Hostname)))
			data.WriteString(opts.Hostname)

			hostnameOpt = "0x27:" + hex.EncodeToString(data.Bytes())
		}

		c.cmd.Args = append(c.cmd.Args, "-x", hostnameOpt)
	}

	// Vendor ID string option is not available for udhcpc6
	if !opts.V6 {
		c.cmd.Args = append(c.cmd.Args, "-V", VendorID)
	}

	// DHCP option 61 (client identifier). Format on the wire is
	// 1 byte type + N bytes id. udhcpc takes hex via -x 0x3d:HEX,
	// where HEX is the literal hex of the option payload — so we
	// prefix with our type byte and hex-encode the rest.
	if len(opts.ClientID) > 0 && !opts.V6 {
		const clientIDType = 0x00 // RFC 2132: type 0 = opaque, no DUID
		var b bytes.Buffer
		b.WriteByte(clientIDType)
		b.Write(opts.ClientID)
		c.cmd.Args = append(c.cmd.Args, "-x", "0x3d:"+hex.EncodeToString(b.Bytes()))
	}

	log.WithField("cmd", c.cmd).Trace("new udhcpc client")

	return c, nil
}

// Start starts udhcpc(6)
func (c *DHCPClient) Start() (chan Event, error) {
	if c.Opts.Namespace != "" {
		// Lock the OS Thread so we don't accidentally switch namespaces
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		origNS, err := netns.Get()
		if err != nil {
			return nil, fmt.Errorf("failed to open current network namespace: %w", err)
		}
		defer origNS.Close()

		ns, err := netns.GetFromPath(c.Opts.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to open network namespace `%v`: %w", c.Opts.Namespace, err)
		}
		defer ns.Close()

		if err := netns.Set(ns); err != nil {
			return nil, fmt.Errorf("failed to enter network namespace: %w", err)
		}

		// Make sure we go back to the old namespace when we return.
		// If restoration fails the goroutine is locked to a thread that is
		// now in the wrong netns; we deliberately keep it locked (and a
		// second Lock call ensures the Unlock above doesn't pair) so the
		// thread dies rather than leak the wrong-netns state into the Go
		// runtime's thread pool.
		defer func() {
			if err := netns.Set(origNS); err != nil {
				log.WithError(err).Error("Failed to restore original netns; pinning thread for kill")
				runtime.LockOSThread()
			}
		}()
	}

	if err := c.cmd.Start(); err != nil {
		return nil, err
	}

	// Buffered + non-blocking send: after Finish runs, the consumer
	// goroutine in dhcpManager has already taken the stop branch and
	// will never read events again. A final event line emitted by
	// udhcpc between SIGTERM and exit must not deadlock the scanner
	// goroutine on an unbuffered send.
	events := make(chan Event, 16)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(c.eventPipe)
		for scanner.Scan() {
			log.WithField("line", string(scanner.Bytes())).Trace("udhcpc handler line")

			// Each line is a JSON-encoded event
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				log.WithError(err).Warn("Failed to decode udhcpc event")
				continue
			}

			select {
			case events <- event:
			default:
				log.WithField("event", event.Type).Warn("udhcpc event dropped: consumer slow or finished")
			}
		}
	}()

	return events, nil
}

// Finish sends SIGTERM to udhcpc(6) and waits for it to exit. SIGTERM will not
// be sent if `Opts.Once` is set.
func (c *DHCPClient) Finish(ctx context.Context) error {
	// If only running to get an IP once, udhcpc will terminate on its own
	if !c.Opts.Once {
		if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("failed to send SIGTERM to udhcpc: %w", err)
		}
	}

	return c.Wait(ctx)
}

// Wait reaps the udhcpc(6) child without signalling it. Use this when the
// process has already exited on its own (e.g. the consumer noticed the
// event pipe close): if cmd.Wait is never called the kernel keeps the
// child as a zombie. Bounded by ctx so a stuck Wait can't block teardown.
func (c *DHCPClient) Wait(ctx context.Context) error {
	// Buffered: on ctx.Done we Kill and return without reading errChan,
	// so the Wait goroutine must not block forever trying to send. With
	// the buffer, Wait completes and the goroutine exits, no zombie.
	errChan := make(chan error, 1)
	go func() {
		errChan <- c.cmd.Wait()
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		// Best-effort kill; if it fails the process is already gone.
		_ = c.cmd.Process.Kill()
		return ctx.Err()
	}
}

// GetIP is a convenience function that runs udhcpc(6) once and returns the IP
// info obtained. The caller's opts is not mutated — we work on a local copy
// so a caller that reuses the options struct between persistent and one-shot
// calls doesn't get its Once flag flipped on.
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

	var info *Info
	done := make(chan struct{})
	go func() {
		for {
			select {
			case event := <-events:
				switch event.Type {
				case "bound", "renew":
					info = &event.Data
				}
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	if err := client.Finish(ctx); err != nil {
		return dummy, err
	}

	if info == nil {
		return dummy, util.ErrNoLease
	}

	return *info, nil
}

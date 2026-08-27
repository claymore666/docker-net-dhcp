// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bufio"
	"bytes"
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

// unsharePath is the absolute path to unshare(1).
//
// Absolute rather than "unshare", which exec.Command resolves through
// LookPath against the inherited PATH — measured: cmd.Path came out as
// /usr/bin/unshare, chosen by an environment variable. No impact today
// (PATH is image-set inside a root-owned rootfs) and the fix costs
// nothing (#707).
//
// This comment used to end "this was the one binary in the tree whose
// identity depended on the environment". IT WAS NOT, and the sentence
// is worth keeping as a correction rather than deleting. dhcpcd itself
// was the other one, sitting one argv position away in this very
// wrapper: it arrives as `$0` of `sh -c '… exec "$0" "$@"'`, and a
// shell's PATH lookup is the same exposure as Go's LookPath. The audit
// that produced this constant looked for exec.Command call sites, so
// the argument that reached execve through a shell was invisible to it
// — an inventory is only as wide as the mechanism it searched for. See
// dhcpcdBin, now also absolute.
//
// That correction was itself incomplete, and this sentence is the
// second half of it. `$0` is one word of a shell command line that
// also runs mount and mkdir, and a shell resolves every command word
// through PATH — not only the one that reaches execve. The sweep that
// produced dhcpcdBin looked at the argument being exec'd, so the four
// commands forty lines below were invisible to it for the third time
// by the same mechanism. They are named absolutely now; see mountBin
// and mkdirBin, and TestMountPrep_NamesEveryBinaryAbsolutely, which
// asserts the shape rather than the spellings so a fourth instance
// cannot arrive quietly.
//
// The path is Alpine's, matching the base image. It is asserted in the
// argv test alongside /bin/sh, dhcpcd and the handler, so moving the
// binary fails a test instead of silently changing which executable
// runs.
const unsharePath = "/usr/bin/unshare"

// mountBin and mkdirBin are the ABSOLUTE paths to the two other
// binaries this package runs. They sit in mountPrep's `sh -c` body,
// where the shell resolves them through PATH exactly as it resolves
// $0 — the same exposure that made dhcpcdBin and unsharePath absolute.
//
// Measured on the pinned base image
// (alpine:3.24.1@sha256:28bd5fe8…): both are /bin/*, and both are
// symlinks to /bin/busybox. They are named for the applet rather than
// for busybox deliberately — the shell dispatches busybox on argv[0],
// and if Alpine ever ships real coreutils here these constants stay
// correct while a busyboxBin would not.
//
// No impact today, and the reason is worth stating so this is not read
// as an outage: PATH is not operator-settable. config.json declares six
// env vars and PATH is not among them, cmd.Env is never assigned, and
// busybox ash falls back to a compiled-in /sbin:/usr/sbin:/bin:/usr/bin
// when PATH is unset — so the bare names resolved. What they could not
// survive is a PATH carrying a decoy. Until this change all four calls
// also carried 2>/dev/null, so a decoy that ran instead would have
// reported success and left the mounts unmade with nothing said; the
// stderr is no longer swallowed, which does not stop a decoy but does
// mean it has to be silent as well as present. See mountPrep.
//
// echoBin joined them with #780. It is the one that looks unnecessary —
// echo is a builtin in busybox ash, so today's shell resolves nothing
// through PATH for it. Named absolutely anyway, for the reason this
// comment already gives twice: the guarantee would rest on which shell
// /bin/sh happens to be, the constant costs nothing, and a value with no
// constant is a value the Dockerfile parity derivation cannot see.
const (
	mountBin = "/bin/mount"
	mkdirBin = "/bin/mkdir"
	echoBin  = "/bin/echo"
)

// shBin is the ABSOLUTE path to the shell unshare execs.
//
// It was a bare "/bin/sh" literal in the argv until now, which made it
// the one binary in this package that no constant named. That mattered
// for a reason that has nothing to do with PATH — the literal was
// already absolute, so nothing was resolved through PATH and there was
// no bug in the running code. It mattered because the parity check
// against the Dockerfile derives what the image must provide from the
// package's constants, and a value with no constant is a value that
// derivation cannot see. /bin/sh was therefore executed on every lease
// and guaranteed by nothing, silently, for the same reason mount and
// mkdir were: the sweep looked at names, and this had none.
//
// Alpine's /bin/sh is a symlink to /bin/busybox, and it is named for
// the shell rather than for busybox for the same reason mountBin is —
// if the base image ever ships a real shell here, this constant stays
// correct.
const shBin = "/bin/sh"

// workDirPrefix names every dhcpcd work directory this plugin creates.
//
// It is not decoration. dhcpcd's `-f <workdir>/dhcpcd.conf` puts the
// directory's absolute path into the child's argv, and that string is
// the ONLY thing on the host that identifies a running dhcpcd as this
// plugin's. dhcpcd's own pidfile directory is deliberately shadowed by
// the per-client tmpfs (see mountPrep, #332) so that concurrent clients
// cannot collide, which also means the usual way of finding a dhcpcd
// does not work here. SweepOrphans matches on this prefix; changing it
// in one place and not the other silently retires the sweep, which is
// why both sides read this constant.
const workDirPrefix = "net-dhcp-dhcpcd-"

// mountPrep is the shell run inside the `unshare -m` mount namespace
// before exec'ing dhcpcd. It (1) shadows the host-shared dhcpcd state
// dir with a private tmpfs (see dhcpcdStateDir), (2) shadows dhcpcd's
// runtime dir the same way so per-interface pidfiles/control sockets
// can't collide across containers (see dhcpcdRunDir — without this the
// second same-named-interface client forwards its argv into the first
// container's dhcpcd and exits without acquiring anything), and
// (3) flips /proc/sys read-write so dhcpcd's interface-setup sysctl
// writes succeed (see procSysPath, #247). All mounts are local to this
// client's mount namespace.
//
// # WHY THE STDERR IS NO LONGER SWALLOWED
//
// Every one of these four carried `2>/dev/null` until now, justified by
// this comment on the grounds that each can legitimately be a no-op or
// be refused, and that "a genuinely blocked sysctl write still surfaces
// via dhcpcd's own stderr, captured into the exit error".
//
// That argument is true, and it covers property (3) ONLY. dhcpcd fails
// loudly when it cannot write a sysctl, so the /proc/sys remount has a
// downstream observer. Properties (1) and (2) have none. If the tmpfs
// does not land, dhcpcd runs perfectly against the SHARED state and run
// directories — which is precisely the collision this function exists
// to prevent — and reports nothing, because nothing went wrong from
// dhcpcd's point of view. The commands are separated by `;`, so a
// failure does not stop the chain, and `exec` is unconditional, so the
// exit status is dhcpcd's. A justification that holds for one of three
// properties was covering all three.
//
// Nothing here is fatal, deliberately. Measured on the pinned base
// image (alpine:3.24.1@sha256:28bd5fe8…), the remount at (3) FAILS on a
// --privileged runtime — `mount: can't find /proc/sys in /proc/mounts`,
// because /proc/sys is not a separate mount there — and /proc/sys is
// already writable, so the failure is correct and harmless. Under the
// capability set this plugin actually declares (CAP_SYS_ADMIN,
// CAP_NET_ADMIN, CAP_SYS_PTRACE, no --privileged) all four succeed. So
// `set -e` or `|| exit` would convert a working host into a dead one
// for a mount that host does not need, which is not a trade a hardening
// release should make.
//
// What is left is audibility, which costs nothing and cannot break any
// host: the diagnostic now reaches the plugin. Measured end to end —
// the shell inherits fd 2 from the unshare process, `exec` replaces the
// shell but keeps its descriptors, and NewDHCPClient sets that fd to
// io.MultiWriter(logrus-at-debug, the bounded stderr tail). With
// `2>/dev/null` the parent captured an empty string; without it the
// parent captures `mount: can't find /proc/sys in /proc/mounts`, before
// the exec, alongside dhcpcd's own later output. It also enters the
// tail buffer, so a subsequent non-zero dhcpcd exit reports it.
//
// It lands at DEBUG level, which is as far as this can go without a new
// observable: dhcpcd shares this exact descriptor after the exec, so
// the shell's diagnostics cannot be raised to warn without raising all
// of dhcpcd's routine stderr with them. A counter that makes a failed
// isolation visible without reading logs is the right answer and is
// filed for v1.9.0 — a new metric is new surface, and this is a
// hardening release.
//
// NOT A CLAIM THAT A COLLISION HAS HAPPENED. It has not been observed.
// The finding is that it could not have been observed.
func mountPrep() string {
	return mountPrepStep(fmt.Sprintf("%s -t tmpfs tmpfs %s", mountBin, dhcpcdStateDir), "state-tmpfs") +
		mountPrepStep(fmt.Sprintf("%s -p %s", mkdirBin, dhcpcdRunDir), "run-mkdir") +
		mountPrepStep(fmt.Sprintf("%s -t tmpfs tmpfs %s", mountBin, dhcpcdRunDir), "run-tmpfs") +
		mountPrepStep(fmt.Sprintf("%s -o remount,bind,rw %s", mountBin, procSysPath), "procsys-remount") +
		"exec \"$0\" \"$@\""
}

// mountPrepFailMarker prefixes the line mountPrep writes to stderr when
// one of its commands fails.
//
// A marker rather than reading the commands' own stderr, because that
// stderr shares a stream with all of dhcpcd's routine output and cannot
// be raised to a warning without raising the rest with it — which is the
// reason the failure was invisible in the first place (#780).
//
// Deliberately not a word that appears in dhcpcd's vocabulary, so a
// count can never be manufactured by the client logging about something
// else.
const mountPrepFailMarker = "net-dhcp-mountprep-failed:"

// mountPrepStep appends `|| echo <marker> <step> >&2; ` to one command.
//
// `||` and not `&&`, and the chain stays `;`-separated: a failed step
// must NOT stop the ones after it or abort the exec. That is the
// pre-existing behaviour and it is the right one — the client degrades
// to sharing the host's view rather than refusing to start, and #780
// asks for the failure to become visible, not for it to become fatal.
// Changing that is a separate decision with a different blast radius.
//
// Every step is built through here so a fifth command cannot be added
// without a marker; TestMountPrep_EveryCommandReportsItsFailure asserts
// that by counting markers against commands rather than by naming the
// four that exist today.
func mountPrepStep(cmd, step string) string {
	return fmt.Sprintf("%s || %s '%s %s' >&2; ", cmd, echoBin, mountPrepFailMarker, step)
}

// mountPrepWatcher counts mountPrep failure markers on a byte stream.
//
// It sits in the client's stderr MultiWriter beside the debug-log pipe
// and the bounded tail buffer, so it sees exactly what the shell wrote,
// before dhcpcd itself has said anything.
//
// Line-buffered because a Writer receives arbitrary chunks: a marker can
// arrive split across two Writes, and matching per-chunk would both miss
// those and double-count a chunk boundary inside one line.
//
// WHICH WAY THE BUFFER BOUND FAILS. A stream that never sends a newline
// would otherwise grow without limit, so the retained partial line is
// capped and trimmed from the FRONT. Trimming can only destroy a partial
// marker, never assemble one, so the bound loses counts rather than
// inventing them — the safe direction for a counter whose whole purpose
// is to be believed when it is non-zero. Markers are written by `echo`
// and so always carry their newline; reaching the cap means something
// other than mountPrep is producing the bytes.
type mountPrepWatcher struct {
	buf []byte
}

func (w *mountPrepWatcher) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		if bytes.Contains(w.buf[:i], []byte(mountPrepFailMarker)) {
			mountPrepFailures.Add(1)
		}
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > stderrTailMax {
		w.buf = w.buf[len(w.buf)-stderrTailMax:]
	}
	return len(p), nil
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

// ValidIfaceName accepts only a kernel-legal network interface name: 1–15
// characters (IFNAMSIZ-1), starting with an alphanumeric and otherwise
// limited to alphanumerics, dot, dash and underscore.
//
// WHAT THE LEADING-ALPHANUMERIC RULE ACTUALLY GUARDS. Not re-splitting.
// The interface name is interpolated into the dhcpcd argv that runs
// under `unshare -m /bin/sh -c '… exec "$0" "$@"'`, and the `"$@"`
// quoting does prevent re-splitting — which is why this comment used to
// call the alnum-first rule "defence-in-depth". That reason was wrong,
// and a rule whose stated reason is wrong is a rule someone relaxes.
//
// The real mechanism is getopt PERMUTATION. dhcpcd 10.3.2's getopt
// permutes, so the interface — which renderArgs places LAST, as a
// trailing positional — is re-read as an option if it looks like one.
// Measured: with the interface replaced by `-c/out/evil.sh`, dhcpcd ran
// that script as uid 0 for PREINIT and CARRIER. Nothing about quoting
// enters into it; `"$@"` delivered the argument faithfully and dhcpcd
// parsed it as a flag.
//
// The rule holds today because no `-c<abs-path>` payload fits inside
// IFNAMSIZ once a leading alphanumeric is required and the kernel
// refuses '/' in a name — but that is a consequence of THIS rule, not
// an independent guard, and the argument only works if the reason is
// written down correctly. See #706 and, for the precedent, #638.
//
// Exported because pkg/plugin applies the same rule one step earlier, at
// CreateNetwork and CreateEndpoint, so a bad name fails the request
// loudly instead of surviving to the argv (#705, #706).
var ValidIfaceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,14}$`).MatchString

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
	// mountWatch counts mountPrep failure markers on the same stderr
	// stream. By value: it is written only by exec's stderr copy
	// goroutine, exactly like stderr above.
	mountWatch mountPrepWatcher
	logPipes   []io.Closer // logrus WriterLevel pipe writers; closed by the reaper

	waitErr  error
	waitDone chan struct{} // closed when cmd.Wait() returns
}

// NewDHCPClient creates a dhcpcd client for iface. It allocates a
// per-client working directory, generates the dhcpcd config (pinned
// identity + observe-only + the event FIFO) and the event FIFO itself,
// and builds the (mount-namespace-wrapped) command. Start runs it.
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
	if !ValidIfaceName(iface) {
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

	workDir, err := os.MkdirTemp("", workDirPrefix)
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
	wrapped := append([]string{unsharePath, "-m", shBin, "-c", mountPrep()}, dargs...)

	// unsharePath, not wrapped[0]. They are the same string — wrapped is
	// built one line up with unsharePath at index 0 — but an index
	// expression names no binary, so nothing reading this file, and no
	// check reading its AST, can say which program this starts. Naming it
	// here costs nothing and is what lets the Dockerfile parity check
	// resolve the exec rather than report that it cannot.
	cmd := exec.Command(unsharePath, wrapped[1:]...)
	// Give the child its own process group.
	//
	// Two reasons, both about signals reaching the wrong process. A
	// signal sent to the plugin's process group — which is what a
	// terminal, a supervisor, or a `kill -- -<pgid>` sends — otherwise
	// reaches every live dhcpcd as well, killing the renewal client of a
	// container that is still running: its lease then stops being
	// renewed and lapses at the server's deadline, with the container
	// none the wiser. (Up to v1.8.x this comment said the client would
	// RELEASE that lease. It would not — dhcpcd's -p governs whether the
	// interface is de-configured, and the release came from a `release`
	// directive that #800 removed.) And in the other direction, a group of its
	// own is what makes the client killable as a unit later, including
	// the short-lived hook processes dhcpcd spawns.
	//
	// Deliberately NOT Pdeathsig. Linux delivers PR_SET_PDEATHSIG on the
	// death of the spawning THREAD, not the process, and Go moves
	// goroutines between threads; Start locks the OS thread only for the
	// duration of the netns switch and unlocks it before returning. The
	// netns-restore-failure path is worse: it re-locks the thread
	// precisely so the thread DIES, which under Pdeathsig would SIGTERM
	// a dhcpcd that had just started correctly inside the right netns —
	// killing a live container's renewal client on the one path where
	// nothing else has gone wrong for that container. SweepOrphans
	// covers the same ground deterministically, at the only moment it
	// matters, without that failure mode (#722).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	c := &DHCPClient{
		Opts:     opts,
		cmd:      cmd,
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
	// The third writer counts mountPrep's failure markers (#780). It has
	// to be here rather than wrapped around c.stderr, because that
	// buffer is BOUNDED to its last few KiB — a namespace-prep failure
	// followed by chatty dhcpcd output would be trimmed out of it before
	// anything read it.
	c.cmd.Stderr = io.MultiWriter(errPipe, c.stderr, &c.mountWatch)
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

// RAObservation is what an acquisition attempt learned about the
// segment's router advertisements while trying to get a lease.
//
// It exists because a failed DHCPv6 acquisition has two entirely
// different meanings and a timeout cannot tell them apart (#868). A
// segment whose advertisement carries the M flag is offering DHCPv6
// addresses, so silence is a failure. A segment advertising without M
// -- stateless, or plain SLAAC -- is saying there are no DHCPv6
// addresses here, which is the NORMAL configuration of a great many
// home routers and not a failure at all.
//
// Both fields are positive assertions. Zero value means "nothing was
// observed", which is the honest reading of an attempt that saw no
// advertisement.
type RAObservation struct {
	// Seen is true once any router advertisement reached the hook.
	Seen bool
	// Managed is true if any advertisement carried the M flag.
	Managed bool
}

// Merge folds another attempt's observation into this one. Both fields
// are OR-ed rather than overwritten: an advertisement seen on the first
// attempt is still a fact on the third, and a later attempt that
// happens to miss it must not erase it.
func (o RAObservation) Merge(other RAObservation) RAObservation {
	return RAObservation{
		Seen:    o.Seen || other.Seen,
		Managed: o.Managed || other.Managed,
	}
}

// raManagedFlag is the letter dhcpcd puts in nd1_flags for a router
// advertisement's "managed address configuration" bit. Measured against
// dhcpcd 10.3.2: managed segments advertise "MO", stateless "O", SLAAC
// the empty string.
const raManagedFlag = "M"

// observeRA folds one router advertisement's flag string into an
// observation.
//
// It is a named function rather than three lines inside the collector
// goroutine because it is the only place the wire's spelling becomes a
// verdict, and everything downstream -- the retry loop's early exit,
// the fatal/tolerated classification, two health counters -- is decided
// by the boolean it returns. Inside the goroutine it was reachable only
// through a live dhcpcd: every unit test of the loop above stubs
// attemptGetIP out, so deleting the flag check entirely left the whole
// package green.
//
// Seen is set from the CALL, not from the flags. An advertisement with
// no flags at all is plain SLAAC -- a segment saying, positively, that
// it offers no DHCPv6 -- and reading that as "nothing was advertised"
// is the one confusion this whole path exists to prevent.
func observeRA(o RAObservation, flags string) RAObservation {
	o.Seen = true
	if strings.Contains(flags, raManagedFlag) {
		o.Managed = true
	}
	return o
}

// collectAcquisition drains one acquisition's event stream and returns
// the last lease it carried (nil if none) together with what it said
// about the segment's router advertisements.
//
// Split out of the goroutine that runs it so both halves are reachable
// without a live dhcpcd. The goroutine's body was previously the only
// place event.RouterFlags was read, and every unit test of the retry
// loop stubs attemptGetIP out -- so a version that read the wrong field,
// or no field, left this package fully green and failed only in the
// integration lane, ten minutes later and one layer away from the line
// at fault.
func collectAcquisition(events <-chan Event) (*Info, RAObservation) {
	var last *Info
	var ra RAObservation
	for event := range events {
		switch event.Type {
		case "bound", "renew":
			v := event.Data
			last = &v
		case "routeradvert":
			ra = observeRA(ra, event.RouterFlags)
		}
	}
	return last, ra
}

// attemptGetIP runs dhcpcd once (opts must already carry Once=true)
// and returns the lease info obtained, plus what the attempt observed
// about the segment's router advertisements. GetIP wraps it in a retry
// loop; the indirection through attemptGetIPFunc lets unit tests
// exercise that loop without running a real dhcpcd.
func attemptGetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
	dummy := Info{}
	var noRA RAObservation

	client, err := NewDHCPClient(iface, opts)
	if err != nil {
		return dummy, noRA, fmt.Errorf("failed to create DHCP client: %w", err)
	}

	events, err := client.Start()
	if err != nil {
		return dummy, noRA, fmt.Errorf("failed to start DHCP client: %w", err)
	}

	// ch carries the final lease seen, or stays unsent if no bound/renew
	// event arrived before the events channel closed. Buffered=1 so the
	// goroutine never blocks on send.
	//
	// raCh carries the router-advertisement observation and is ALWAYS
	// sent, including on the paths that return an error: on a segment
	// that offers no DHCPv6 the caller gets no lease, and the
	// observation is the only thing that makes that outcome
	// interpretable.
	ch := make(chan Info, 1)
	raCh := make(chan RAObservation, 1)
	go func() {
		last, ra := collectAcquisition(events)
		raCh <- ra
		if last != nil {
			ch <- *last
		}
		close(ch)
	}()

	// awaitRA takes the observation the collector goroutine sends once
	// the event stream closes.
	//
	// It is read on the ERROR paths too, and that is the point. dhcpcd
	// exiting non-zero on a stateless segment is not evidence that no
	// advertisement arrived -- the advertisement is what makes that exit
	// interpretable, and returning the zero value there would report
	// "no router on this segment" for a segment whose router had just
	// spoken three times. The event channel closes once the reaper has
	// reaped dhcpcd, so this receives on every path the process actually
	// ran on; ctx is the bound for the ones where it did not.
	awaitRA := func() RAObservation {
		select {
		case ra := <-raCh:
			return ra
		case <-ctx.Done():
			return noRA
		}
	}

	if err := client.Finish(ctx); err != nil {
		return dummy, awaitRA(), err
	}

	select {
	case info, ok := <-ch:
		ra := awaitRA()
		if !ok {
			return dummy, ra, util.ErrNoLease
		}
		return info, ra, nil
	case <-ctx.Done():
		return dummy, awaitRA(), ctx.Err()
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
func GetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
	dummy := Info{}

	optsCopy := *opts
	optsCopy.Once = true

	var lastErr error
	// Accumulated across attempts, never reset. What the segment
	// advertised on the first try is still what it advertises on the
	// last, and the verdict #868 needs is about the SEGMENT, not about
	// one attempt.
	var ra RAObservation
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return dummy, ra, fmt.Errorf("%w (last attempt error: %w)", err, lastErr)
			}
			return dummy, ra, err
		}

		info, attemptRA, err := attemptGetIPFunc(ctx, iface, &optsCopy)
		ra = ra.Merge(attemptRA)
		if err == nil {
			return info, ra, nil
		}
		// An advertisement WITHOUT the managed flag is conclusive on the
		// spot: the segment has said there are no DHCPv6 addresses here,
		// and retrying until the budget expires would burn the whole
		// container-start budget to reach the same answer. An
		// advertisement WITH it, or none at all, still gets every
		// attempt -- the first is a server that may yet answer, the
		// second may still be a router that has not spoken.
		if ra.Seen && !ra.Managed {
			return dummy, ra, err
		}
		if !isRetryableLeaseErr(err) {
			// A context error mid-attempt is the deadline, not a new
			// failure — report what we were retrying when it hit.
			if lastErr != nil && ctx.Err() != nil {
				return dummy, ra, fmt.Errorf("%w (last attempt error: %w)", err, lastErr)
			}
			return dummy, ra, err
		}

		lastErr = err
		log.WithError(err).WithField("iface", iface).Warn("DHCP lease acquisition attempt failed; retrying")

		select {
		case <-time.After(leaseRetryDelay + rand.N(leaseRetryJitter)):
		case <-ctx.Done():
			return dummy, ra, fmt.Errorf("%w (last attempt error: %w)", ctx.Err(), lastErr)
		}
	}
}

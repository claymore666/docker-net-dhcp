//go:build linux

package runtime

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// This file is M4's done-condition: a journal written by REAL managers against
// a REAL server, reloaded after everything in memory is thrown away, and
// checked against THE SERVER'S OWN LEASE FILE.
//
// The lease file is the point. Every other thing a restart test could compare
// against — the manager, the events it emitted, the record before it was
// written — is the client agreeing with itself, and a client that invented a
// lease out of nothing satisfies all of them. It cannot make dnsmasq write an
// address, a hardware address, a client identifier and an expiry time into
// --dhcp-leasefile.

const (
	restartBridge  = "br0"
	restartClients = 3
	restartScope   = "net-lan"
	// One plugin process writes every record here; the managers are named per
	// client. Instance is the writer, Manager is the manager.
	restartWriter = "m4-restart-writer"

	// The two directions the record's deadline may differ from the server's,
	// derived in the comment beside the assertion that uses them. MEASURED
	// 2026-09-03 over 46 samples: 0.010s to 1.002s, never early.
	restartLeaseEarly = 2 * time.Second
	restartLeaseLate  = 3 * time.Second
)

// TestTheRebuiltJournalMatchesTheServersLeaseFile writes one record per client
// through a real acquisition, drops every manager and every in-memory record,
// reloads the file, and asserts the rebuilt phases, addresses, identities and
// deadlines against dnsmasq's own lease file.
func TestTheRebuiltJournalMatchesTheServersLeaseFile(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		runRestartAgainstDnsmasq(t)
		return
	}
	reexecInNamespaces(t)
}

func runRestartAgainstDnsmasq(t *testing.T) {
	// One bridge, one veth pair per client, so the clients are N DISTINCT
	// hardware addresses on one segment. N is the number that matters: a
	// restart test with one record cannot tell a rebuild that reloads the
	// journal from one that remembers the last thing it saw.
	//
	// forward_delay 0 rather than a wait: with STP disabled a port forwards
	// immediately, and setting the delay says so instead of relying on it.
	mustRun(t, "ip", "link", "add", restartBridge, "type", "bridge", "forward_delay", "0")
	mustRun(t, "ip", "addr", "add", testServerIP+"/24", "dev", restartBridge)
	mustRun(t, "ip", "link", "set", restartBridge, "up")

	type client struct {
		id       string
		iface    string
		mac      []byte
		identity []byte
		hostname string
	}
	var clients []client
	for i := 1; i <= restartClients; i++ {
		cli := fmt.Sprintf("cli%d", i)
		port := fmt.Sprintf("brp%d", i)
		mustRun(t, "ip", "link", "add", cli, "type", "veth", "peer", "name", port)
		mustRun(t, "ip", "link", "set", port, "master", restartBridge)
		mustRun(t, "ip", "link", "set", port, "up")
		mustRun(t, "ip", "link", "set", cli, "up")

		iface, err := net.InterfaceByName(cli)
		if err != nil {
			t.Fatalf("InterfaceByName(%s): %v", cli, err)
		}
		mac := append([]byte(nil), iface.HardwareAddr...)
		clients = append(clients, client{
			id:    fmt.Sprintf("rec-%d", i),
			iface: cli,
			mac:   mac,
			// RFC 4361's type-255 shape, and the reason it is here rather
			// than being left to default: dnsmasq writes the client
			// identifier into its lease file, so a record whose identity is
			// wrong is visible in the SERVER's file rather than only in ours.
			identity: append([]byte{0xff}, mac...),
			hostname: fmt.Sprintf("m4-client-%d", i),
		})
	}

	srv := startDnsmasqCfg(t, dnsmasqConfig{iface: restartBridge})

	path := filepath.Join(t.TempDir(), "records.jsonl")
	store, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}

	for _, c := range clients {
		acquireIntoTheJournal(t, store, c.id, c.iface, c.identity, c.hostname, c.mac)
	}

	// ------------------------------------------------- the process boundary --
	//
	// Everything above is gone from here on. The managers are cancelled, the
	// store handle is closed, and the ONLY thing that crosses this line is the
	// path — which is exactly what a restarted plugin has.
	if err := store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	store = nil

	reloaded, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	defer func() { _ = reloaded.Close() }()
	evs, err := reloaded.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d := reloaded.Damage(); d.Any() {
		t.Fatalf("the journal nothing interrupted reports damage: %s", d)
	}
	rb := lease.Rebuild(evs)
	if len(rb.Rejects) != 0 {
		t.Fatalf("the fold refused %d event(s) of its own journal: %+v", len(rb.Rejects), rb.Rejects)
	}
	if len(rb.Records) != restartClients {
		t.Fatalf("%d record(s) rebuilt from %d event(s), want %d", len(rb.Records), len(evs), restartClients)
	}
	t.Logf("reloaded %d event(s) into %d record(s)", len(evs), len(rb.Records))

	// ------------------------------------------------ the server's own file --
	srv.waitCount(t, "DHCPACK("+restartBridge+")", restartClients, "the clients did not all get an ACK")
	leases := readDnsmasqLeases(t, srv.leasefile, restartClients)
	now := time.Now()
	for _, dl := range leases {
		t.Logf("dnsmasq's lease file: %s to %s (%s), client-id %s, until %s",
			dl.addr, dl.mac, dl.hostname, dl.clientID, dl.expiry.UTC().Format(time.RFC3339))
	}
	seenAddr := map[netip.Addr]bool{}
	for _, dl := range leases {
		if seenAddr[dl.addr] {
			t.Fatalf("dnsmasq's lease file names %s twice, so %d clients did not get %d addresses", dl.addr, restartClients, restartClients)
		}
		seenAddr[dl.addr] = true
	}

	for _, dl := range leases {
		got := rb.ByScopeAddr(restartScope, dl.addr)
		if len(got) != 1 {
			t.Fatalf("dnsmasq's lease file says %s is leased to %s, and the rebuilt journal resolves that address to %d record(s)",
				dl.addr, dl.mac, len(got))
		}
		rec := got[0]

		if rec.Phase != lease.PhaseJoined {
			t.Errorf("%s: dnsmasq holds a lease for %s and the rebuilt record is in phase %s, want joined",
				rec.ID, dl.addr, rec.Phase)
		}
		if !rec.Held {
			t.Errorf("%s: dnsmasq holds a lease for %s and the rebuilt record holds nothing; the trailing stop was folded as a loss",
				rec.ID, dl.addr)
		}
		if got := hexColons(rec.CHAddr); got != dl.mac {
			t.Errorf("%s: the rebuilt record wears %s, dnsmasq leased %s to %s", rec.ID, got, dl.addr, dl.mac)
		}
		if got := hexColons(rec.Identity); got != dl.clientID {
			t.Errorf("%s: the rebuilt identity is %s, the client identifier dnsmasq recorded for %s is %s",
				rec.ID, got, dl.addr, dl.clientID)
		}
		if rec.Params == nil || len(rec.Params.ClientID) == 0 {
			t.Errorf("%s: the rebuilt record carries no Params.ClientID, so its step journal is not replayable", rec.ID)
		}

		// THE DEADLINE, which is the half of the record a restart exists for.
		//
		// The span is exact and is asserted as an equality: both ends come
		// from one clockBridge reading, so a rebuilt lease that does not run
		// for precisely the lease time the server granted is wrong by
		// construction, whatever the epoch.
		if got := rec.Lease.Expire.Sub(rec.Lease.Acquired); got != testLeaseSec*time.Second {
			t.Errorf("%s: the rebuilt lease on %s runs for %s; dnsmasq granted %ds and both ends are converted through one clock reading",
				rec.ID, dl.addr, got, testLeaseSec)
		}

		// The epoch is not exact, and the two reasons are opposite in sign.
		// The record's expiry runs from the instant the DHCPREQUEST was SENT
		// (RFC 2131 section 4.4.5); dnsmasq's runs from `now` as its event
		// loop last read it, which is a whole second obtained once per
		// iteration, so the file can be stamped as much as a second and a
		// fraction BEFORE the reply it describes. The record is therefore
		// normally the LATER of the two — MEASURED 2026-09-03, 46 samples:
		// 0.010s to 1.002s late, never early. Late is bounded by truncation,
		// early by the round trip, and neither is unbounded: a fold that
		// invented a deadline, or carried one over from another record, is
		// out by minutes against a 120-second lease.
		diff := rec.Lease.Expire.Sub(dl.expiry)
		t.Logf("%s: rebuilt expiry %s, dnsmasq's file %s, difference %s",
			rec.ID, rec.Lease.Expire.UTC().Format(time.RFC3339Nano), dl.expiry.UTC().Format(time.RFC3339), diff)
		if diff > restartLeaseLate || diff < -restartLeaseEarly {
			t.Errorf("%s: the rebuilt lease on %s expires at %s and dnsmasq's own lease file says %s, a difference of %s; "+
				"the record is stamped at the DHCPREQUEST and the file at the server's cached whole second, which allows %s early and %s late",
				rec.ID, dl.addr, rec.Lease.Expire.UTC().Format(time.RFC3339Nano), dl.expiry.UTC().Format(time.RFC3339), diff, restartLeaseEarly, restartLeaseLate)
		}
		resumed, ok := rec.Resume(now)
		if !ok {
			t.Errorf("%s: dnsmasq's lease on %s runs until %s and the rebuilt record offers nothing to resume",
				rec.ID, dl.addr, dl.expiry.UTC().Format(time.RFC3339))
			continue
		}
		if resumed.Addr.Addr() != dl.addr {
			t.Errorf("%s: the record would ask the server to confirm %s; dnsmasq's lease file says it holds %s",
				rec.ID, resumed.Addr.Addr(), dl.addr)
		}
		if resumed.ServerID.String() != testServerIP {
			t.Errorf("%s: the resumed lease names server %s, want %s — a renewal has nothing to unicast to",
				rec.ID, resumed.ServerID, testServerIP)
		}

		// The counters the chassis will read come off the record, and the
		// trailing stop is in StoppedNotLost rather than in Losses.
		if rec.Counters.Acquisitions != 1 || rec.Counters.Losses != 0 || rec.Counters.StoppedNotLost != 1 {
			t.Errorf("%s: %d acquisition(s), %d loss(es), %d stop(s); want 1, 0, 1",
				rec.ID, rec.Counters.Acquisitions, rec.Counters.Losses, rec.Counters.StoppedNotLost)
		}
		if rec.Counters.Wire.Sent < 2 {
			t.Errorf("%s: the record accounts for %d frame(s) sent, want at least the DISCOVER and the REQUEST",
				rec.ID, rec.Counters.Wire.Sent)
		}
	}

	// The other direction: every rebuilt record is in the server's file. A
	// comparison that only walked the file would pass a journal that had
	// invented a fourth record.
	byAddr := map[netip.Addr]bool{}
	for _, dl := range leases {
		byAddr[dl.addr] = true
	}
	for _, rec := range rb.Records {
		a, ok := rec.Addr()
		if !ok {
			t.Errorf("%s: the rebuilt record holds no address at all", rec.ID)
			continue
		}
		if !byAddr[a] {
			t.Errorf("%s: the rebuilt record claims %s and dnsmasq's lease file has no such lease", rec.ID, a)
		}
	}
}

// acquireIntoTheJournal runs one real acquisition and writes every event it
// produces into the store, exactly as a chassis would.
//
// It cancels the manager at the end, which is not incidental: the cancel makes
// ring 1 drop the lease with ReasonStopped, so the LAST event this writes is
// always a Lost. That is the event a fold must not treat as a loss, and this is
// where a fold that does is caught against outside evidence.
func acquireIntoTheJournal(t *testing.T, store *RecordStore, id, ifname string, identity []byte, hostname string, mac []byte) {
	t.Helper()

	params := proto.DefaultParams(mac)
	params.DesyncMin, params.DesyncMax = 0, 0
	// ConflictAsync, and it is the ONE line in this fixture that is a
	// judgement rather than a scaling.
	//
	// The subject of this test is the DHCP exchange. RFC 5227's
	// probe-before-use — the default, ConflictWait — adds four to seven
	// seconds to EVERY acquisition here, which is the price D22 charges a
	// container and not something worth paying in a test of something else;
	// conflict_dnsmasq_linux_test.go pays it once, with the RFC's own
	// constants, and measures it.
	//
	// Async rather than off, because off would take the ARP socket, the
	// probes and section 2.4's listener out of the path of every one of these
	// tests. Under async they are all still there and still running beside
	// the exchange; what changes is only when the caller is told it may use
	// the address. A conflict check that broke an ordinary acquisition, or
	// that saw this host's own frames as somebody else's, would still redden
	// these tests.
	//
	// briskACD scales the schedule's DURATIONS and not its counts, so the
	// probing here is over in a third of a second instead of overlapping the
	// renewal and the reboot these tests are about. Its reasons are at its
	// definition.
	params.Conflict = proto.ConflictAsync
	params.ACD = briskACD()
	params.ClientID = identity
	params.Hostname = hostname

	var seq uint64
	append := func(ev lease.RecordEvent) {
		t.Helper()
		seq++
		ev.ID, ev.Seq, ev.Instance = id, seq, restartWriter
		if ev.At.IsZero() {
			ev.At = time.Now()
		}
		if err := store.Append(ev); err != nil {
			t.Fatalf("%s: appending %s: %v", id, ev.Op, err)
		}
	}

	append(lease.RecordEvent{
		Op: lease.OpCreate, Scope: restartScope, Family: lease.FamilyV4,
		CHAddr: mac, Identity: identity, Params: &params,
	})

	c, err := NewClient(ClientConfig{Interface: ifname, Params: params, EventBuffer: 8})
	if err != nil {
		t.Fatalf("%s: NewClient: %v", id, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	append(lease.RecordEvent{Op: lease.OpBind})

	// No duration anywhere: if the exchange never completes the test hangs
	// until go test's own timeout, which is louder than a wait that was too
	// short.
	acquired := false
	for ev := range c.Events() {
		append(lease.EventRecord(id, restartWriter, 0, time.Now(), ev))
		if ev.Kind == lease.Failed {
			t.Fatalf("%s: acquisition failed: %s", id, ev)
		}
		if ev.Kind == lease.Acquired {
			acquired = true
			break
		}
	}
	if !acquired {
		t.Fatalf("%s: the event stream ended before a lease was acquired", id)
	}

	cancel()
	<-runErr
	// Everything the manager still had to say, including the trailing Lost the
	// stop produces. Events is closed by Run, so this drains rather than
	// blocks.
	stops := 0
	for ev := range c.Events() {
		append(lease.EventRecord(id, restartWriter, 0, time.Now(), ev))
		if ev.Kind == lease.Lost && ev.Reason == proto.ReasonStopped {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("%s: the stopped manager emitted %d Lost(stopped) event(s), want 1; the case this test exists for did not happen", id, stops)
	}
	// One process writes all three records, so all three carry the same writer
	// id and each names its OWN manager. That is the separation finding 1 was
	// about, driven here against a real server rather than only in a table.
	s := c.Stats()
	append(lease.RecordEvent{Op: lease.OpStats, Manager: id, Stats: &s})
}

// dnsmasqLease is one line of --dhcp-leasefile.
type dnsmasqLease struct {
	expiry   time.Time
	mac      string
	addr     netip.Addr
	hostname string
	clientID string
}

// readDnsmasqLeases waits for the server's lease file to hold n leases and
// parses them.
//
// It POLLS rather than waiting, and it is bounded by go test's own timeout
// rather than by a duration written here: dnsmasq logs the DHCPACK from its
// reply path and writes the lease file from its main loop, so the log line the
// caller waits for does not order the file write. A sleep would be a guess
// about that gap; a spin is the same guess with the guess removed.
func readDnsmasqLeases(t *testing.T, path string, n int) []dnsmasqLease {
	t.Helper()
	for {
		got, err := parseDnsmasqLeases(path)
		if err == nil && len(got) >= n {
			if len(got) != n {
				t.Fatalf("dnsmasq's lease file holds %d lease(s), want %d:\n%+v", len(got), n, got)
			}
			return got
		}
		goruntime.Gosched()
	}
}

// parseDnsmasqLeases reads the file. The format is one lease per line:
// expiry-as-unix-seconds, hardware address, address, hostname, client
// identifier — the last two written as "*" when dnsmasq has none.
func parseDnsmasqLeases(path string) ([]dnsmasqLease, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []dnsmasqLease
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		secs, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		addr, err := netip.ParseAddr(f[2])
		if err != nil {
			continue
		}
		out = append(out, dnsmasqLease{
			expiry:   time.Unix(secs, 0),
			mac:      f[1],
			addr:     addr,
			hostname: f[3],
			clientID: f[4],
		})
	}
	return out, nil
}

// hexColons renders bytes the way dnsmasq writes a hardware address or a client
// identifier into its lease file, so the two sides are compared in one
// spelling rather than through a second parser.
func hexColons(b []byte) string {
	var sb strings.Builder
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(':')
		}
		fmt.Fprintf(&sb, "%02x", c)
	}
	return sb.String()
}

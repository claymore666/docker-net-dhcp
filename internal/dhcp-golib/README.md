# dhcp-golib — a managed-lease DHCP client for Go

**Private. Not published.** See the publication rule in the plugin's
`vision.md` §5.2 — this repo goes public the day the plugin depends on it,
and not before. That is a binary trigger, not a judgement call.

## What this is

Not a DHCP packet library — that slot is occupied (`insomniacslk/dhcp`).
This is the layer nobody has published: **lifecycle**.

> Give me a managed lease on this interface, and tell me when it changes.

Transactions, timers, the state machine, persistence, change notification.

## Usage

The caller supplies an interface name and a parameter set. What it gets back
is a running client and a stream of lease events, each carrying the address,
the gateway, the routes, the DNS servers and the absolute deadlines already
resolved out of the options — nothing is applied to the link, because
configuring the interface is the caller's job and that is what lets one
process hold leases on many links at once.

```go
func ExampleClient() {
	iface := os.Getenv("DHCP_GOLIB_EXAMPLE_IFACE")
	if iface == "" {
		return
	}

	client, err := runtime.NewClient(runtime.ClientConfig{
		Interface: iface,
		Params:    proto.DefaultParams(nil),
	})
	if err != nil {
		log.Print(err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := client.Run(ctx); err != nil {
			log.Print(err)
		}
	}()

	for ev := range client.Events() {
		if ev.Kind != lease.Acquired {
			continue
		}
		log.Printf("%s via %s until %s", ev.Lease.Addr, ev.Lease.Gateway, ev.Lease.Expire)
		break
	}

	client.Release()
	// Output:
}
```

This is `ExampleClient` in `runtime/example_test.go`, byte for byte, so it is
compiled by the suite rather than transcribed into this file. Set
`DHCP_GOLIB_EXAMPLE_IFACE` to a link with a DHCP server on it to run it.

## Milestones

The commit history and the docs name milestones M0 to M8. They are the order
the library was built in, not releases.

| | |
|---|---|
| M0 | The repository, the local verifier and its two gates, before any protocol code. |
| M1 | The vertical slice: the four rings from wire to runtime, one DHCPv4 lease from INIT to BOUND against a real dnsmasq in a user namespace. |
| M2 | Giving a lease back: DHCPDECLINE and DHCPRELEASE. |
| M3 | Keeping a lease: renewal at T1, rebinding at T2, expiry, and a NAK on renewal. |
| M4 | The durable lease record: a journal that survives a restart and repairs a torn tail. |
| M5 | Restart with a remembered address: INIT-REBOOT and the requested-address report. |
| M6 | Address conflict detection per RFC 5227, in three modes: wait, async, off. |
| M7 | IPv6. Not started. |
| M8 | Integration into the docker-net-dhcp plugin. Done in the plugin's repository, not here. |

"Done" means the milestone's tests are in the tree and the verifier passes on
them.

## Design

The architecture document and the protocol conformance checklist are held
privately alongside the plugin project, not in this tree, and are deliberately
not named by path: this repository publishes on the trigger above, and a path
into private scaffolding would publish with it. This README states the part a
reader needs before opening any file; `docs/gates.md` states what the two gates
enforce and what they cannot see.

The one thing to know before reading any code: **ring 1 is pure.** The
state machine is `Step(now, rnd, event) -> (state, []action)` with no I/O, no
clock and no goroutines inside it. Time **and entropy** are parameters —
both protocols require randomised backoff, so `rnd` has to come in from
outside or the core stops being deterministic. That is what makes
the tests instant and the replay debugger possible, and it is not
negotiable without re-reading the design doc's §2.1.

## Layout

Four rings; each depends only on the rings below it.

    ring 3  runtime/   sockets, real clock, netlink, netns, persistence, metrics
    ring 2  lease/     manager: one managed lease per (interface, family)
    ring 1  proto/     THE STATE MACHINE — pure. no I/O, no clock, no goroutines
    ring 0  wire/      codec: bytes <-> typed messages

All four were empty at M0 on purpose: the gates below were built and proven
against an empty package, because a gate added after the code it guards gets
weakened to fit the code. M1 filled all four.

## What works today — through M6

One IPv4 lease, taken and KEPT: INIT to BOUND over a real socket, renewed at T1
and rebound at T2, given back with a DHCPRELEASE or refused with a DHCPDECLINE.
Each of the things below is a test rather than a claim:

- **A lease from a real server.** `runtime` re-executes itself into a user and
  network namespace, wires a veth pair, runs dnsmasq on one end and this
  library on the other, and asserts the exchange against **dnsmasq's own log** —
  DHCPDISCOVER, DHCPOFFER, DHCPREQUEST, DHCPACK, and for the renewal a second
  DHCPREQUEST and DHCPACK with the DHCPDISCOVER count unmoved, and a DHCPNAK
  driven by restarting the server with a pool that no longer holds the leased
  address — not against the library's opinion of what happened. No root, no
  password, no host state touched.
- **A restart that keeps its address, against that same server.** The client is
  stopped and started again with the lease it held, and the server's log shows
  a DHCPREQUEST and a DHCPACK for that address with no DHCPDISCOVER between
  them. A second run, against a replacement server whose pool no longer holds
  the address, shows the refusal and then the whole acquisition the RFC
  prescribes after one; a third, whose remembered lease is past its deadline,
  shows the DHCPDISCOVER and never the DHCPREQUEST.
- **A lease record that outlives the process that wrote it.** Three managers
  lease from that same server at once over one bridge, every manager and every
  in-memory record is then thrown away, and the journal is reloaded from its
  file and checked against **dnsmasq's own lease file** — the addresses, the
  client identifiers and the expiry times the server wrote for itself
  (`TestTheRebuiltJournalMatchesTheServersLeaseFile`). A tail torn by a write
  that ran out of file is repaired rather than appended onto, and the line lost
  to it is counted rather than assumed away
  (`TestAShortWriteLeavesTheStoreUsableAndTheNextEventSurvives`,
  `TestReopeningAfterATornTailDoesNotLandOnTheFragment`).
- **An address probed before it is used, against a real squatter on the link.**
  RFC 5227's probe window runs on the wire: a second host answers for the
  address dnsmasq offered, and the server's log carries the DHCPDECLINE and the
  acquisition that restarts after it — in the mode that withholds the address
  until the window closes, and in the mode that reports it and then takes it
  back (`TestASquatterInTheProbeWindowMakesAWaitingClientDecline`,
  `TestASquatterInTheProbeWindowMakesAnAsyncClientDecline`). A conflict after
  BOUND takes RFC 5227 §2.4's path
  (`TestASquatterAfterBoundTakesSection24sPath`), and a client built with
  detection off puts no ARP frame on the link at all
  (`TestAnOffClientPutsNoARPOnTheWire`).
- **A socket that keeps the namespace it was opened in.** A client is built on
  a thread inside a network namespace of its own, the thread is then destroyed,
  and the client leases from a server that exists only in there — while the
  goroutine running it cannot even see the interface. That is what lets one
  process lease on many containers' links at once.
- **That same exchange replayed offline.** The journal of the live run is fed
  back through ring 1 and must produce the identical lease. Ring 1 is pure, so
  the replay needs no socket, no clock and no server.
- **The whole acquisition path in milliseconds.** `proto` tables the path with
  no root, no namespace and no network at all.

### What it does NOT do

Stated because a bound nobody writes down is read as a guarantee:

- **A client whose reboot goes unanswered keeps nothing.** RFC 2131 permits a
  client that gets neither an ACK nor a NAK to its INIT-REBOOT request to go on
  using the lease for the rest of its term; this one does not, and acquires
  from INIT instead. Nothing reaches the link that no server has confirmed in
  this process's lifetime. Silence from a server is also what a message that
  never left the host produces, and the two are not worth telling apart by
  guessing.
- **No INFORM.** A caller that already has an address and wants only the
  parameters is not served; the message type is in `wire` and nothing sends it.
- **IPv4 only.** No DHCPv6, no Router Advertisement, no SLAAC.
- **No ARP resolution for the DHCP unicast.** The transport unicasts only to a peer whose hardware address it
  learned from a frame that peer sent; a unicast it cannot address is REFUSED
  rather than broadcast anyway. RENEWING is what that refusal falls on, and it
  is why a renewal behind a relay agent waits for T2 and its broadcast instead
  of losing the lease.
- **No fragment reassembly and no BPF filter.** A fragmented reply is dropped;
  every IPv4 frame on the link is read and filtered in user space, and the cost
  is counted as `Skipped` rather than assumed away.
- **One server implementation has ever answered it:** dnsmasq 2.91.

## Testing

`./verify.sh` is the only arbiter this repository has: one command and one
verdict line, PASS or FAIL, with no row allowed to report PASS without also
saying how many things it examined. Its dnsmasq tests run inside an
unprivileged user namespace, so none of it needs root, a password, or any host
state; they have a row and a wall-clock bound of their own, separate from the
pure suite's. One row may report a third verdict: the arbiter's own oracle
records SKIPPED when `verify.sh`, the manifest and `scripts/` are byte for byte
what they were the last time it passed here, and `./verify.sh --oracle` runs it
regardless — which is what to run before a merge. What each row measures, and
what a skip is not re-checking, is in `docs/verifying.md`; what the two ring
gates cannot see is in `docs/gates.md`.

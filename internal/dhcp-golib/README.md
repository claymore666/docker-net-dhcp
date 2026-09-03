# dhcp-golib — a managed-lease DHCP client for Go

**Private. Not published.** See the publication rule in the plugin's
`vision.md` §5.2 — this repo goes public the day the plugin depends on it,
and not before. That is a binary trigger, not a judgement call.

## What this is

Not a DHCP packet library — that slot is occupied (`insomniacslk/dhcp`).
This is the layer nobody has published: **lifecycle**.

> Give me a managed lease on this interface, and tell me when it changes.

Transactions, timers, the state machine, persistence, change notification.

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

## What works today — through M3

One IPv4 lease, taken and KEPT: INIT to BOUND over a real socket, renewed at T1
and rebound at T2, given back with a DHCPRELEASE or refused with a DHCPDECLINE.
Three things are true of it, and each is a test rather than a claim:

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
- **No INFORM, and no address-in-use probe.** `ReportConflict` sends the
  DHCPDECLINE, but nothing in this library notices a conflict on its own: a
  real one on a real host produces no DHCPDECLINE, because nothing looks.
- **IPv4 only.** No DHCPv6, no Router Advertisement, no SLAAC.
- **No ARP.** The transport unicasts only to a peer whose hardware address it
  learned from a frame that peer sent; a unicast it cannot address is REFUSED
  rather than broadcast anyway. RENEWING is what that refusal falls on, and it
  is why a renewal behind a relay agent waits for T2 and its broadcast instead
  of losing the lease.
- **No fragment reassembly and no BPF filter.** A fragmented reply is dropped;
  every IPv4 frame on the link is read and filtered in user space, and the cost
  is counted as `Skipped` rather than assumed away.
- **One server implementation has ever answered it:** dnsmasq 2.91.

### One thing that surprised us, recorded because it will surprise the next reader

A reply from a server on the SAME HOST arrives with its UDP checksum **not
computed**. Linux writes the folded pseudo-header sum into the field and leaves
completing it to hardware, so an AF_PACKET reader on the far side of a veth
pair sees `CHECKSUM_PARTIAL` bytes. MEASURED 2026-08-29 against dnsmasq 2.91:
the captured OFFER held `0x24f6` in a field whose completed value is `0xe58c`.
Both are fixtures in `runtime/ipudp_test.go`, asserted against the captured
bytes. `0x24f6` is the pseudo-header sum for that source, destination and
length and nothing else — flip a payload octet and the completed checksum
moves while the field does not, which is a test, and is exactly why the field
says nothing about the payload.

A client that verifies the checksum strictly therefore never sends a REQUEST —
which is precisely what the first run of the dnsmasq test did, for two minutes,
retransmitting DISCOVER while the server answered every one of them. The parser
recognises that exact value, reports the payload as unverified, and the
transport counts it — though the counter says only what a field held, never
where the sender was.

**The bound, both halves of it:** neither an uncompleted checksum nor RFC 768's
zero checks the payload, so a corrupt payload is accepted under both — more
cheaply under the zero. The accepting value in the uncompleted case is not a
lucky collision either: it is a pure function of source, destination and UDP
length, all read from the frame itself, so anyone who can put a frame on the
link can compute it. Closing it needs `PACKET_AUXDATA`, whose
`TP_STATUS_CSUMNOTREADY` states the deferral as a fact instead of leaving us to
infer it — new I/O, and a later milestone.

## Verifying

    ./verify.sh

One command, one verdict. Exit 0 is PASS and is the normal state; exit 1 is
FAIL. A step that cannot be measured is a FAIL, never a skip.

It runs `go build`, `go vet`, `gofmt`, `shellcheck` over the shell scripts, the
gate roster cross-check, the T1 and T2 gates, the race-enabled unit suite under
a wall-clock ceiling AND a `go test -timeout` (the ceiling cannot bound a test
that never returns — it is computed after `go test` comes back), a check that
the flags the suite runs with carry a hang timeout that exceeds that ceiling
and that the suite invocation expands them, a check that every test function
DECLARED in a `_test.go` file actually ran, a citation check, and its own
oracle.

**No row can report PASS on an exit status alone.** A row records PASS only by
also stating how many things it examined, and a count that is absent or zero is
rewritten to FAIL by the one function that writes a row. This is structural
rather than a guard per row, because three rows were found passing over an
absent subject in three consecutive review rounds — the unit suite with every
test build-tagged out, the oracle replaced by `exit 0`, and the row roster
itself with a step call deleted — and all three inherited the same default,
that a command with nothing to do exits 0.

The residual is worth naming beside the claim: nothing inside the script can
force that count to be DERIVED rather than written. What closes that is
external — a scenario that empties a row's domain and requires the row to go
red. This paragraph used to say there was one such scenario per row. There was
one, for one row of eleven; `docs/gates.md` now lists which rows have one,
which do not, and why the ones that do not cannot.

The citation check is stated here as a BOUND, because the sentence that used to
stand in its place was a completeness claim and the tree falsifies it. What it
reads is: a Test/Benchmark/Fuzz/Example token appearing after the first `//` on
a `.go` line that is not a URL scheme separator, or anywhere on a line of a
`.md` file; and it requires each one to appear in a top-level `func`, `var`,
`const` or `type` declaration. It does **not** see block comments, `.sh` files,
or a token in a `.go` line's ordinary text — there are live examples of all
three in this tree, `scripts/test-verify.sh` chief among them. The seven
enumerated escapes, including the one direction in which it produces a false
positive, are in `docs/gates.md`; they are part of the check, not a caveat
about it.

"Every check" used to be bounded by this sentence: *a check runs only if
`verify.sh` calls it, and nothing inside `verify.sh` notices a call that is no
longer there.* That bound was accurate and it rested on the oracle's scenarios
noticing instead — and on 2026-08-30 a review measured that the oracle can be
replaced by a two-line script that exits 0. **An acknowledged bound was
load-bearing on a guard that dies with its subject**, and the composition was
never measured: with the oracle stubbed AND a step call deleted, the arbiter
printed `VERDICT: PASS (10 steps)`.

**That fix was defeated too, and the sentence that used to stand here is the
reason it is worth reading the next one carefully.** It said: the row roster is
cross-checked against the rows recorded, and the oracle's expected scenario
count is derived by `verify.sh` from the oracle's own source, so a stub —
total or partial — is a mismatch rather than an answer. Both halves were true
and both were defeated in one round, the same way, because **every expectation
was derived from the thing it was checking.** MEASURED 2026-08-30 by review:
delete the `shellcheck` gate — its step, its roster entry, its two scenarios —
and eleven rows became ten with `VERDICT: PASS (10 steps)` and four live
`SC2034` findings in the tree. Replace the oracle with forty-five empty stub
functions and one `echo`, and it passes. Delete the count guard together with
the scenarios that drive it, and it passes.

Four rounds found that same shape, one level up each time, and every guard had
a non-vacuity floor. **Every floor was at zero.** Zero is the one size a
population cannot reach by deletion, so shrinking one by a single member was
invisible in all of them.

What stands now is `verify.manifest.sh`: a file that contains the expectation
and nothing else — the row names, the gate names, the scenario names, a literal
count beside each list, and a band around the declared-test population. Both
`verify.sh` and the oracle read it, so neither derives its expectation from
itself or from the other, and `internal/manifest` pins the same names and
numbers from Go, in another directory, running inside the suite.

**And that, in turn, was defeated — on a different axis, which is why it is
worth its own paragraph.** MEASURED 2026-08-30 by review: keep every name,
delete four scenario BODIES, make the arbiter's own guard inert, and the run
reports `VERDICT: PASS` with a live defect in the tree. The manifest was never
touched and every count was satisfied in full, because **a name is not a
behaviour**. Every operand answered *is it there*; none answered *does it do
anything*.

So the manifest now states, per scenario, what that scenario must be OBSERVED
to have done — the process result class, and a row verdict it read out of the
subject's own table. The oracle reports what each scenario actually observed;
`verify.sh` compares the report against the manifest. The comparison lives in
none of the three places somebody would edit to make a scenario stop working,
so an emptied body is not a passing body: it is a scenario that observed
nothing, named in the diagnosis. A scenario that dies mid-plant reports its own
death for the same reason — silence and success used to be the same output.

That bound was taken. A review built exactly the body described above — kept
only the lines producing the observation, dropped every assertion, planted a
different defect reaching the same row — composed it with the bound beside it,
and got a passing verdict over a live defect with four scenarios testing
nothing. So a contract now names the DEFECT as well as the row: its fourth
field is a fragment of the note the ARBITER wrote beside that row, and the
arbiter is not the scenario that planted anything. Reddening the right row for
the wrong reason produces a different note.

**BOUND, and it is the honest one:** the diagnosis is matched as a substring,
so two plants the arbiter describes in the same words are still
indistinguishable. And every check on the oracle's report is defeatable by a
fabricator that reproduces the report — which is why `verify.sh` also plants
seven defects for ITSELF before calling the oracle, and requires each to redden
its own row while the unplanted rows stay green. That is a lower bound on the
arbiter's liveness, not proof of it; what it buys is that blinding the arbiter
now takes an edit to `verify.sh` too.

The claim is bounded, because a completeness claim here is the sentence this
project has been wrong about four times. What holds is narrower: **the row
roster and the scenario list cannot be shrunk by an edit confined to a single
file**, because `internal/manifest` pins them a second time, in Go, and the
edit then has to be made twice, in two languages. **The escape, named rather
than left to be found:** the self-drive's own detection set is held by a length
literal in `verify.manifest.sh` and by nothing else, so deleting entries there
shrinks it and the run still passes. MEASURED; the plugin's deferred-work
record carries the reproduction.

The step deletions below were driven rather than assumed.
MEASURED 2026-08-29, deleting one step at a time from a copy and running
`scripts/test-verify.sh` against it: eight of the nine steps then present
redden at least one oracle scenario — `gofmt` 4, the gate roster 3, the two
gates 6, the unit suite refuses the run outright, `shellcheck` 2, `build` and
`vet` 1 each. Those counts were taken against the 19-scenario oracle, before
`hang-bounded`, `bounds-ordering` and `stale-citation` were added later the
same day. They are LOWER bounds now rather than equalities: a scenario can only
add a detection, never remove one, and nobody re-ran the nine deletions. The
two steps added after that sweep — the timeout-ordering check and the citation
check — were each driven by deleting the step from a copy and watching the
scenario that owns it report ABSENT.

What landed on 2026-08-30 is a different shape and is described as one: three
checks added INSIDE steps that already existed, so step deletion is not the
drive for them. Each was driven by the mutant it exists to catch — the suite
invocation detached from its flag array, one package's tests and then the whole
library's tests switched off, and a URL in a string literal, that last one in
both directions because the risk in the fix was that it would blind the check.
The oracle carries `suite-args-detached`, `suite-one-package-disabled`,
`suite-tests-disabled`, `citation-url` and `citation-after-url` for them.

`vet` had no witness until this measurement was taken; it passed 18 of the 18
scenarios that existed then with the step deleted, and the `vet-violation`
scenario exists because of that.

The oracle step is the one that cannot be closed from the inside: see below.

There is no CI on this repository and there will not be: the self-hosted
runners belong to the plugin repository and cannot serve a second private repo
without an organisation. `verify.sh` is the only arbiter there is, which is why
it is one command and not a paragraph describing what a developer should run —
and why it has an oracle of its own, `scripts/test-verify.sh`, which plants a
defect in a copy of the tree and requires the row that owns that defect to be
the row that fails. `verify.sh` runs it as a step.

That loop closes only while it is wired, and it cannot close itself: a
`verify.sh` that drops its oracle step never runs the scenario that checks the
step is there. MEASURED 2026-08-29 — deleting the step is caught, but only
incidentally, by `shellcheck` objecting that `--inner`'s variable became
unused; composing past that one objection gives a clean PASS verdict with the
arbiter's own arbiter silently gone. Running
`scripts/test-verify.sh` directly is the check for that, and it is a human act,
not a wired one. The rest of what neither can see is in `docs/gates.md`.

### The two gates

- **T1 — ring 1 imports nothing that does I/O.** Enforced by parsing the import
  set, not by convention.
- **T2 — no test waits on wall-clock time.** Enforced by an identifier
  allowlist over test files, plus a wall-clock ceiling on the suite.

Both are load-bearing guarantees rather than hygiene, and both are checked
against a planted violation rather than trusted. The allowlists the gates read
are checked too, and separately: a correct gate enforcing a widened table is
the failure a gate test cannot see. What each one **cannot** see
is written down in `docs/gates.md`; read that before relying on either.

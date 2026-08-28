// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag,
// unlike the rest of the package. The floor decides whether a whole
// integration run passes, so its logic has to be testable without a
// live plugin — a floor that has never been observed rejecting
// anything is not known to work. Keeping the decision pure and
// untagged puts healthfloor_test.go in the ordinary `go test ./...`
// unit job. Everything that needs a socket stays in health.go.
//
// HealthResponse lives here rather than in health.go for the same
// reason: the floor takes one, so it has to compile untagged.

package harness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// HealthResponse mirrors pkg/plugin.HealthResponse. Duplicated here
// so the integration package doesn't pull on pkg/plugin internals.
type HealthResponse struct {
	Healthy bool `json:"healthy"`
	// InstanceID identifies the plugin process that served this
	// response. Two reads are comparable as a delta only when their
	// InstanceID matches — see counterwindow.go and #405.
	InstanceID      string  `json:"instance_id"`
	UptimeSeconds   float64 `json:"uptime_seconds"`
	ActiveEndpoints int     `json:"active_endpoints"`
	PendingHints    int     `json:"pending_hints"`
	RecoveredOK     int32   `json:"recovered_ok"`
	RecoveryFailed  int32   `json:"recovery_failed"`
	// RecoveryFailed has four benign twins, at the four points recovery
	// can stop early for a reason that is not a plugin fault. None is
	// healthy-affecting.
	//
	// RecoveryDeferred is the entry gate: the daemon was not serving yet
	// when recovery ran, so it was retried after the socket came up.
	// Expected on any daemon restart (#383).
	RecoveryDeferred int32 `json:"recovery_deferred"`
	// RecoveryAbortedContainerGone is the per-endpoint case: the
	// container had already exited by the time recovery reached it
	// (#376).
	RecoveryAbortedContainerGone int32 `json:"recovery_aborted_container_gone"`
	// RecoveryNetworkGone is the per-network case: the network was
	// removed between the listing that found it and the read of its
	// detail, so the whole network is skipped (#648). The list recovery
	// walks is a snapshot, and a suite that creates and removes networks
	// continuously hits this. Until #648 it landed in RecoveryFailed and
	// failed a run in which every test passed.
	RecoveryNetworkGone int32 `json:"recovery_network_gone"`
	// RecoveryFingerprintsSkipped is the endpoint-level sibling: recovery
	// adopted the endpoint but could not learn its hostname, so it
	// recorded no fingerprint and DeleteEndpoint will lay no tombstone
	// for it (#721). Not fatal — the endpoint has a renewal client, it
	// has lost only address stability across its next restart — but a
	// suite where this climbs is one whose restart-stability assertions
	// are being decided by something other than the code under test.
	RecoveryFingerprintsSkipped int32 `json:"recovery_fingerprints_skipped"`
	// RecoveryAlreadyManaged is the per-endpoint case on the other side:
	// a Join reached the endpoint first, so recovery yielded and left
	// that client in place (#480). Expected whenever a deferred recovery
	// overlaps containers coming back.
	RecoveryAlreadyManaged int32 `json:"recovery_already_managed"`
	JoinStartFailures      int32 `json:"join_start_failures"`
	// JoinAbortedContainerGone is the benign twin of JoinStartFailures:
	// the container exited before the persistent client was up. Not
	// healthy-affecting (#373).
	JoinAbortedContainerGone int32 `json:"join_aborted_container_gone"`
	// JoinAbortedNoContainer is the other benign twin: no container ever
	// claimed the endpoint, so its address was released rather than left
	// to expire (#566).
	JoinAbortedNoContainer int32 `json:"join_aborted_no_container"`
	JoinAttachSlow         int32 `json:"join_attach_slow"`

	// RestartLinkUpWaited / RestartLinkUpTimeouts mirror the #408
	// window: a child link that came up only after the departing link
	// released the address, and that wait outlasting its budget.
	// Neither is healthy-affecting (#422).
	RestartLinkUpWaited     int32 `json:"restart_link_up_waited"`
	RestartLinkUpTimeouts   int32 `json:"restart_link_up_timeouts"`
	JoinAbortedEndpointLeft int32 `json:"join_aborted_endpoint_left"`
	TombstoneWriteFailures  int32 `json:"tombstone_write_failures"`
	// TombstoneQuarantines is healthy-affecting (#724): the tombstone
	// file was unparseable and was moved aside, taking every live
	// tombstone on the host with it.
	TombstoneQuarantines int32 `json:"tombstone_quarantines"`
	// TombstonesConsumed is RecoveredOK's counterpart: the address was
	// preserved by replaying a tombstone rather than by recovery
	// re-adopting a live endpoint. Together they let a restart test say
	// WHICH path ran instead of only that the address survived (#386).
	TombstonesConsumed int32 `json:"tombstones_consumed"`
	// AddressConflicts is healthy-affecting (#524) and so appears in
	// floorCounters below. ConflictProbeFailures is not, but is mirrored
	// here so a run can say whether the detector actually ran — a probe
	// that never happened reads exactly like a clean segment.
	AddressConflicts      int32 `json:"address_conflicts"`
	ConflictProbeFailures int32 `json:"conflict_probe_failures"`
	// ConflictProbeStaleRoutes counts leftover probe routes reclaimed
	// from a probe cut short before it could clean up (#572).
	ConflictProbeStaleRoutes int32 `json:"conflict_probe_stale_routes"`
	// AddressConflictProbes is what makes address_conflicts=0 mean
	// anything: zero probes and a clean segment read identically
	// otherwise.
	AddressConflictProbes int32 `json:"address_conflict_probes"`
	// SandboxNetnsVisible is how many sandbox netns entries the plugin
	// can see, or -1 if it cannot read the directory (#567). Sampled per
	// request, not accumulated. A pointer so an older plugin that does
	// not publish it is distinguishable from one reporting -1 — absent
	// data is not a value.
	SandboxNetnsVisible *int32 `json:"sandbox_netns_visible"`
	LeaseChanged        int32  `json:"lease_changed"`
	LeasesObtained      int32  `json:"leases_obtained"`
	LeasesRenewed       int32  `json:"leases_renewed"`
	DHCPTimeouts        int32  `json:"dhcp_timeouts"`
	// ClientStopFailures was lease_release_failures until #800. A
	// renewal client that did not shut down cleanly when signalled — it
	// says nothing about the lease, which is held to expiry either way
	// because nothing this plugin runs sends a DHCPRELEASE.
	ClientStopFailures  int32 `json:"client_stop_failures"`
	NAKsReceived        int32 `json:"naks_received"`
	LedgerWriteFailures int32 `json:"ledger_write_failures"`
	// DirectivesRefused / MountPrepFailures are the two places the DHCP
	// client package declines to do what it was asked and carries on
	// anyway (#780): a dhcpcd directive dropped for a control character
	// in its value, and a per-client mount-namespace preparation command
	// that failed. Neither is healthy-affecting — both describe an input
	// that did not take effect, not a container without a renewal
	// client. MountPrepFailures counts COMMANDS, not clients.
	DirectivesRefused int32 `json:"directives_refused"`
	MountPrepFailures int32 `json:"mount_prep_failures"`
	// RouterAdvertGuardFailures counts steps of a DHCPv6 client's
	// Router-Advertisement guard that failed (#875): the guard puts the
	// container's kernel in charge of router discovery and prefix
	// processing and stops dhcpcd turning that off again. Not
	// healthy-affecting, and it counts STEPS, not clients. A suite that
	// reads it non-zero is looking at a container whose IPv6 default
	// route will stop being refreshed, which the plugin's own view
	// cannot distinguish from a quiet segment.
	RouterAdvertGuardFailures int32 `json:"router_advert_guard_failures"`
	// ParentLinkWaits / ParentLinkWaitTimeouts cover contention on a
	// shared parent NIC, where a macvlan and an ipvlan child cannot
	// coexist (#486/#549). Waits means an operation queued and got
	// through; timeouts means it gave up and asked the kernel anyway,
	// which is when a container start can still fail with EBUSY.
	// Neither is healthy-affecting.
	ParentLinkWaits        int32 `json:"parent_link_waits"`
	ParentLinkWaitTimeouts int32 `json:"parent_link_wait_timeouts"`
	// DHCPServerTierFallbacks / DHCPServerPolicyExhausted cover the
	// dhcp_servers preference list (#111) and dhcp_deny_servers (#669).
	// Fallbacks means a preferred server was silent and the next one in
	// the list answered — the feature working, and the only signal that
	// a ranked server has gone away. Exhausted means every server the
	// network was allowed to use stayed silent, which is what separates
	// "the servers you named are down" from "DHCP is broken". Neither
	// is healthy-affecting: both describe the segment, not the plugin.
	DHCPServerTierFallbacks   int32 `json:"dhcp_server_tier_fallbacks"`
	DHCPServerPolicyExhausted int32 `json:"dhcp_server_policy_exhausted"`

	// DHCPv6ConfigOnly counts DHCPv6 replies that carried configuration
	// and no address — the stateless case (#815). Not healthy-affecting
	// and deliberately not in the floor table: on a stateless segment
	// this rising is the feature working, and on every other segment it
	// stays at zero on its own.
	DHCPv6ConfigOnly int32 `json:"dhcpv6_config_only"`

	// DHCPv6NotOffered / DHCPv6NoRouterAdvert cover the two ways an
	// IPv6 endpoint can come up without a DHCPv6 lease (#868): the
	// router advertised no managed address, or no router advertised at
	// all. Neither is healthy-affecting and neither belongs in the
	// floor table — on a v4-only or managed-v6 segment both stay at
	// zero on their own, and on a stateless segment the first one
	// rising is the feature working. They are separate fields for the
	// same reason they are separate counters: a test that asserted
	// only their sum could not tell a stateless network from a
	// segment with no router on it.
	DHCPv6NotOffered       int32 `json:"dhcpv6_not_offered"`
	DHCPv6NoRouterAdvert   int32 `json:"dhcpv6_no_router_advert"`
	IPv6LinkEnableFailures int32 `json:"ipv6_link_enable_failures"`

	// published is the key set of the payload this value was decoded
	// from. It exists because an absent JSON field decodes to zero,
	// which is indistinguishable from a counter that is genuinely at
	// zero — so without it the floor reads "clean" for counters the
	// plugin never sent (#377).
	//
	// nil means "this value was built by hand, not decoded", and the
	// presence check is skipped. UnmarshalJSON always sets a non-nil
	// map, including for an empty or null payload, so nil cannot occur
	// on the path that talks to a real plugin.
	published map[string]json.RawMessage
}

// UnmarshalJSON decodes as usual and additionally records which keys
// the payload actually carried.
//
// The `plain` alias is what stops this from recursing: an alias type
// has the same fields but not the methods, so the inner Unmarshal uses
// the default struct decoder. published is unexported and therefore
// invisible to encoding/json, which is also why nothing outside this
// package can fabricate a misleading key set.
func (h *HealthResponse) UnmarshalJSON(b []byte) error {
	type plain HealthResponse
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	if keys == nil {
		// A literal `null` body unmarshals into a nil map without
		// erroring. Normalise it: a payload that carried no keys is
		// "published nothing", not "presence unknown".
		keys = map[string]json.RawMessage{}
	}
	*h = HealthResponse(p)
	h.published = keys
	return nil
}

// floorCounter is one counter the floor reads.
//
// The table below is the single source of truth for that set: both the
// value check and the presence check iterate it, so a counter cannot be
// added to one and forgotten in the other. name must equal the JSON tag
// on the field read — TestFloorCounterNamesMatchJSONTags pins that, and
// the presence check catches the other half of the same drift, where
// the plugin renames a key this side has not followed.
type floorCounter struct {
	name  string
	read  func(*HealthResponse) int32
	fatal bool
	why   string
}

var floorCounters = []floorCounter{
	{
		name:  "join_start_failures",
		read:  func(h *HealthResponse) int32 { return h.JoinStartFailures },
		fatal: true,
		why:   "a running container was left without a renewal client; since #373 the benign container-exited case is counted separately as join_aborted_container_gone, so this counter now means only a real fault",
	},
	{
		name:  "tombstone_write_failures",
		read:  func(h *HealthResponse) int32 { return h.TombstoneWriteFailures },
		fatal: true,
		why:   "the plugin could not persist its tombstone state to disk; an endpoint will not keep its address across a restart",
	},
	{
		name:  "tombstone_quarantines",
		read:  func(h *HealthResponse) int32 { return h.TombstoneQuarantines },
		fatal: true,
		why:   "the tombstone file was unparseable and was quarantined as tombstones.json.corrupt-<ts>; every live tombstone on the host went with it, so any container that restarts inside the TTL window comes back with a different MAC and a different address. Strictly worse than tombstone_write_failures, which costs one container the same thing (#724). The quarantined file is still on disk under STATE_DIR and nothing reaps it — read it before deleting it, it is the only evidence of what was lost",
	},
	{
		name:  "recovery_failed",
		read:  func(h *HealthResponse) int32 { return h.RecoveryFailed },
		fatal: true,
		why:   "recovery could not rebuild a RUNNING container's renewal client, so its lease will not renew until it is restarted. Fatal since #421: the benign paths that used to land here are counted separately — recovery_deferred for a daemon that was not serving yet (#383), recovery_aborted_container_gone for a container that had already exited (#376), and recovery_network_gone for a network removed out from under the walk (#648) — and the probation runs this counter was left non-fatal for came back clean",
	},
	{
		name:  "address_conflicts",
		read:  func(h *HealthResponse) int32 { return h.AddressConflicts },
		fatal: true,
		why:   "an endpoint was leased an address another device on the segment already holds, so traffic for it is wrong for both hosts (#524). Fatal from the start, unlike recovery_failed: there is no benign path into this counter — it moves only when a probe got an ARP reply from a MAC that is not the endpoint's. A run that trips this has a container up on somebody else's address, which is the exact production fault the counter was added for",
	},
}

// absentWhy explains a finding raised because the plugin did not
// publish a counter at all. Fatal regardless of the counter's own
// verdict — recovery_failed being merely noisy is a statement about
// what its value means, not a licence to stop looking at it.
const absentWhy = "the plugin did not publish this counter, so this run proves nothing about it — an absent JSON field decodes as zero and would otherwise read as clean. Either the plugin under test is an older build than the suite (rebuild and reinstall it), or the counter was renamed in pkg/plugin/endpoints.go without updating floorCounters in this file"

// FloorFinding is one healthy-affecting counter the floor took issue
// with — either it moved off zero, or the plugin did not report it.
type FloorFinding struct {
	Counter string
	Value   int32
	// Absent marks a counter the plugin never published. Value is
	// meaningless for these — the point is that there was no value.
	Absent bool
	// Flag marks a finding about the plugin's own boolean verdict
	// rather than a counter. Value is meaningless for these too:
	// printing "healthy=0" would invite a reader to look for a counter
	// that does not exist.
	Flag bool
	// Fatal distinguishes "this counter only ever means a real plugin
	// fault" from "this counter is known to also count benign events".
	// A non-fatal finding is still printed — loudly — because it is a
	// signal, just not one we can hang a build on yet.
	Fatal bool
	// Why explains the verdict in the failure output. The reader is
	// someone staring at a red CI job, not someone with this file open.
	Why string
}

// healthyKey is the plugin's own summary verdict on the payload.
const healthyKey = "healthy"

// healthyWhy explains a floor failure raised by the flag rather than by
// a counter this file knows about.
const healthyWhy = "the plugin reports itself unhealthy while every counter this suite checks is at zero. That means pkg/plugin's Healthy expression covers a condition floorCounters does not — a new healthy-affecting counter was added there without being mirrored here. The plugin's own verdict wins: it is the surface operators page on"

// CheckHealthFloor answers "is the plugin OK?" for a whole run, which
// is a different question from the per-test deltas in
// assertNoNewHealthFaults. Deltas catch "did this test break
// something"; the floor catches a fault that no individual test
// happened to bracket — including one left behind by the main suite
// before the failure suite even started.
//
// It returns findings for every healthy-affecting counter that is
// non-zero or unreported, and nothing at all for a clean run. Each
// counter yields at most one finding.
//
// The values are ABSOLUTE, not deltas from the start of the run, and
// that is the point: an absolute floor is what notices a fault that
// predates the first test. The cost is that running against a
// long-lived plugin (a local box where the plugin has been up across
// several sessions) can report a counter from an earlier run, so the
// findings say "since plugin start" out loud.
//
// A counter the plugin does not publish is itself a fatal finding.
// That case is not hypothetical: an old build left installed on a dev
// box answers /Plugin.Health without join_start_failures at all, and
// before #377 the floor read the resulting zero as clean — weaker
// locally than in CI while looking identical. The same silence would
// follow a renamed JSON tag in CI, where the plugin is always built
// from the branch under test, so this is not a dev-box-only guard.
//
// Note this is deliberately NOT `!h.Healthy`, though it is now one
// step away from it. Every counter behind that flag means exactly
// one thing since #376 split the benign container-exit, and #648 the
// removed network, out of recovery_failed; what is left is wanting a
// few runs of evidence
// before promoting recovery_failed to fatal, because the cost of
// getting that wrong is a red suite nobody can explain. When it is
// promoted, this table collapses into a single check of h.Healthy.
func CheckHealthFloor(h *HealthResponse) []FloorFinding {
	if h == nil {
		return nil
	}
	var out []FloorFinding
	for _, c := range floorCounters {
		if h.published != nil {
			if _, ok := h.published[c.name]; !ok {
				out = append(out, FloorFinding{
					Counter: c.name,
					Absent:  true,
					Fatal:   true,
					Why:     absentWhy,
				})
				continue
			}
		}
		if v := c.read(h); v > 0 {
			out = append(out, FloorFinding{
				Counter: c.name,
				Value:   v,
				Fatal:   c.fatal,
				Why:     c.why,
			})
		}
	}

	// The plugin's own verdict, checked last and independently of the
	// table above (#421).
	//
	// Every counter in floorCounters is now fatal, so in principle this
	// is redundant — and that is exactly why it is worth having. The
	// table is this suite's *mirror* of pkg/plugin's Healthy
	// expression, and a mirror drifts: add another healthy-affecting
	// counter to the plugin and the floor keeps reporting clean until
	// somebody remembers this file. Asking the plugin directly closes
	// that gap without waiting for the mirror to catch up.
	//
	// Absence is judged too, on the same principle as the counters: a
	// payload with no `healthy` key decodes to false, which would
	// otherwise fail every run for the wrong reason and teach everyone
	// to ignore it.
	//
	// Judged only on a decoded payload. `published == nil` means the
	// value was built by hand rather than received, and there a false
	// Healthy is the zero value of an unset bool, not the plugin saying
	// anything — exactly the distinction the counters' own presence
	// check already makes. Reading it as a verdict would fail every
	// unit test that builds a literal to exercise one counter.
	if h.published == nil {
		return out
	}
	if _, ok := h.published[healthyKey]; !ok {
		return append(out, FloorFinding{
			Counter: healthyKey,
			Absent:  true,
			Flag:    true,
			Fatal:   true,
			Why:     absentWhy,
		})
	}
	if !h.Healthy {
		// Only reported when nothing else already explains it —
		// otherwise every real fault would print twice, once named and
		// once as this catch-all, and the named one is more useful.
		if len(out) == 0 {
			out = append(out, FloorFinding{
				Counter: healthyKey,
				Flag:    true,
				Fatal:   true,
				Why:     healthyWhy,
			})
		}
	}
	return out
}

// FloorFailed reports whether any finding is fatal. Split from
// CheckHealthFloor so callers print every finding and fail on a
// subset, rather than choosing between reporting and enforcing.
func FloorFailed(findings []FloorFinding) bool {
	for _, f := range findings {
		if f.Fatal {
			return true
		}
	}
	return false
}

// floorEvidenceMaxFaultLines bounds the fault section. A run that
// produced more fault lines than this has a much bigger problem than
// the one the floor is reporting, and the tail plus the on-disk path
// still lead a reader to the rest.
const floorEvidenceMaxFaultLines = 200

// FloorEvidence picks the parts of a plugin log worth printing when the
// floor fails, and returns them ready to write to stderr.
//
// The floor runs in TestMain after m.Run(), where no test's cleanup is
// in scope and DumpPluginLog's *testing.T is not available — so before
// this, a floor failure printed a counter and nothing else, leaving the
// evidence sitting on disk unread (#385). On CI that disk is an
// ephemeral runner, so "sitting on disk" means gone.
//
// Two sections, both bounded:
//
//   - every error- and warning-level line, wherever it falls in the run.
//     This is not a heuristic: each counter the floor can report is
//     incremented next to a log.Error or log.Warn at every one
//     of its increment sites, so the line that explains a finding is
//     always in this section. Warnings are included as well as errors
//     because the counter's own line is sometimes a Warn (the tombstone
//     write failure is), and because the warning before an error is
//     usually the half that says why.
//   - the last tailLines lines, for the sequence leading up to the end
//     of the run, which the fault lines alone do not give.
//
// The full log stays on the runner; callers print its path alongside
// this so a reader who needs everything knows where everything is.
func FloorEvidence(logData []byte, tailLines int) string {
	lines := strings.Split(strings.TrimRight(string(logData), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return "  (plugin log is empty)\n"
	}

	var faults []string
	for _, l := range lines {
		if strings.Contains(l, "level=error") || strings.Contains(l, "level=warning") {
			faults = append(faults, l)
		}
	}

	var b strings.Builder
	if len(faults) == 0 {
		// Worth saying out loud rather than printing an empty heading:
		// a counter moved but the log carries no error or warning, which
		// means the counter and its log line have drifted apart.
		b.WriteString("  no error- or warning-level lines in the plugin log — " +
			"a counter moved without logging, which is itself a defect\n")
	} else {
		shown := faults
		if len(shown) > floorEvidenceMaxFaultLines {
			shown = shown[len(shown)-floorEvidenceMaxFaultLines:]
			fmt.Fprintf(&b, "  --- last %d of %d error/warning lines ---\n",
				len(shown), len(faults))
		} else {
			fmt.Fprintf(&b, "  --- %d error/warning lines ---\n", len(faults))
		}
		for _, l := range shown {
			b.WriteString("  " + l + "\n")
		}
	}

	if tailLines > 0 {
		tail := lines
		if len(tail) > tailLines {
			tail = tail[len(tail)-tailLines:]
		}
		fmt.Fprintf(&b, "  --- last %d lines ---\n", len(tail))
		for _, l := range tail {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}

// floorFullCoverageRatio is how much of the suite the plugin's uptime
// has to span before the floor's verdict counts as covering the run.
// Slightly under 1 because the plugin was already up when the suite
// started, but the two clocks are read at different moments and the
// fixture setup between them is not free.
const floorFullCoverageRatio = 0.98

// floorPredatesRunSeconds is how far the plugin's uptime has to exceed
// the suite before the line says so. In CI the plugin is installed for
// the run, so the two are seconds apart and a note would be noise; on a
// local run against a plugin that has been up for hours the counters
// carry history the run did not produce, and a bare "clean" reads as a
// verdict on the run alone. Five minutes separates those two worlds
// without firing on ordinary install-then-run slack.
const floorPredatesRunSeconds = 300

// FloorCleanLine renders the floor's verdict when nothing was found.
//
// It exists because "clean" on its own was a lie worth fixing. The
// counters reset whenever the plugin process does, and three tests
// recycle it, so on a main-suite run the floor often sees only the last
// ~80 seconds of eleven minutes — and said `clean — no healthy-affecting
// counter moved` regardless. Run #379 printed exactly that for a run
// that did contain a real fault (#383), erased by a later respawn. A
// headline that reads "clean" gets quoted as evidence, so it has to
// carry what it actually looked at (#385).
//
// suite is the wall-clock the suite took. A zero or negative value
// means the caller could not measure it, and the qualifier is dropped
// rather than guessed at.
//
// The number that follows "run" is always suiteSeconds. Uptime is a
// different quantity and is labelled as one: the full-coverage branch
// used to print uptime under the word "run", which on a local run
// against a long-lived plugin read `the whole 15479s run` for a suite
// that took 61 seconds (#474). Same failure as the one #385 fixed — a
// quotable "clean" that does not carry what it looked at — arrived at
// from the other side.
func FloorCleanLine(h *HealthResponse, suiteSeconds float64) string {
	if h == nil {
		return ""
	}
	if suiteSeconds <= 0 {
		return fmt.Sprintf(
			"HEALTH FLOOR: clean — no healthy-affecting counter moved over the plugin's %.0fs uptime.\n"+
				"  The suite's own duration was not measured, so how much of the run that spans is\n"+
				"  unknown (healthy=%v).\n",
			h.UptimeSeconds, h.Healthy)
	}
	if h.UptimeSeconds >= suiteSeconds*floorFullCoverageRatio {
		line := fmt.Sprintf(
			"HEALTH FLOOR: clean — no healthy-affecting counter moved over the whole %.0fs run\n"+
				"  (plugin up %.0fs, so its counters span it; healthy=%v)\n",
			suiteSeconds, h.UptimeSeconds, h.Healthy)
		if h.UptimeSeconds-suiteSeconds > floorPredatesRunSeconds {
			line += fmt.Sprintf(
				"  The plugin predates this run by %.0fs, so these counters also carry history\n"+
					"  from before it. Clean is therefore stronger than this run needed — but had\n"+
					"  anything moved, it could not have been pinned on this run either.\n",
				h.UptimeSeconds-suiteSeconds)
		}
		return line
	}
	return fmt.Sprintf(
		"HEALTH FLOOR: clean over the last %.0fs of a %.0fs run — %.0f%% of it (healthy=%v).\n"+
			"  The plugin restarted mid-suite and its counters reset with it, so this verdict\n"+
			"  says nothing about the earlier %.0fs. The per-test deltas cover that stretch.\n",
		h.UptimeSeconds, suiteSeconds, 100*h.UptimeSeconds/suiteSeconds, h.Healthy,
		suiteSeconds-h.UptimeSeconds)
}

// AttachGraceLine reports how many attaches finished only because of
// the daemon-busy grace (#406), and is printed on every run.
//
// It exists because the census going to zero does not, on its own, mean
// the grace is what did it: these failures are intermittent — runs have
// scored 6, 5, 3 and 0 against unchanged code — so one clean run proves
// nothing. join_attach_slow moving is positive evidence of the
// mechanism rather than an absence of failures, and the two together
// are what an argument for the fix rests on.
//
// A run with zero failures AND zero slow attaches says only that the
// condition did not arise; it is not a pass, and the wording says so
// rather than leaving a reader to assume.
func AttachGraceLine(h *HealthResponse, joinFailures int) string {
	if h == nil {
		return ""
	}
	switch {
	case h.JoinAttachSlow > 0 && joinFailures == 0:
		return fmt.Sprintf(
			"ATTACH GRACE: %d attach(es) finished only after outlasting AWAIT_TIMEOUT, and none\n"+
				"  were abandoned. Before #406 each of those was a running container left with no\n"+
				"  renewal client. This is the fix working, observed rather than inferred.\n",
			h.JoinAttachSlow)
	case h.JoinAttachSlow > 0:
		return fmt.Sprintf(
			"ATTACH GRACE: %d attach(es) needed the grace, and %d still failed. The grace is not\n"+
				"  sufficient for every case (#406).\n", h.JoinAttachSlow, joinFailures)
	case joinFailures == 0:
		return "ATTACH GRACE: no attach needed the grace this run. The daemon-busy window did not\n" +
			"  arise, so this run is not evidence either way about #406.\n"
	default:
		return fmt.Sprintf(
			"ATTACH GRACE: %d Join failure(s) and no attach used the grace. The failures are not\n"+
				"  the daemon-busy mechanism #406 describes — look elsewhere.\n", joinFailures)
	}
}

// ConflictProbeLine reports whether the address-conflict detector
// actually ran, which address_conflicts alone cannot say. Zero
// conflicts and zero probes are the same reading, and "nothing checked"
// is exactly what #524 looked like in production for months — green
// health, every counter at zero, a container on somebody else's
// address.
//
// Same shape as AttachGraceLine above, for the same reason: a zero that
// could mean either "the mechanism worked" or "the condition never
// arose" is not evidence until something distinguishes them.
func ConflictProbeLine(h *HealthResponse) string {
	if h == nil {
		return ""
	}
	switch {
	case h.AddressConflicts > 0:
		return fmt.Sprintf(
			"CONFLICT PROBE: %d leased address(es) were already held by another device on the\n"+
				"  segment, out of %d probe(s). The floor fails on this — see above (#524).\n",
			h.AddressConflicts, h.AddressConflictProbes)
	case h.AddressConflictProbes == 0:
		return "CONFLICT PROBE: no probe reached a verdict this run, so address_conflicts=0 is not\n" +
			"  evidence the segment was clean — it is the absence of a measurement. Either no\n" +
			"  endpoint was leased a v4 address, or the detector did not run (#524).\n"
	case h.ConflictProbeFailures > 0:
		return fmt.Sprintf(
			"CONFLICT PROBE: %d probe(s) reached a verdict and found no conflict, but %d could not\n"+
				"  run at all. The clean verdict covers only the endpoints that were checked (#524).\n",
			h.AddressConflictProbes, h.ConflictProbeFailures)
	default:
		return fmt.Sprintf(
			"CONFLICT PROBE: %d probe(s) reached a verdict, none found a conflict, none failed.\n"+
				"  The detector ran and the segment was clean — observed, not inferred (#524).\n",
			h.AddressConflictProbes)
	}
}

// joinStartFailureMsg is the log line the plugin emits at every real
// join_start_failures increment. The benign twin logs something else
// ("Container went away during attach"), so counting this message counts
// exactly the faults, with no classification logic duplicated here.
const joinStartFailureMsg = "Failed to start persistent DHCP client"

// fatalFaultSignature ties a healthy-affecting counter to the log line
// the plugin writes when it bumps it.
//
// The counters reset with the plugin process and the main suite recycles
// it, so CheckHealthFloor's verdict has only ever covered the stretch
// since the last restart — measured at 10% of one run and 12% of
// another. The log does not reset, so counting these lines is the same
// verdict over the whole run. That is the remaining half of #385; the
// Join half already worked this way and is what caught three failures a
// green run had hidden.
//
// join_start_failures is deliberately absent: it has its own census
// because it groups by cause, and counting it here as well would double
// it in the total.
//
// msg must be a substring of the line the plugin actually logs.
// TestFatalFaultSignaturesExistInPluginSource pins every one of them
// against pkg/plugin, because a reworded log line would silently turn
// this census into a constant zero — an absent measurement wearing the
// costume of a clean one.
type fatalFaultSignature struct {
	counter string
	msg     string
	why     string
}

var fatalFaultSignatures = []fatalFaultSignature{
	{
		counter: "tombstone_write_failures",
		msg:     "Failed to persist tombstone",
		why:     "a container restart may come back with a different MAC and IP",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: NetworkInspect failed",
		why:     "a whole network was skipped during post-restart recovery",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: failed to load network options",
		why:     "a whole network was skipped during post-restart recovery",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: endpoint recovery failed",
		why:     "an endpoint was not rebuilt after a plugin restart",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: daemon never became reachable",
		why:     "recovery gave up entirely; every previously-attached endpoint is running without renewal",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: persistent DHCP client Start failed",
		why:     "a running container's lease will not renew until it is restarted",
	},
}

// FaultCensus counts healthy-affecting faults other than Join failures
// across the WHOLE plugin log, and returns the total plus a report.
//
// Returns 0 and "" for a clean run so it stays quiet, and so a caller
// cannot mistake a report for a verdict — the count is the verdict.
func FaultCensus(logData []byte) (int, string) {
	lines := strings.Split(string(logData), "\n")
	counts := map[string]int{}
	whys := map[string]string{}
	total := 0
	for _, sig := range fatalFaultSignatures {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, sig.msg) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		total += n
		counts[sig.counter+": "+sig.msg] = n
		whys[sig.counter+": "+sig.msg] = sig.why
	}
	if total == 0 {
		return 0, ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PLUGIN FAULTS: %d across the whole run "+
		"(the log spans the run; the counters only span the last plugin restart)\n", total)
	for _, k := range sortedKeys(counts) {
		fmt.Fprintf(&b, "  %3d  %s\n       %s\n", counts[k], k, whys[k])
	}
	return total, b.String()
}

// JoinFailureCensus counts Join-start failures across the WHOLE plugin
// log, and summarises what they were.
//
// This exists because the counter cannot answer the question. Counters
// reset when the plugin process does, and the main suite recycles it
// three times, so join_start_failures at the end of a run describes only
// the last ~80 seconds (#385). One run showed the gap plainly: twelve of
// these failures in the log, and a counter reading 1.
//
// The log has no such limit — it spans the run. So for "did this run
// produce Join failures, and why", the log is the instrument and the
// counter is not. Printed on every run, clean or not: this is the number
// that says whether the Join budget is sized for the host it is running
// on (#401), and a number you only see when something else already went
// red is a number you cannot use to prevent anything.
//

// JoinFailureCount counts the same failures the census summarises, for
// callers that need the number rather than the prose.
//
// Separate from JoinFailureCensus because the census is a diagnostic and
// this is a verdict, and they answer to different pressures: a
// diagnostic may be reworded freely, a verdict may not change what it
// counts without someone deciding to.
func JoinFailureCount(logData []byte) int {
	n := 0
	for _, l := range strings.Split(string(logData), "\n") {
		if strings.Contains(l, joinStartFailureMsg) {
			n++
		}
	}
	return n
}

// Returns an empty string when there were none, so a healthy run stays
// quiet.
func JoinFailureCensus(logData []byte) string {
	reasons := map[string]int{}
	total := 0
	for _, l := range strings.Split(string(logData), "\n") {
		if !strings.Contains(l, joinStartFailureMsg) {
			continue
		}
		total++
		reasons[joinFailureReason(l)]++
	}
	if total == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "JOIN FAILURES: %d across the whole run "+
		"(the log spans the run; join_start_failures only spans the last plugin restart)\n", total)
	for _, r := range sortedKeys(reasons) {
		fmt.Fprintf(&b, "  %3d  %s\n", reasons[r], r)
	}
	return b.String()
}

// joinFailureReason pulls the error= field out of a logrus text line, so
// the census groups by cause rather than listing every occurrence. A
// timeout waiting for the Docker API and a container that vanished are
// different problems and should not be summed into one number.
func joinFailureReason(line string) string {
	const key = `error="`
	i := strings.Index(line, key)
	if i < 0 {
		return "(no error field)"
	}
	rest := line[i+len(key):]
	// logrus quotes the value and escapes any inner quote, so the first
	// unescaped quote ends it.
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' {
			j++
			continue
		}
		if rest[j] == '"' {
			return rest[:j]
		}
	}
	return rest
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- the conflict-probe census gate (#551) --------------------------
//
// ConflictProbeLine above already distinguishes the three states that
// matter. Nothing acted on the third. From #527 merging until #550,
// every single run printed
//
//	CONFLICT PROBE: 1 probe(s) reached a verdict and found no conflict,
//	  but 2 could not run at all.
//
// because the macvlan/ipvlan fixture's parent carried no on-subnet
// address, so the detector could not run on two of the three attachment
// modes. The line said so on every run and the suite went green
// throughout. That is the failure the detector itself exists to prevent,
// one level up: "nothing checked" and "nothing found" must not read the
// same. The instrument was right; there was no gate behind it.
//
// WHY THE OBVIOUS GATE IS THE WRONG ONE. "Endpoints were created and no
// probe reached a verdict" sounds like the property, and it never fires
// on the case above — one probe DID reach a verdict there. The blindness
// was in the two that could not run, so the failures are what must be
// judged, against what the suite legitimately expects.
//
// Two things would otherwise make this fail for reasons unrelated to the
// property:
//
//  1. TestAddressConflict_BareParentIsUndetermined (#541) drives the
//     degraded path ON PURPOSE and increments conflict_probe_failures.
//     Failing on failures > 0 would break a correct test, so a test that
//     means to degrade a probe declares it with AllowConflictProbeFailures
//     and the gate judges the excess.
//  2. The floor runs per shard, and a shard whose tests lease no v4
//     address legitimately reaches zero probes. Failing on
//     address_conflict_probes == 0 alone would make the verdict depend on
//     how the partitioner happened to balance that run — a gate whose
//     result depends on shard assignment is worse than no gate.
//
// The premise that makes "leases but no probes" sound: the post-lease
// conflict probe is NOT opt-in. checkAddressConflict runs for every
// endpoint that received a v4 address, on both the bridge and the
// parent-attached paths. (The opt-in validate_dhcp preflight is a
// different probe entirely and does not touch these counters.) And
// leases_obtained is v4-only — v6 has its own counter — so a v6-only
// shard cannot trip this.

// conflictAllowance accumulates the probe failures this shard EXPECTS,
// declared by the tests that cause them deliberately.
//
// Package-level and mutex-guarded rather than plumbed through: the floor
// runs in TestMain after every test has finished, so there is no value
// to thread and no ordering to get wrong. A test declares its intent
// where it degrades the probe, which is the only place that knows.
var conflictAllowance struct {
	mu sync.Mutex
	n  int32
}

// AllowConflictProbeFailures declares that n conflict-probe failures are
// expected in this shard, because a test degrades a probe on purpose.
//
// Call it from the test that does the degrading, next to the degrading,
// so the declaration cannot drift away from its reason. Anything beyond
// the declared count fails the run.
func AllowConflictProbeFailures(n int32) {
	conflictAllowance.mu.Lock()
	defer conflictAllowance.mu.Unlock()
	conflictAllowance.n += n
}

// AllowedConflictProbeFailures reports the total declared so far.
func AllowedConflictProbeFailures() int32 {
	conflictAllowance.mu.Lock()
	defer conflictAllowance.mu.Unlock()
	return conflictAllowance.n
}

// base returns a usable baseline, so callers with none (a hand-built
// HealthResponse, an older lane, a failed baseline read) get zeroes and
// therefore the old whole-plugin-life behaviour. Judging more than this
// process caused is the safe direction; judging less would hide a real
// failure, which is the outcome this census exists to prevent.
func base(b *HealthResponse) *HealthResponse {
	if b == nil {
		return &HealthResponse{}
	}
	return b
}

// deltaSincePluginStart converts a cumulative counter into what this
// process is answerable for.
//
// A value BELOW the baseline means the plugin restarted mid-run and its
// counters went back to zero. The current value is then already scoped
// to the restart — narrower than this process, not wider — so it is
// used as-is rather than clamped to zero. Clamping would report "no
// probes failed" for a run in which the plugin died, which is precisely
// the shape of #385: the counters reset, the floor saw the tail, and a
// run with three failed Joins went green.
func deltaSincePluginStart(now, was int32) int32 {
	if now < was {
		return now
	}
	return now - was
}

// ConflictCensusFindings judges whether the conflict-probe census is
// evidence or an alibi, given how many failures the shard declared.
//
// Returns nil when there is nothing to say — including the honest
// nothing of a shard that leased no v4 address.
// conflictProbeFailureMsgs is every log line the plugin writes at a
// conflict_probe_failures increment (pkg/plugin/conflict_probe.go).
// Listed rather than pattern-matched so that adding a fourth failure
// path without adding it here is the only way to under-count, and so a
// reader can check the list against the source by eye.
var conflictProbeFailureMsgs = []string{
	"[conflict-probe] address-conflict probe could not run",
	"[conflict-probe] cannot parse leased address; address conflict not checked",
	"[conflict-probe] cannot parse endpoint MAC; address conflict not checked",
}

// ConflictProbeFailuresInLog counts probe failures across the WHOLE run.
//
// This exists because the counters do not. The plugin's counters live in
// its process, the main suite recycles that process, and the floor's own
// output says so: "the plugin restarted mid-suite and its counters reset
// with it ... this verdict says nothing about the earlier 209s". A
// counter-only census would therefore read clean for every probe that
// failed before the last restart — the same shape as #385, which is why
// the Join half already counts log lines instead.
func ConflictProbeFailuresInLog(logData []byte) int {
	if len(logData) == 0 {
		return 0
	}
	n := 0
	for _, l := range strings.Split(string(logData), "\n") {
		for _, msg := range conflictProbeFailureMsgs {
			if strings.Contains(l, msg) {
				n++
				break
			}
		}
	}
	return n
}

// ConflictCensusFindings judges the census. observedInLog is the count
// from ConflictProbeFailuresInLog; the larger of it and the counter wins,
// because the counter can only ever under-report after a restart and the
// log can only under-report if a line was lost.
func ConflictCensusFindings(h *HealthResponse, allowed int32, observedInLog int, baseline *HealthResponse) []FloorFinding {
	if h == nil {
		return nil
	}
	// An absent counter is not a zero. If the plugin never published
	// these, the census cannot be judged at all, and saying so is the
	// point — silently treating <not reported> as 0 would rebuild the
	// blindness this closes.
	if h.published != nil {
		for _, k := range []string{"address_conflict_probes", "conflict_probe_failures"} {
			if _, ok := h.published[k]; !ok {
				return []FloorFinding{{
					Counter: k,
					Absent:  true,
					Fatal:   true,
					Why: "the plugin did not publish this, so whether the address-conflict " +
						"detector ran cannot be established. address_conflicts=0 is not evidence " +
						"without it (#551).",
				}}
			}
		}
	}

	var out []FloorFinding

	// Counters are cumulative for the PLUGIN's life; the allowance is
	// declared by THIS process. Comparing them directly is only correct
	// when the plugin was started for this run, which the sharded lanes
	// happen to guarantee and the coverage lane does not — it drives one
	// instrumented plugin through both suites back to back. There, the
	// main suite's deliberately-degraded probe (declared, allowed, fine)
	// was still on the counter when the failure suite's process started
	// with an allowance of 0, and the floor called it unexplained.
	//
	// Wrong in the other direction too, and that is the worse one: a
	// failure that happened before this process started would be
	// attributed to it, and a real one that happened after a restart
	// could be masked by subtracting.
	probeFailures := deltaSincePluginStart(h.ConflictProbeFailures, base(baseline).ConflictProbeFailures)
	probes := deltaSincePluginStart(h.AddressConflictProbes, base(baseline).AddressConflictProbes)
	leases := deltaSincePluginStart(h.LeasesObtained, base(baseline).LeasesObtained)

	failures := probeFailures
	if int32(observedInLog) > failures {
		failures = int32(observedInLog)
	}

	if excess := failures - allowed; excess > 0 {
		why := fmt.Sprintf(
			"%d conflict probe(s) could not run at all; %d is the number this shard declared "+
				"as deliberate, so %d went unexplained. Each one is an endpoint whose address was "+
				"never checked, and address_conflicts=0 does not cover them — that reading is "+
				"exactly how #524 stayed invisible in production for months (#551).",
			failures, allowed, excess)
		out = append(out, FloorFinding{
			Counter: "conflict_probe_failures",
			Value:   failures,
			Fatal:   true,
			Why:     why,
		})
	}

	// The detector never even tried, on a shard that leased addresses
	// for it to check. Distinct from the case above: there, probes were
	// attempted and failed; here nothing was attempted at all.
	if probes == 0 && failures == 0 && leases > 0 {
		out = append(out, FloorFinding{
			Counter: "address_conflict_probes",
			Value:   0,
			Fatal:   true,
			Why: fmt.Sprintf(
				"%d v4 lease(s) were obtained and the conflict detector was never invoked for "+
					"any of them. The probe is not opt-in, so this is the detector having stopped "+
					"working rather than a shard with nothing to check (#551).",
				h.LeasesObtained),
		})
	}

	return out
}

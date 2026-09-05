# Security Policy

## Reporting a vulnerability

Please report suspected vulnerabilities **privately** via GitHub's
security advisory form:

**<https://github.com/claymore666/docker-net-dhcp/security/advisories/new>**

Do not open a public issue for anything you believe is exploitable.
You can expect an initial response within a few days; this is a
solo-maintained project, so please allow a reasonable window for a
fix before any public disclosure (90 days is a fine default).

**Response process.** On receipt the maintainer triages and confirms
the report, fixes the issue in a released version, and publishes a
GitHub Security Advisory. **Reporters are credited** in the advisory
and the fix's release notes unless they ask to remain anonymous.

## Scope — what this plugin is

`docker-net-dhcp` is a privileged Docker network plugin: it runs with
the host PID namespace, host networking, and the Docker socket mounted.

`config.json` at the repository root **requests** four capabilities —
`CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE` — and
that is what the daemon asks you to approve at `docker plugin install`.
It is **not** the effective set. Docker composes a plugin's capabilities
*additively* over the OCI defaults, so the process ends up with
seventeen: the four above plus the default set, which already contains
`CAP_NET_RAW`. Read off a running plugin process:

```
$ grep CapEff /proc/<plugin-pid>/status
CapEff: 00000000a82c35fb
$ capsh --decode=00000000a82c35fb
```

**`CAP_NET_RAW` is requested from the 2.0 beta onward, and the request
is what changed, not the power.** The DHCP exchange runs on an
interface that has no address yet, which needs an `AF_PACKET` socket,
and that is the ordinary path for every endpoint rather than an
optional feature. The plugin process already held the capability before
the beta, because it is in the OCI default set — the read above is from
a 1.x process. What `config.json` controls is the set the daemon
**shows you and asks you to approve** at install and on upgrade, so
adding the line makes every operator re-approve while the effective set
stays exactly as it was. If you are prompted on an upgrade, that is
this change; it is recorded in the release notes so a prompt nobody
intended can be told apart from this one.

### Every grant in `config.json`, one sentence each

One row per privilege the manifest asks for, naming what in this tree
consumes it. `scripts/check-privilege-sentences.sh` fails when this
table and `config.json` disagree in either direction — a grant with no
row, or a row for a grant that is no longer requested — so a privilege
that is dropped cannot go on reading as granted here.

The rows say what the grant is FOR. They are not a claim that nothing
else in the process could use it: a capability is held process-wide, and
the effective set is the seventeen above, not these four.

<!-- privilege-sentences: begin -->

| grant | what it is for | consumer |
|---|---|---|
| `network:host` | The plugin resolves and reads the parent link of every macvlan/ipvlan network, and any `METRICS_ADDR` listener binds, on the host's own network namespace rather than in a namespace of its own. | `pkg/plugin/parent_gate.go`, `cmd/net-dhcp/metrics.go` |
| `pidhost` | Two consumers, and only one of them is the network namespace: the fallback route into a container's netns via `/proc/<pid>/ns/net`, and every `resolv.conf` write, which enters the container's MOUNT namespace through `/proc/<pid>/ns/mnt` and has no sandbox-key equivalent. | `pkg/plugin/resolvconf.go`, `pkg/plugin/container_netns.go` |
| `mount:/var/run/docker.sock:bind` | The Docker API, read-only: `NetworkList`, `NetworkInspect` and `ContainerInspect`, which is where a container's hostname for DHCP option 12 comes from. Anything but GET and HEAD is refused before it is sent. | `pkg/plugin/docker_client.go`, `pkg/plugin/docker_transport.go` |
| `mount:/var/lib/net-dhcp:rbind,rw` | `STATE_DIR`: the lease record, per-network options, tombstones and the audit ledger, which must survive `docker plugin rm` and upgrade. | `pkg/plugin/state.go` |
| `mount:/var/run/docker:rbind,ro` | Read-only. The daemon's sandbox netns entries: the route tried first into a container's network namespace, which carries a recovery after a plugin restart, and the evidence that separates "the container went away mid-attach" from a plugin fault. | `pkg/plugin/sandbox_netns.go`, `pkg/plugin/network.go` |
| `CAP_NET_ADMIN` | Every address, route, MTU and link change the plugin applies inside a container's network namespace, and the parent/child link creation that attaches it. | `pkg/plugin/dhcp_manager.go`, `pkg/plugin/netlink_seam.go` |
| `CAP_NET_RAW` | The `AF_PACKET` socket the DHCP exchange runs on — the interface has no address yet, so an ordinary UDP socket cannot carry it — and the RFC 5227 ARP probes on the same socket family. | `internal/dhcp-golib/runtime/transport_packet_linux.go`, `internal/dhcp-golib/runtime/arp_linux.go` |
| `CAP_SYS_ADMIN` | `setns` into a container's network namespace on a locked OS thread, and into its mount namespace for a `resolv.conf` write. | `pkg/dhcp/chassis.go`, `pkg/plugin/resolvconf.go` |
| `CAP_SYS_PTRACE` | Opening `/proc/<pid>/ns/*` of a container whose init runs as a non-root user: the kernel gates it on `PTRACE_MODE_READ`, which a uid mismatch fails without this capability (#317). Both `/proc/<pid>/ns/mnt` for `resolv.conf` and the netns fallback route need it. | `pkg/plugin/resolvconf.go`, `pkg/plugin/container_netns.go` |

<!-- privilege-sentences: end -->

**The sandbox-key route was measured, and it carries a re-adoption but
not an attach.** The plugin asks first for the key the daemon publishes
under `/var/run/docker/netns/`. Whether that key resolves depends on one
thing: libnetwork creates each entry as an ordinary empty file and
bind-mounts the namespace over it, and the plugin's read-only
`/var/run/docker` is a bind mount taken when the *plugin process*
starts — a snapshot, not a subscription. A sandbox that already existed
at that moment is reachable through its key; one created afterwards is
not, and the key resolves to the empty file underneath.

A `Join` is always for a sandbox younger than the plugin, so
`/proc/<pid>/ns/net` carries **every attach**, as it always has. The one
case that goes the other way is recovery after a plugin restart, where
the container predates the new plugin process: there the key route is
used and the container's PID is never read. The plugin verifies that
what it opened is a network namespace before using it, so the refusal is
immediate and counted rather than surfacing later as a dead persistent
client.

That is why `pidhost` and `CAP_SYS_PTRACE` are unchanged, and it is now
two independent reasons rather than one: the network namespace, above,
and `resolv.conf` propagation, which enters the container's **mount**
namespace by PID and for which no sandbox key exists at all.

On such a host, per attach, `sandbox_key_entry_failures` and
`sandbox_pid_fallbacks` each rise by one and `sandbox_key_entries` stays
flat; after a plugin restart `sandbox_key_entries` rises once per
recovered endpoint instead. If attaches start counting under
`sandbox_key_entries`, the daemon's sandbox mounts are reaching this
plugin — a newer engine, or a different mount configuration — and the
netns half of these two grants is no longer load-bearing on your host.
None of that is `healthy`-affecting: a fallback that succeeds is a
working endpoint, and it costs a privilege rather than a lease. The
per-attach log line naming the refused key is at `debug`, because on a
stock engine it is correct on every attach and there is nothing to do
about it; the counters carry the signal at every level.

**Which refusal, and how you can tell.** The paragraph above names one
cause — the entry is the placeholder file, because the daemon's bind
mount came after the plugin's own. A second cause produces the same
`sandbox_key_entry_failures` count and wants the opposite response: a
daemon started with a non-default `--exec-root` publishes sandbox keys
under `<exec-root>/netns/`, which this plugin does not accept and
refuses on sight. The two are separated by the arm counters —
`sandbox_key_not_a_namespace` for the first, `sandbox_key_not_permitted`
for the second, which since 2.0-alpha.1 means a key that EXISTS and was
refused (`sandbox_key_absent`, the endpoint no key was published for at
all, `sandbox_key_wrong_ns_type` and `sandbox_key_unavailable` are the
remaining three; all five sum to `sandbox_key_entry_failures`).

The claim in this section is the first, and it is asserted rather than
argued: `TestSandboxKeyRoute_Macvlan`, `_Bridge`, `_Ipvlan` and
`_NonRootContainer` in `test/integration/sandbox_key_route_test.go` each
require `sandbox_key_not_a_namespace` to rise by exactly one per attach
and the other three arms to stay flat, and
`TestRecovery_PluginDisableEnable_PreservesEndpoint` in
`test/integration/recovery_test.go` requires all five to be zero on the
recovered instance — the same key form, on the same daemon, in the same
run, refused there and accepted here.

**The bound on that claim**, stated rather than closed: those cells
measure this lane's daemon, which runs the default `--exec-root`. On a
host where it is not the default, the refusal you see is
`sandbox_key_not_permitted`, the paragraph above does not describe what
happened, and the remedy is a change to this plugin rather than to your
host.

**What the measurement covers, and what it does not.** The integration
lane runs the cells above on bridge, macvlan and ipvlan, for a root and
a non-root container init, and across a plugin restart, on one rootful
daemon. Rootless Docker and `dockerd --userns-remap` are outside it and
are not claimed. Whether giving `/var/run/docker` slave mount
propagation in `config.json` would make the attach route work as well is
an open question this lane has not been asked; the recovery case shows
the key route itself is sound when the mount is visible, so propagation
is the indicated experiment — but it is a manifest privilege change and
it is recorded rather than assumed.

### Pointing the plugin at a read-only Docker socket proxy

The plugin's whole use of the Docker API is three read calls plus the
client library's version ping. `DOCKER_HOST` (empty by default, which
keeps the mounted socket) lets an operator put a proxy in front of it,
so a compromise of the plugin cannot reach the API calls that start a
container.

The proxy must allow exactly:

```
GET  /_ping                       and  HEAD /_ping
GET  /v1.*/networks
GET  /v1.*/networks/{id}
GET  /v1.*/containers/{id}/json
```

A worked example. The proxy listens on a **TCP endpoint on the host's
loopback**, which the plugin reaches because it runs with host
networking:

```
# 1. run any HTTP proxy that forwards only the paths above to
#    /var/run/docker.sock, listening on 127.0.0.1:2375
# 2. point the plugin at it
docker plugin disable claymore666/docker-net-dhcp:<tag>
docker plugin set    claymore666/docker-net-dhcp:<tag> DOCKER_HOST=tcp://127.0.0.1:2375
docker plugin enable claymore666/docker-net-dhcp:<tag>
```

The socket bind mount in `config.json` stays either way — it is what
the default value resolves to, and dropping it would break every
installation that sets nothing.

**Three bounds, said here rather than found later.**

*A proxy on its own unix socket does not work, and the reason is the
manifest.* The plugin sees exactly the paths `config.json` mounts, and
the socket mount's `source` is fixed at `/var/run/docker.sock` with no
`settable` field — so `docker plugin set … DOCKER_HOST=unix:///run/
docker-ro.sock` names a path that does not exist inside the plugin. A
unix-socket proxy is only reachable if it listens at the manifest's own
source path, which means displacing the daemon's socket for every other
client on the host. Use the TCP form above.

*A TLS endpoint is not supported.* Nothing in the plugin reads
`DOCKER_CERT_PATH` or configures a TLS client, so the endpoint has to be
a plain one on a host the plugin already trusts — loopback, in practice.

*The plugin's own refusal is a backstop, not the boundary.* It refuses
unsafe methods before sending them and counts each one
(`docker_api_non_get_refusals`), which means the plugin and the proxy
fail the same way. It does not make the proxy unnecessary: the plugin is
the thing being defended against, and a refusal it implements itself is
a refusal an attacker who controls it can remove.

Assume the effective set when reasoning about a report, not the four
in `config.json`. Reports are especially welcome for:

- container → host or container → plugin escapes through the netns /
  mount-ns handling (`pkg/plugin`, `pkg/dhcp`). Both namespaces are still
  entered: the plugin `setns`es a locked thread into a container's network
  namespace to open the DHCP socket, and into its **mount** namespace on
  every `resolv.conf` write (`pkg/plugin/resolvconf.go`);
- parsing of untrusted DHCP-server responses: the wire decoders in the
  in-tree DHCP library (`internal/dhcp-golib/wire`), the chassis that
  turns a decoded lease into a plugin event
  (`pkg/dhcp/chassis.go`, `pkg/dhcp/info_sanitize.go`), and lease/option
  propagation into containers;
- anything that lets one container influence another container's
  lease, address, or DNS (cross-endpoint identity confusion).

A hostile **LAN DHCP server** is partially in scope: the plugin
necessarily trusts the server for addressing (that is its job), but
memory-unsafe or injection-style handling of server-supplied bytes is
a bug.

### The optional metrics port

Since v1.8.0 the plugin can expose a Prometheus `/metrics` endpoint over
TCP, enabled only by setting `METRICS_ADDR` (see
[Reference](docs/reference.md#metrics)). It is **off by default** and
should be bound to loopback or a management interface.

Two properties are deliberate and worth reporting against if either
turns out to be false:

- the TCP listener serves `/metrics` and nothing else — the libnetwork
  RPCs that create networks and join endpoints are not routed on it,
  which matters because this listener is on the **host** network
  namespace, not a container's;
- the exposition carries aggregate counters only. No endpoint IDs,
  container names, addresses or MACs appear in it, so scraping it does
  not disclose which container holds which lease.

Read that second property narrowly: it is a statement about `/metrics`,
not about the plugin. The plugin does have a surface that records which
container holds which lease — see the lease ledger below.

### The lease ledger on disk

`/metrics` carries nothing per-container. `STATE_DIR/leases.jsonl`
carries everything, and it is the more interesting file of the two.

With `audit_log=true` on a network, every lease-lifecycle event —
`bound`, `renew`, `stopped`, `stop_failed` — is appended as one JSON
object holding **network, endpoint, container, hostname, IP and MAC**:
a complete container ↔ IP ↔ MAC correlation, at rest.

Three properties are deliberate and worth reporting against if any is
false:

- it is **off by default**, per network, and nothing enables it
  implicitly;
- the file is mode `0600`, as is everything the plugin writes under
  `STATE_DIR`, and an upgrade tightens what it finds rather than leaving
  older files open (#708);
- it is bounded — rotated at 16 MB or 30 days, one rotated generation
  kept, so ~32 MB worst case.

What it is **not** is ephemeral. `STATE_DIR` is `/var/lib/net-dhcp` on
the **host**, bind-mounted `rw` into the plugin, so the ledger survives
`docker plugin disable`, `docker plugin rm` and upgrade — the supported
upgrade path leaves it exactly where it was. Removing the plugin does
not remove the correlation data it wrote; that is a deliberate
deletion.

### The plugin's UNIX socket

`/metrics` is also served on the plugin's UNIX socket unconditionally,
which is unchanged ground: that socket is `root`-only, and anything able
to read it can already call every RPC. Since v1.8.0 the plugin sets that
mode itself (`0600`) instead of inheriting whatever the plugin runtime's
umask happened to give it, and a test fails if the socket ever comes up
readable by group or other.

## Supported versions

Only the latest released version is supported with security fixes.
There is no backport policy — upgrades are cheap (`docker plugin
install` of the new tag).

## Known accepted findings

Vulnerabilities detected in dependencies that have **no fixed
release** are documented with justification and a review date in
[`.github/vuln-allowlist.txt`](.github/vuln-allowlist.txt) and
re-evaluated by CI on every PR (govulncheck gate) plus a weekly
scheduled scan. Entries there are deliberate, reviewed acceptances —
not oversights.

## Security assurance case

A short argument for why the plugin's security posture is adequate for
its purpose.

**Security goals.** (1) The plugin must not let a container escape to
the host or to another container via its netns/mount-ns handling. (2)
Untrusted, attacker-influenced input — chiefly DHCP-server responses —
must not cause memory-unsafe behaviour or injection. (3) Published
artifacts must be tamper-evident so users install what was built.

**Threats** (see *Scope* above). The plugin is privileged
(`CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE`,
host PID ns, Docker socket), so the
relevant adversaries are a malicious container, a hostile LAN DHCP
server supplying crafted lease/option bytes, and a supply-chain
attacker tampering with distributed images.

**Mitigations and why they suffice.**
- *Memory safety / injection:* the plugin is written in Go
  (memory-safe). Server-supplied bytes are decoded by the in-tree DHCP
  library's wire codec, which is a nested Go module with its own test
  suite, pinned by SHA in `internal/dhcp-golib/SOURCE` and checked
  byte-for-byte on every PR by `scripts/check-dhcp-golib-copy.sh`; every
  string value that reaches a container or a log first passes
  `pkg/dhcp.SafeValue`, which refuses control characters and counts the
  refusal (`unsafe_option_values_dropped`). **The named bound:** this
  repository's own fuzz step is a leftover of the 1.x parsers and no
  longer names a target that exists here, so no fuzzing runs in this
  branch's lane — the library's suite is where the codec is exercised,
  and re-pointing the step is tracked work rather than a claim made
  here.
- *Escape / cross-container confusion:* the live integration suite drives
  real DHCP exchanges through bridge/macvlan/ipvlan against a real
  kernel and daemon, including recovery, tombstone-stability, and
  concurrency scenarios; the race detector runs in unit CI.
- *No cryptography of its own:* the plugin performs no encryption,
  authentication, or key handling, so whole classes of crypto-misuse
  threats do not apply.
- *Supply chain:* releases are cosign-signed (keyless), carry SLSA build
  provenance and an SBOM, and ship a signed `checksums.txt`; CI gates
  every change with `govulncheck`, CodeQL, `staticcheck`, and Dependency
  Review (see the [README](README.md#verifying-releases)).

**Residual risk.** The plugin necessarily trusts the DHCP server for
*addressing* (that is its function); a hostile server can hand out bad
addresses/routes, which is a network-design concern outside the
plugin's control. This is accepted and documented in *Scope*.


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

`config.json` at the repository root **requests** three capabilities —
`CAP_NET_ADMIN`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE` — and that is what
the daemon asks you to approve at `docker plugin install`. It is **not**
the effective set. Docker composes a plugin's capabilities *additively*
over the OCI defaults, so the process ends up with seventeen, the three
above plus the default set — `CAP_NET_RAW` among them. Read off a
running plugin process:

```
$ grep CapEff /proc/<plugin-pid>/status
CapEff: 00000000a82c35fb
$ capsh --decode=00000000a82c35fb
```

Assume the effective set when reasoning about a report, not the three
in `config.json`. Reports are especially welcome for:

- container → host or container → plugin escapes through the netns /
  mount-ns handling (`pkg/plugin`, `pkg/dhcp`);
- parsing of untrusted DHCP-server responses (the `dhcpcd` hook-event
  path: `cmd/dhcp-handler`, `pkg/dhcp.BuildEvent`, lease/option
  propagation into containers);
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
(`CAP_NET_ADMIN`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE`, host PID ns,
Docker socket), so the
relevant adversaries are a malicious container, a hostile LAN DHCP
server supplying crafted lease/option bytes, and a supply-chain
attacker tampering with distributed images.

**Mitigations and why they suffice.**
- *Memory safety / injection:* the plugin is written in Go (memory-safe)
  and the untrusted DHCP-response parsers (`pkg/dhcp.BuildEvent`, the
  handler-pipe JSON decoder) have native fuzz targets plus seed corpora
  run on every PR, so malformed server input is exercised, not assumed
  safe.
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


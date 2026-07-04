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
`CAP_NET_ADMIN`/`CAP_SYS_ADMIN`, the host PID namespace, host
networking, and the Docker socket mounted. Reports are especially
welcome for:

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
(`CAP_NET_ADMIN`/`CAP_SYS_ADMIN`, host PID ns, Docker socket), so the
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


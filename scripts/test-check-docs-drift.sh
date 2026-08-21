#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-docs-drift.sh (#345). Synthesizes a Go
# package with a HealthResponse struct and a DHCPNetworkOptions struct,
# a plugin manifest, and a docs tree — then asserts each of the checks
# bites, that the waiver mechanism works, and that a fully documented
# tree with cross-links passes.
#
# The cover-manifest cases (rule 2b) carry their own growth case: a
# THIRD cover-only setting, documented, must pass. A gate that encoded
# today's GOCOVERDIR/REQUEST_CAPTURE_DIR would satisfy every other case
# while blocking the next instrumentation knob.
#
# The duplication cases mirror the two real shapes that drifted before
# this gate existed: a settings *table* and a counter *bullet list*.
set -u

CHECK="$(dirname "$0")/check-docs-drift.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PKG="$TMP/pkg"
DOCS="$TMP/docs"
mkdir -p "$PKG" "$DOCS"

cat > "$PKG/endpoints.go" <<'EOF'
package plugin

type HealthResponse struct {
	Healthy       bool  `json:"healthy"`
	LeasesRenewed int32 `json:"leases_renewed"`
	// A comment line inside the struct body.
	NAKsReceived int32 `json:"naks_received"`
}

type DHCPNetworkOptions struct {
	Mode   string `mapstructure:"mode"`
	Bridge string
}
EOF

cat > "$TMP/config.json" <<'EOF'
{
  "env": [ { "name": "LOG_LEVEL", "value": "info", "settable": ["value"] } ],
  "mounts": [
    { "type": "bind", "source": "/var/lib/net-dhcp", "destination": "/var/lib/net-dhcp" }
  ]
}
EOF

write_reference() {
    cat > "$DOCS/reference.md" <<'EOF'
# reference
| `healthy` | x |
| `leases_renewed` | x |
| `naks_received` | x |
| `LOG_LEVEL` | x |
| `mode` | x |
| `bridge` | x |
EOF
}

run() { MANIFEST="$TMP/config.json" bash "$CHECK" "$PKG" "$DOCS" "$DOCS/reference.md" 2>&1; }

fail=0
expect() { # <want-exit> <label> <must-contain-or-empty>
    local want="$1" label="$2" needle="${3:-}" out rc
    out=$(run); rc=$?
    if [ "$rc" -ne "$want" ]; then
        echo "FAIL: $label — exit $rc, want $want"; printf '    %s\n' "$out"; fail=1; return
    fi
    if [ -n "$needle" ] && ! printf '%s' "$out" | grep -F "$needle" >/dev/null; then
        echo "FAIL: $label — output missing '$needle'"; printf '    %s\n' "$out"; fail=1; return
    fi
    echo "PASS: $label"
}

# 1. Everything documented once → clean.
write_reference
rm -f "$DOCS/guide.md"
expect 0 "fully documented tree passes" "docs-drift gate passed"

# 2. A counter the code returns but the reference never names.
write_reference
sed -i '/naks_received/d' "$DOCS/reference.md"
expect 1 "undocumented counter fails" "counter naks_received"

# 3. A settable env var missing from the reference.
write_reference
sed -i '/LOG_LEVEL/d' "$DOCS/reference.md"
expect 1 "undocumented plugin setting fails" "setting LOG_LEVEL"

# 4. A second page documenting a setting in a table row.
write_reference
# shellcheck disable=SC2016  # backticks are markdown, not shell
printf '# guide\n| `LOG_LEVEL` | duplicate |\n' > "$DOCS/guide.md"
expect 1 "duplicate setting in a table row fails" "guide.md documents \`LOG_LEVEL\` in a table row"

# 5. A second page documenting a counter as a definition bullet — the
#    shape that actually drifted in the real docs.
write_reference
# shellcheck disable=SC2016  # backticks are markdown, not shell
printf '# guide\n- `leases_renewed` — duplicate definition\n' > "$DOCS/guide.md"
expect 1 "duplicate counter in a bullet fails" "guide.md documents \`leases_renewed\` in a definition bullet"

# 6. A second page documenting a driver option in a table row.
write_reference
# shellcheck disable=SC2016  # backticks are markdown, not shell
printf '# guide\n| `mode` | duplicate |\n' > "$DOCS/guide.md"
expect 1 "duplicate driver option fails" "guide.md documents \`mode\` in a table row"

# 7. Cross-links and prose mentions are exactly what other pages should
#    keep — they must not trip the gate.
write_reference
cat > "$DOCS/guide.md" <<'EOF'
# guide
See [`LOG_LEVEL`](reference.md#plugin-settings) for verbosity, and read
about `leases_renewed` in the reference. Set `mode` at create time.
EOF
expect 0 "cross-links and prose do not trip the gate" "docs-drift gate passed"

# 8. An in-page waiver suppresses one name on one page, and says so.
write_reference
cat > "$DOCS/guide.md" <<'EOF'
# guide
<!-- docs-drift-ok: mode — comparison table, not the option -->
| `mode` | comparison |
EOF
expect 0 "in-page waiver is honoured" "WAIVED guide.md"

# 9. A waiver is scoped to its name — it must not blanket the page.
write_reference
cat > "$DOCS/guide.md" <<'EOF'
# guide
<!-- docs-drift-ok: mode — comparison table, not the option -->
| `mode` | comparison |
| `LOG_LEVEL` | duplicate |
EOF
expect 1 "waiver does not cover other names on the page" "documents \`LOG_LEVEL\`"

# 10. A page routing a reader through the plugin rootfs to a path the
#     manifest bind-mounts — the shape that survived #440 in two places
#     and sent operators to an empty directory (#489).
write_reference
rm -f "$DOCS/guide.md"
printf '# guide\nsudo cat /var/lib/docker/plugins/*/rootfs/var/lib/net-dhcp/leases.jsonl\n' > "$DOCS/guide.md"
expect 1 "rootfs route to a bind-mounted path fails" "routes a reader through the plugin rootfs"

# 11. The reference is not exempt from that one — it is where the real
#     stale path lived, so exempting it would gate everything but the
#     page that drifted.
write_reference
rm -f "$DOCS/guide.md"
printf 'sudo cat /var/lib/docker/plugins/*/rootfs/var/lib/net-dhcp/tombstones.json\n' >> "$DOCS/reference.md"
expect 1 "the reference is scanned too" "reference.md routes a reader"

# 12. A rootfs path to something the manifest does NOT mount is correct
#     and must stay: the plugin log really does live there.
write_reference
rm -f "$DOCS/guide.md"
printf '# guide\nsudo cat /var/lib/docker/plugins/*/rootfs/var/log/net-dhcp.log\n' > "$DOCS/guide.md"
expect 0 "an unmounted rootfs path is left alone" "docs-drift gate passed"

# 13. The README beside the docs tree is scanned as well — it carries
#     operator-facing recipes too.
write_reference
rm -f "$DOCS/guide.md"
printf '# readme\nsudo cat /var/lib/docker/plugins/*/rootfs/var/lib/net-dhcp/leases.jsonl\n' > "$TMP/README.md"
expect 1 "a README beside docs/ is scanned" "README.md routes a reader"
rm -f "$TMP/README.md"

# 14. A vanished struct is an explicit failure, not a vacuous pass —
#     the check must never go quiet because the code moved.
write_reference
rm -f "$DOCS/guide.md"
cat > "$PKG/endpoints.go" <<'EOF'
package plugin

type SomethingElse struct{}
EOF
expect 2 "renamed HealthResponse cannot gate" "could not extract HealthResponse"

# --- 5. a documented install creates the bind source it needs (#494) ----
# The mirror of the rootfs rule. That one is about where a reader is sent
# to LOOK; this one is about what must EXIST before the plugin starts.
# The real bug: reference.md's install block created the directory and
# its upgrade block, on the same page, did not — so the page as a whole
# looked fine and the procedure a reader actually follows did not work.

# Case 14 above deliberately renames the struct away to prove the gate
# refuses rather than passes vacuously. Everything after it needs the
# fixture put back, or these cases exit 2 on that check and never reach
# the rule they are about.
cat > "$PKG/endpoints.go" <<'EOF'
package plugin

type HealthResponse struct {
	Healthy       bool  `json:"healthy"`
	LeasesRenewed int32 `json:"leases_renewed"`
	// A comment line inside the struct body.
	NAKsReceived int32 `json:"naks_received"`
}

type DHCPNetworkOptions struct {
	Mode   string `mapstructure:"mode"`
	Bridge string
}
EOF

# The shape that shipped: two procedures, only one of them correct.
write_reference
rm -f "$DOCS/guide.md"
cat >> "$DOCS/reference.md" <<'EOF'

```bash
sudo mkdir -p /var/lib/net-dhcp
docker plugin install ghcr.io/ns/docker-net-dhcp:v1.0.0
```

```bash
docker plugin rm ghcr.io/ns/docker-net-dhcp:vOLD
docker plugin install ghcr.io/ns/docker-net-dhcp:vNEW
```
EOF
expect 1 "a second procedure missing the mkdir is caught" "installs the plugin without creating"

# Per-block, not per-page: the same page passes once both procedures
# create it. If this ever fails, the rule has quietly become per-page and
# the case above is passing for the wrong reason.
write_reference
cat >> "$DOCS/reference.md" <<'EOF'

```bash
sudo mkdir -p /var/lib/net-dhcp
docker plugin install ghcr.io/ns/docker-net-dhcp:v1.0.0
```

```bash
docker plugin rm ghcr.io/ns/docker-net-dhcp:vOLD
sudo mkdir -p /var/lib/net-dhcp
docker plugin install ghcr.io/ns/docker-net-dhcp:vNEW
```
EOF
expect 0 "each procedure is judged on its own" "docs-drift gate passed"

# Prose is not a procedure. A page discussing `docker plugin install`
# outside a fence must not be dragged in — otherwise the rule is
# unsatisfiable on any page that merely names the command.
write_reference
rm -f "$DOCS/guide.md"
cat > "$DOCS/guide.md" <<'EOF'
# guide
Privileges are granted interactively at `docker plugin install` time.
EOF
expect 0 "a prose mention is not a procedure" "docs-drift gate passed"

# The opt-out that keeps this honest: a source no documented procedure
# ever creates is daemon-owned (docker.sock) and not the operator's to
# make. Such a source must not turn every install block red.
write_reference
rm -f "$DOCS/guide.md"
cat > "$TMP/config-sock.json" <<'EOF'
{
  "env": [ { "name": "LOG_LEVEL", "value": "info", "settable": ["value"] } ],
  "mounts": [
    { "type": "bind", "source": "/var/run/docker.sock", "destination": "/run/docker.sock" }
  ]
}
EOF
cat >> "$DOCS/reference.md" <<'EOF'

```bash
docker plugin install ghcr.io/ns/docker-net-dhcp:v1.0.0
```
EOF
out=$(MANIFEST="$TMP/config-sock.json" bash "$CHECK" "$PKG" "$DOCS" "$DOCS/reference.md" 2>&1)
if [ $? -eq 0 ]; then
    echo "PASS: a daemon-owned source is not the operator's to create"
else
    echo "FAIL: a daemon-owned source is not the operator's to create"
    printf '%s\n' "$out" | sed 's/^/    /'
    fail=1
fi

# --- 2b. the second manifest -------------------------------------------
# config-cover.json declares settables config.json does not, and the
# gate used to read only config.json — so those were settings nothing
# had ever looked at. They are exempt from the operator reference by
# name (they do not exist on the shipped plugin) and owe the
# contributor page a definition instead.

cover_run() { # <cover-manifest> <contrib-doc>
    MANIFEST="$TMP/config.json" COVER_MANIFEST="$1" CONTRIB_DOC="$2" \
        bash "$CHECK" "$PKG" "$DOCS" "$DOCS/reference.md" 2>&1
}
cover_expect() { # <want-exit> <label> <cover-manifest> <contrib-doc> <needle>
    local want="$1" label="$2" cm="$3" cd="$4" needle="${5:-}" out rc
    out=$(cover_run "$cm" "$cd"); rc=$?
    if [ "$rc" -ne "$want" ]; then
        echo "FAIL: $label — exit $rc, want $want"; printf '%s\n' "$out" | sed 's/^/    /'; fail=1; return
    fi
    if [ -n "$needle" ] && ! printf '%s' "$out" | grep -F "$needle" >/dev/null; then
        echo "FAIL: $label — output missing '$needle'"; printf '%s\n' "$out" | sed 's/^/    /'; fail=1; return
    fi
    echo "PASS: $label"
}

write_reference
rm -f "$DOCS/guide.md"

mkdir -p "$TMP/cover"
cat > "$TMP/cover/config-cover.json" <<'EOF'
{
  "env": [
    { "name": "LOG_LEVEL", "value": "info", "settable": ["value"] },
    { "name": "GOCOVERDIR", "value": "/coverage", "settable": ["value"] }
  ],
  "mounts": [
    { "type": "bind", "source": "/var/lib/net-dhcp", "destination": "/var/lib/net-dhcp" }
  ]
}
EOF

# Documented where it lives → exempt, and the exemption is printed.
printf '# internals\nThe cover build writes counters to `GOCOVERDIR`.\n' > "$DOCS/internals.md"
cover_expect 0 "a cover-only setting documented in the contributor page is exempt" \
    "$TMP/cover/config-cover.json" "$DOCS/internals.md" "EXEMPT setting GOCOVERDIR"

# ...and the exemption is not a blanket: it must not be silent, and it
# must not swallow the shipped settings rule.
cover_expect 0 "a shipped setting is still judged against the reference" \
    "$TMP/cover/config-cover.json" "$DOCS/internals.md" "PASS  setting LOG_LEVEL documented"

# Documented nowhere → the gate bites. This is the shape that matters:
# a real operator setting landing in the cover manifest alone would
# otherwise escape rule 2 forever.
printf '# internals\nNothing about the cover build here.\n' > "$DOCS/internals.md"
cover_expect 1 "a cover-only setting documented nowhere fails" \
    "$TMP/cover/config-cover.json" "$DOCS/internals.md" "settable on the coverage plugin"

# Growth must be possible: a THIRD cover-only setting, documented,
# passes. A gate that encoded today's two would block the next one.
cat > "$TMP/cover/config-cover3.json" <<'EOF'
{
  "env": [
    { "name": "LOG_LEVEL", "value": "info", "settable": ["value"] },
    { "name": "GOCOVERDIR", "value": "/coverage", "settable": ["value"] },
    { "name": "REQUEST_CAPTURE_DIR", "value": "", "settable": ["value"] },
    { "name": "REPLAY_TRACE_DIR", "value": "", "settable": ["value"] }
  ],
  "mounts": [
    { "type": "bind", "source": "/var/lib/net-dhcp", "destination": "/var/lib/net-dhcp" }
  ]
}
EOF
cat > "$DOCS/internals.md" <<'EOF'
# internals
The cover build writes counters to `GOCOVERDIR`, request bodies to
`REQUEST_CAPTURE_DIR`, and replay traces to `REPLAY_TRACE_DIR`.
EOF
cover_expect 0 "a newly added, documented cover-only setting passes" \
    "$TMP/cover/config-cover3.json" "$DOCS/internals.md" "EXEMPT setting REPLAY_TRACE_DIR"

# A cover-only setting must NOT be required in the operator reference —
# it does not exist on the plugin a user installs. The reference above
# never mentions GOCOVERDIR, and that is the passing case just proven;
# this asserts the gate does not quietly demand it.
out=$(cover_run "$TMP/cover/config-cover3.json" "$DOCS/internals.md")
if printf '%s' "$out" | grep -F "setting GOCOVERDIR is settable on the plugin but absent" >/dev/null; then
    echo "FAIL: a cover-only setting is being demanded in the operator reference"; fail=1
else
    echo "PASS: a cover-only setting is not demanded in the operator reference"
fi

# The exemption needs somewhere to live. No contributor page, but
# cover-only settings exist → cannot check, not clean.
cover_expect 2 "a missing contributor page cannot carry the exemption" \
    "$TMP/cover/config-cover3.json" "$DOCS/nonexistent.md" "does not exist"

# No cover manifest at all: nothing to judge, said out loud rather than
# passed over in silence.
rm -f "$DOCS/internals.md"
write_reference
cover_expect 0 "an absent cover manifest is reported, not assumed" \
    "$TMP/cover/config-missing.json" "$DOCS/internals.md" "no cover-only settings to judge"

# The default resolution is part of the contract: with nothing but
# MANIFEST set, the cover manifest is looked for beside it. Everything
# above passes COVER_MANIFEST explicitly, which would leave a broken
# default undetected — and the default is what CI actually runs.
write_reference
cp "$TMP/cover/config-cover.json" "$TMP/config-cover.json"
printf '# internals\nThe cover build writes counters to `GOCOVERDIR`.\n' > "$DOCS/internals.md"
expect 0 "the cover manifest is found beside MANIFEST by default" "EXEMPT setting GOCOVERDIR"
rm -f "$TMP/config-cover.json" "$DOCS/internals.md"

if [ "$fail" -ne 0 ]; then
    echo "check-docs-drift tests FAILED"
    exit 1
fi
echo "all check-docs-drift tests passed"

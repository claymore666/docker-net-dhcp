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

# 13a. THE SAME FINDING, ON THE SAME PAGE, WITH THE PAGE UNREADABLE.
#      The case above is the control and it has to stay green either
#      side of this one, or this case is measuring the fixture rather
#      than the gate.
#
#      `grep` on an unreadable page exits 2 having matched nothing,
#      which reads as "this page documents nothing" -- a silent pass on
#      exactly the page a stale route would be hiding on. Rule 3 was
#      given a refusal for that; rules 4 and 5 were not, and README.md
#      is judged by rules 4 and 5 ALONE -- rule 3 iterates
#      `$DOCS_DIR/*.md` and never sees it. So the pages under `docs/`
#      were covered only incidentally (rule 3 exits 2 first) and this
#      one was covered by nothing.
#
#      Measured 2026-08-28 against the gate as it stood: exit 1
#      readable, **exit 0 and `docs-drift gate passed` at mode 000**,
#      with the identical finding sitting in the file.
chmod 000 "$TMP/README.md"
expect 2 "an unreadable README refuses instead of documenting nothing" \
    "is not a readable regular file"
chmod 644 "$TMP/README.md"
expect 1 "the same README readable is still reported" "README.md routes a reader"
rm -f "$TMP/README.md"

# 13b. `-f` folds "absent" together with "present but not a regular
#      file". Absent is legitimate -- the docs tree stands on its own --
#      so the test is `-e` and the refusal below decides the rest. A
#      DIRECTORY named README.md would otherwise leave the page set
#      without a word, which is the same silence one shape along.
mkdir -p "$TMP/README.md"
expect 2 "a directory named README.md refuses rather than vanishing" \
    "is not a readable regular file"
rmdir "$TMP/README.md"

# 13c. PRESERVATION for both of the above: with no README at all the
#      gate is clean, because absent really is a legitimate state and a
#      refusal that fired on it would red every repository without one.
expect 0 "no README beside docs/ is not an error" "docs-drift gate passed"

# 13d. Rule 5 reaches README.md too, and it is the rule the readability
#      refusal now runs in front of. Without a case here, "rules 4 and
#      5 both judge this page" would be a claim with one witness.
write_reference
printf '# readme\n\n```sh\nmkdir -p /var/lib/net-dhcp\ndocker plugin install x\n```\n\n```sh\ndocker plugin install x\n```\n' > "$TMP/README.md"
expect 1 "rule 5 reaches README.md as well" "README.md installs the plugin without creating"
chmod 000 "$TMP/README.md"
expect 2 "an unreadable README refuses before rule 5 can miss it" \
    "is not a readable regular file"
chmod 644 "$TMP/README.md"
rm -f "$TMP/README.md"
write_reference

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

# --- the option list is EVIDENCE, not intent (#839) --------------------
# Rule 3 judges a duplicate home against the names extracted at :196,
# and that awk took a bare `"$PKG_DIR"/*.go` glob with no readability
# guard and no emptiness backstop -- unlike the counter rule beside it,
# which has had one all along. awk says nothing about an operand it
# cannot open, and "no options in this package" and "I could not read
# the package" produced the same shorter list of names, so a real
# duplicate simply stopped being looked for.
#
# Measured 2026-08-28 with the duplicate below planted and the control
# confirmed red: a DIRECTORY named *.go gave mawk rc=0 (violation
# MISSED) while gawk skipped it and still reported; the SAME package
# with one source at mode 000 gave rc=0 under BOTH awks. The second arm
# is why the guard is readability rather than "handle a directory".
#
# The fixture is a directory rather than a mode-000 file because mode
# 000 is readable as root, and a case that quietly stops testing when
# the suite runs as root is the failure this file exists to prevent.
write_reference
printf '# second\n| `mode` | a second home for the mode option |\n' > "$DOCS/second.md"
expect 1 "a duplicate option home is reported (the control for the two below)" \
    "documents \`mode\` in a table row"

mkdir "$PKG/notafile.go"
expect 2 "an unreadable Go source refuses instead of shortening the option list" \
    "is not a readable regular file"
rmdir "$PKG/notafile.go"

# The same rule on the other side of the comparison. `grep` on an
# unreadable page exits 2 and matches nothing, which rule 3 read as
# "this page documents nothing" -- a silent pass on exactly the page a
# second home would be hiding on.
rm -f "$DOCS/second.md"
mkdir "$DOCS/second.md"
expect 2 "an unsearchable docs page refuses instead of reading as documenting nothing" \
    "could not be" 
rmdir "$DOCS/second.md"

# Preservation: with both restored to ordinary files and no duplicate,
# the widening must not have made every corpus refuse.
write_reference
expect 0 "an ordinary package and docs directory are still clean"

# The backstop's own two arms, each driven, because a guard with no case
# reports exactly what a working guard reports. Mutation found both of
# these missing: deleting the whole backstop, and dropping either half
# of it, left this suite green.
pkg_run() { # <pkg-dir>
    MANIFEST="$TMP/config.json" bash "$CHECK" "$1" "$DOCS" "$DOCS/reference.md" 2>&1
}
pkg_expect() { # <want-exit> <label> <pkg-dir> <needle>
    local want="$1" label="$2" dir="$3" needle="${4:-}" out rc
    out=$(pkg_run "$dir"); rc=$?
    if [ "$rc" -ne "$want" ]; then
        echo "FAIL: $label — exit $rc, want $want"; printf '    %s\n' "$out"; fail=1; return
    fi
    if [ -n "$needle" ] && ! printf '%s' "$out" | grep -F "$needle" >/dev/null; then
        echo "FAIL: $label — output missing '$needle'"; printf '    %s\n' "$out"; fail=1; return
    fi
    echo "PASS: $label"
}

# The EMPTINESS arm. A package whose sources parse but declare no
# options at all is indistinguishable, downstream, from a package this
# script failed to read -- and rule 3 would then look for a shorter list
# of names and report green having looked for fewer things.
mkdir -p "$TMP/pkg-nostruct"
cat > "$TMP/pkg-nostruct/endpoints.go" <<'EOF'
package plugin

type HealthResponse struct {
	Healthy bool `json:"healthy"`
}
EOF
pkg_expect 2 "a package declaring no options refuses instead of shortening the list" \
    "$TMP/pkg-nostruct" "could not extract DHCPNetworkOptions"

# No sources at all. The refusal that answers is the COUNTER rule's,
# not the option glob's: `counters` reads $PKG_DIR/endpoints.go and
# endpoints.go is itself a *.go file, so the glob cannot be empty
# unless that earlier guard has already refused. Measured -- this is
# why the empty-glob branch survives mutation, and the case asserts the
# ordering rather than a message that cannot appear. If the counters
# backstop is ever removed, this case still demands exit 2 and the
# empty-glob refusal is what has to carry it.
mkdir -p "$TMP/pkg-empty"
pkg_expect 2 "a package directory with no Go sources refuses, at the first guard that sees it" \
    "$TMP/pkg-empty" "could not extract HealthResponse"

# --- the awk seam: the STATUS half of the extraction backstop ----------
# Both extractions read awk's exit status, and with every Go operand
# proved readable in the shell first no source fixture can make awk fail
# while still printing. So that half was a branch with no case -- the
# same shape this gate was fixed for one rule along, and the reason its
# two sibling gates in this change each grew a seam. The stub prints a
# COMPLETE, plausible name list and then exits non-zero, so only the
# status can tell the gate something went wrong.
AWK_STUB="$TMP/awk-stub"
cat > "$AWK_STUB" <<'STUB'
#!/bin/sh
printf 'healthy\nleases_renewed\nnaks_received\nmode\nbridge\n'
exit 3
STUB
chmod +x "$AWK_STUB"
write_reference
rm -f "$DOCS/guide.md"
out=$(DOCS_DRIFT_AWK="$AWK_STUB" MANIFEST="$TMP/config.json" \
      bash "$CHECK" "$PKG" "$DOCS" "$DOCS/reference.md" 2>&1); rc=$?
if [ "$rc" -eq 2 ] && printf '%s' "$out" | grep -F "awk exit 3" >/dev/null; then
    echo "PASS: an awk that prints a full name list and then fails is a refusal"
else
    echo "FAIL: an awk that prints a full name list and then fails is a refusal — exit $rc"
    printf '    %s\n' "$out"; fail=1
fi

# Preservation for the seam itself: the same stub exiting 0 must be
# BELIEVED, or the case above would pass because the seam is broken
# rather than because the status is read.
cat > "$AWK_STUB" <<'STUB'
#!/bin/sh
printf 'healthy\nleases_renewed\nnaks_received\nmode\nbridge\n'
STUB
chmod +x "$AWK_STUB"
out=$(DOCS_DRIFT_AWK="$AWK_STUB" MANIFEST="$TMP/config.json" \
      bash "$CHECK" "$PKG" "$DOCS" "$DOCS/reference.md" 2>&1); rc=$?
if [ "$rc" -eq 0 ]; then
    echo "PASS: the same name list with status 0 is believed, not refused"
else
    echo "FAIL: the same name list with status 0 is believed, not refused — exit $rc"
    printf '    %s\n' "$out"; fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo "check-docs-drift tests FAILED"
    exit 1
fi
echo "all check-docs-drift tests passed"

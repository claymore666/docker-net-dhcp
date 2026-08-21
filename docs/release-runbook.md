# Release runbook

How to publish a `vX.Y.Z` of `docker-net-dhcp`. Written from the
v0.8.0 cycle, where we hit two operator-side gotchas (GHCR
package-link, Docker Hub token scope) that aren't reproducible
from the workflow file alone — capturing them here so the next
release isn't another archaeology session.

The goal: a clean release is one tag push, no manual steps.

## One-time prerequisites

These are per-account / per-Hub-repo setup plus the tooling on the
box you release from, **not** per-release. Done once when the
publishing chain is first wired up.

### Local tooling on the release box

The workflow installs everything it needs itself; this is only about
the commands **you** type. Nothing here is checked by CI, so a missing
tool surfaces as a step you skip rather than a gate that fails — which
is exactly how the v1.3.5 release ended up unable to run step 10's
verification, and how v1.5.0 was tagged before anyone noticed `cosign`
was absent, leaving the signature unverified locally until afterwards.

Twice is a class, so it has a check now. **Run this before step 1:**

```sh
bash scripts/check-release-tooling.sh
```

Exit 0 means every step below can actually be executed on this box. It
verifies `gh`, `cosign` **major 3**, and a configured
`user.signingkey`; `crane` is reported but optional. Its own
table-driven tests run in CI (`scripts/test-check-release-tooling.sh`),
so the check cannot rot into something that always passes.

| Tool | Needed for | Install |
| --- | --- | --- |
| `gh` | every step — PRs, milestones, run status, `gh release view` | distro package or <https://cli.github.com> |
| `cosign` | step 10's `verify-blob` re-verification | `go install github.com/sigstore/cosign/v3/cmd/cosign@latest` |
| `crane` | optional — comparing `:latest` and `:vX.Y.Z` digests by hand | `go install github.com/google/go-containerregistry/cmd/crane@latest` |

**Use cosign v3 or newer.** The release signs `checksums.txt` keylessly
and emits a **Sigstore bundle**, which is the v3 default; v2's
`--output-signature` / `--output-certificate` pair was removed in
favour of it. v3 is what the workflow itself installs and what the
v1.3.5 verification was run with (v3.1.2), and what verified v1.5.0
(v3.1.3); older majors are untested against
`checksums.txt.sigstore.json` here.

v2 does not fail with anything resembling "your cosign is too old" — it
fails with `Error: bundle does not contain cert for verification, please
provide public key`, which implicates the artifact rather than the
toolchain (#522). That string is now quoted on
[Verifying releases](verifying-releases.md) so a search for it lands on
the answer. `scripts/check-cosign-docs.sh` keeps every page that prints
a cosign command naming the same major as
`scripts/check-release-tooling.sh` enforces.

Also needed, but already true on any box that has committed here: a
git signing key, since step 9 tags with `-s`. Confirm with
`git config --get user.signingkey` before you get to the tag.

### GHCR — package must be linked to the repo

By default a workflow's `GITHUB_TOKEN` can push to GHCR packages it
**created** but not packages that already exist under the user/org.
This fork's `ghcr.io/claymore666/docker-net-dhcp` package was first
published manually before the release workflow existed, so on first
tag push the workflow gets `403 Forbidden` from GHCR even though
`permissions: packages: write` is set.

Fix it once at
<https://github.com/users/claymore666/packages/container/docker-net-dhcp/settings>:

1. **Manage Actions access** → **Add Repository** → pick
   `claymore666/docker-net-dhcp`.
2. Set role to **Write**.
3. Save.

Symptom if missed: workflow run logs show `error pushing plugin:
unexpected status from POST request to
https://ghcr.io/v2/.../blobs/uploads/: 403 Forbidden` at the **Push
to GHCR** step. The fix takes effect for the next workflow run; no
re-tag needed.

### GitHub Pages — enable the docs site

The versioned documentation site (mkdocs-material + mike, `#133`) is
built and published by `.github/workflows/pages.yml`. That workflow
pushes the rendered site to the `gh-pages` branch; GitHub Pages has to
be told to serve from it — once, after the branch first exists.

The first run on `dev` (or the first tag) creates `gh-pages`. Then, at
<https://github.com/claymore666/docker-net-dhcp/settings/pages>:

1. **Build and deployment → Source** = **Deploy from a branch**.
2. **Branch** = `gh-pages` / `(root)`. Save.

The site then resolves at <https://claymore666.github.io/docker-net-dhcp/>.
No per-release action: each `vX.Y.Z` tag publishes its own docs version
and moves the `latest` alias automatically (rc tags publish a preview
without moving `latest` — same guard as the image `:latest`). Until the
first release, the workflow points the site root at the moving `dev`
version so it isn't a 404.

### Docker Hub — secrets and scopes

The workflow's Hub steps are gated on a job-level
`HAS_HUB_CREDS` check. They skip cleanly when credentials are
absent (GHCR alone still publishes), so initial setup can be
deferred.

When you **do** want Hub published:

1. Create the repo on Hub (free): <https://hub.docker.com/repository/create>
   — name `net-dhcp`, namespace `claymore666`, visibility **Public**.
   The Hub UI doesn't auto-create plugin repos on first push the
   way it does for image repos; create it manually first.
2. Generate an access token at
   <https://app.docker.com/settings/personal-access-tokens>:
   - Description: something descriptive (`docker-net-dhcp release CI`).
   - **Access permissions: Read & Write** at minimum, but the
     description-sync step needs **admin** scope on the repo —
     read+write alone gets `401` on description PATCH. Picking
     "Read, Write & Delete" (the broadest permission level Hub
     offers personal tokens) covers both image push and
     description sync.
3. Add two repo secrets at
   <https://github.com/claymore666/docker-net-dhcp/settings/secrets/actions>:
   - `DOCKERHUB_USERNAME` = `claymore666`
   - `DOCKERHUB_TOKEN` = the token from step 2.

Symptom if scope is wrong: image push works, but the **Sync Docker
Hub description from README** step ends with
`401 Unauthorized` calling `PATCH /v2/repositories/...`. Regenerate
the token with the broader scope and re-run the workflow with
`gh workflow run release.yml -f tag=vX.Y.Z`.

### Workflow file must parse

GitHub Actions parses every workflow on every push, including
branch pushes that don't match the trigger. A parse error doesn't
fail loudly — it produces a "failed" run with no jobs and silently
**doesn't** trigger on tag pushes either. v0.8.0 hit this with
`if: ${{ secrets.X != '' }}` at step level (rejected; secrets
context isn't allowed in step-level `if`).

First line of defence: the `actionlint` job in the Test workflow
lints every workflow file on every PR (and
`scripts/test-actionlint.sh` asserts the linter still catches this
exact bug class). Second line: the **rc-tag dry-run** (next
section) exercises the whole publishing chain before every real
tag. Avoid dispatching `release.yml` with a bare existing release
tag — that *rebuilds and re-points* the tag and `:latest`
(different toolchain ⇒ different digest), mutating artifacts users
may have pinned.

### Pre-release dry-run (rc tags)

A tag with a pre-release suffix (`v1.0.0-rc1`) runs the release
workflow in **pre-release mode**: the full chain executes — build,
push of `:v1.0.0-rc1` to both registries, Hub description sync,
verify-install — but **`:latest` is not moved** and no bare
release tag is touched. Zero impact on anything a user pulls by
default.

Use it before every real release tag (step 9 below):

```sh
git checkout main && git pull --ff-only      # the release commit
git tag -s v1.0.0-rc1 -m "v1.0.0-rc1" && git push origin v1.0.0-rc1
```

Watch the run; every step including **verify-install** and, since
v1.7.0, **release-arm64** / **verify-install-arm64** must be green.

**The rc tag also starts the arm64 integration lane** (#531) — pushing
it triggers `integration-arm64` on its own. Nothing to dispatch, and
nothing to remember: the tag is the gate, so what the rc proves follows
from the tag. The lane still runs only against release candidates
(`v*-rc*` never matches a bare release tag), because the runner pool it
targets — label `dhcp-ci-arm64` — is provided for that window and not
for day-to-day PRs.

An arm64 runner has to be online for the rc. Since v1.8.0 (#632) that
needs no action: the board carries a standing runner that registers
itself at boot and reconnects on its own, so pushing the tag is the
whole procedure. Nothing to mint, nothing to launch. If the lane
reports no runner anyway, the board is down rather than unstarted —
that is a host problem, not a release step.

Two jobs to read, and they say different things:

- **`arm64-suite`** — the verdict. Must be green before the real tag.
- **`arm64-lane-present`** — red means the suite never *started*: no
  runner carried `dhcp-ci-arm64` within the wait, so this candidate has
  **no arm64 verdict at all**. It exists because a job with no runner
  sits queued for hours and queued renders as "in progress", which is
  indistinguishable from a lane still working. Treat it as "bring the
  board up and re-dispatch", never as a flake to wave through.

Inside `arm64-suite`, one step is worth knowing about before it
surprises you: **Verify the host's NFS-outage watchdog** (v1.8.0+,
#677). It asks the board whether it actually *booted* a working
watchdog, which the source-side check cannot know — that root is a
netbooted image and can predate a fix the tree already carries. If it
fails the suite fails, and it means the board is running unwatched: an
NFS outage will wedge it until someone visits it physically. Fix the
image; do not wave it through.

It can also come back **only partly verified**, which surfaces as a
workflow annotation and does *not* fail the step. That means the kernel
ring buffer has aged past this boot, so the watchdog's *holder* could
not be confirmed. The arming verdict comes from sysfs and is never the
part that is lost, so an annotation here does not block the tag.

To re-run the lane against a tag that already exists:

```sh
gh workflow run integration-arm64.yml --ref vX.Y.Z-rcN
```

The rc window is the **enforcement gate** for the
documentation review (procedure step 3): every PR on the milestone
must be reconciled against README, `docs/`, and the RELEASE_NOTES
section, and they must describe the version about to ship — if
stale text or an undocumented behaviour change surfaces now, fix it
before the real tag. Then tag the real release. Naming: rc of the *upcoming*
version (`v1.0.0-rc1` before `v1.0.0`) — semver orders it before
the release and it labels the content truthfully. Bump the rc
number for another attempt after a fix; never reuse an rc tag.

Cleanup (optional): rc plugin tags can be deleted from GHCR/Hub
after the real release ships; the git tag stays as the audit
trail.

## Per-release procedure

Pre-flight: every issue / PR going into the release should be on
the `vX.Y.Z` milestone (the workflow leans on this for the
"Closes" list in the release PR).

1. **Branch off `dev`:** `git checkout -b release/vX.Y.Z origin/dev`
2. **Bump install pins:** `scripts/bump-version.sh vX.Y.Z` (#251). It
   rewrites every published-image pin
   (`ghcr.io/claymore666/docker-net-dhcp:vPREV` in the `plugin install` /
   `network create` / `driver:` / `plugin inspect` snippets across
   `README.md` and `docs/`) to the new tag, and leaves bare `vX.Y.Z`
   feature markers and historical prose (`As of vPREV every PR...`,
   `v1.1.0 onward`) alone — the image ref is what tells a pin from
   prose. Verify with `git diff` and `scripts/check-version-pins.sh`
   (the same gate `test.yaml` runs: every pin must agree on one
   version). The gate also fails CI if a future hand-edit leaves the
   pins inconsistent.
3. **Documentation review — PR-driven against the milestone.** Don't
   review from memory; review from the change set. List every PR on the
   `vX.Y.Z` milestone and reconcile each one's user-visible change
   against the docs:

   ```sh
   gh pr list --state merged --limit 200 \
     --json number,title,milestone \
     --jq '.[] | select(.milestone.title=="vX.Y.Z") | "#\(.number) \(.title)"'
   ```

   For **each** merged PR, confirm the docs reflect what it changed:
   new/changed driver-opts land in the option tables (`README.md`,
   `docs/reference.md`, `docs/parent-attached-modes.md`); behaviour
   changes (Health counters, DHCP-client behaviour, identity, recovery)
   land in `reference.md` / `parent-attached-modes.md` / `internals.md`;
   examples and numbers match. **A milestone PR that changed
   user-visible behaviour but carries no doc delta is the signal to look
   harder, not to wave through** — that is exactly the drift that the
   #205↔#152 case (busybox→dhcpcd prose surviving a docs restructure;
   fixed in #234/#237) slipped through a memory-based read.

   Then still read everything user-visible top-to-bottom for anything
   the per-PR pass misses — `README.md` (feature list, driver-opt table,
   examples), `GOVERNANCE.md` and `SECURITY.md`, every file under
   `docs/` (including this runbook — process changes during the cycle
   land here too), and the coverage table if republished. Anything
   describing the previous version's behaviour, options, or numbers gets
   updated on the release branch now. Everything under `docs/` (plus
   `docs/index.md`, the site home) is what the versioned documentation
   site publishes for this tag, so the review *is* the site review —
   there's no separate wiki to reconcile.

   **Read the pages whole, and aim at the ungated prose.** The
   reference material defends itself: `check-option-docs.sh`,
   `check-docs-drift.sh` and `check-version-pins.sh` gate every driver
   option, health counter, plugin setting and image pin, so those tables
   are the *least* likely place to find drift. What rots is everything
   else — a walkthrough's shell snippet, a troubleshooting row, a
   sentence in a Behaviour section, a hand-maintained list. The v1.5.0
   pass found eight divergences (#489) and every one of them was in
   ungated prose; none would have been caught by grepping for keywords.

   **Some drift belongs to no milestone PR at all**, so the per-PR read
   cannot reach it by construction. Check these directly:

   - **Commands and paths that never existed or stopped working.**
     `docker plugin logs` was in the README's bug-report checklist and
     is not a Docker subcommand. Run the commands the docs tell a
     reader to run.
   - **Text invalidated by a feature in *this* release.** #440 mounted
     `STATE_DIR` from the host and left two recipes still routing
     operators through the plugin rootfs. A feature PR updates the
     section it is about; it rarely finds the other page that quietly
     depended on the old behaviour.
   - **Syntax deprecated upstream.** Compose, Docker CLI and dhcpcd move
     on their own schedule. `docker compose -f <snippet> config` prints
     the deprecation warnings for anything in a Compose example.
   - **Restated lists that live somewhere else.** Required CI checks,
     registries, privileges. Prefer replacing the copy with a pointer at
     the authority — the way step 5 defers to branch protection — over
     updating a copy that will decay again.
   - **Mechanisms this release added that no page describes.** The
     failure is *absence*, not wrongness, so nothing reads as wrong and
     no grep finds it. Take the release's mechanism changes and ask
     which section of `internals.md` covers each; v1.6.0 shipped a lease
     reclaim and a per-parent gate with no section for either, while
     every counter table was green.
   - **Standing preconditions that read like old-version prose.** The
     `BREAKING CHANGE IN v1.5.0` block (`README.md`, `docs/index.md`,
     `docs/reference.md`) is a precondition for *every* install, not a
     changelog entry — but it names an old version, so a pass tidying
     stale version references deletes it in good faith. **Keep it.**
   - **Counted claims.** "Four flip `healthy` to `false`" is right until
     a fifth is added, and the sentence still parses. Check any stated
     count against the code. That particular claim is now enforced by
     `scripts/check-health-contract.sh` (#638) — it had drifted for two
     releases *under this instruction*, which is the argument for a
     gate over a rule: the sentence is checked on every PR now, not
     when someone remembers to. The instruction still stands for every
     other counted claim, none of which has one.

   **Verify each finding against the artifact, not from reasoning.**
   Run the command, `config` the snippet, `ls` the path on the test
   box, query the API (`gh api repos/.../branches/dev/protection`). A
   confidently-argued divergence that turns out to be wrong costs more
   than the one it replaced.

   **A finding that is a class, not an incident, ends in a gate.** Same
   rule as anywhere else in this project: if the same shape of staleness
   can recur on the next mount, option or workflow change, add the check
   rather than a promise to remember. The rootfs-path finding above
   became a fourth rule in `check-docs-drift.sh`, deriving the
   bind-mount destinations from `config.json`, so that class now fails
   loudly.

   **The badge answers are documentation too.** `.bestpractices.json`
   at the repo root holds this project's OpenSSF Best Practices answers
   — one `<criterion>_status` plus a `<criterion>_justification` each —
   and every justification is a claim about the repository that can go
   stale exactly like prose. Reconcile it against the milestone the same
   way, then check it against the live entry:

   ```sh
   python3 scripts/badge-sync.py --diff
   ```

   If a milestone PR earned or invalidated a criterion (a new gate, a
   document that now exists, a policy that changed), update the file on
   the release branch.

   Getting the reviewed answers *onto* the live entry is manual and
   deliberate: the badge site takes them through its own form, one
   **criteria level** at a time —
   `https://www.bestpractices.dev/en/projects/13229/{passing,silver,gold}/edit`,
   in a browser you are signed into. A field only appears on the level
   that owns it, and the level-less `/edit` URL 404s for everyone
   including the owner, which is worth knowing before it looks like an
   expired login.

   **Hash-check every justification you paste before submitting** — the
   procedure is in the script's header. Typing an answer by hand is how
   this project once produced a 65-field divergence from its own
   source of truth; the hash is what makes hand-entry safe. Then run
   `--diff` again: it must report that the entry matches. That
   confirmation is the point of the script, and the reason it has no
   push mode.

   The work happens here, on the release branch. The rc dry-run (step 8)
   is the **enforcement gate**: the real `vX.Y.Z` tag does not ship
   until every milestone PR is ticked off against the docs — by the real
   tag, text and code (and the published site) must agree.
4. **Add a `## vX.Y.Z` section** to `RELEASE_NOTES.md`, **above
   the previous version's section**. Summarise what's changing in
   user-visible terms; the workflow doesn't auto-build this from
   commit messages. Include any **operator-visible compatibility
   notes** (e.g. v0.8.0 narrowed the `IsDHCPPlugin` regex — that
   needed a callout).

   **Credit outside contributions by name**, the way the v1.0.0 notes
   do. Find them rather than recalling them — almost every PR here is
   the maintainer's or Dependabot's, so an outside one is easy to miss
   precisely because it is rare:

   ```sh
   gh pr list --state merged --limit 300 --json number,title,author \
     --jq '.[] | select(.author.login|test("claymore666|dependabot")|not)
                | "#\(.number) @\(.author.login) \(.title)"'
   ```

   Also confirm the merged commit still carries their authorship
   (`git log -1 --format='%an <%ae>' <sha>`) — a rebase or squash of a
   fork branch is where that quietly becomes the maintainer's.
5. **PR `release/vX.Y.Z` → `dev`.** Required checks on `dev` are
   `test`, `staticcheck`, `integration` (every PR builds and exercises
   its own plugin on the integration runner), `actionlint`,
   `govulncheck`, `attribution`, and CodeQL's `Analyze (go)` +
   `Analyze (actions)` — eight in total. Merge when green.

   `main` requires those eight **plus `coverage`**, which is why the
   ratchet first bites at the release PR in the next step and not
   before. Branch protection is the authority here; if this list and
   the settings disagree, the settings win and this list is the thing
   to fix.
6. **Open the release PR `dev` → `main`** with title
   `Release vX.Y.Z` and a `Closes #N` line for **every issue** in
   the milestone. The list is what auto-closes them when the PR
   merges; without it the milestone stays open after the tag.

   **Because the list is milestone membership, membership has to be
   true.** An issue that is in the milestone but not done gets closed
   as delivered, silently, by the tag. The taxonomy already says
   `backlog` never sits on a milestoned issue for exactly this reason,
   and the **Milestone scope** workflow
   (`.github/workflows/milestone-scope.yml`,
   `scripts/check-milestone-scope.sh`) checks it daily rather than
   leaving it to whoever builds the list. It splits the two cases,
   because their fixes are opposite: `backlog` **with** `in-dev` means
   the work shipped and the label is stale (drop the label, keep the
   milestone); `backlog` **without** it means the work has not started
   (move it off the milestone). Read that run before opening the
   release PR — it is a schedule, so a red one waits quietly.
   Release PRs additionally run the **Coverage** workflow with the
   coverage ratchet (`scripts/coverage-ratchet.sh` vs
   `.github/coverage-baseline.txt`): no release ships with less
   per-package coverage than the previous one. If a package beat its
   floor during the cycle, raise the baseline as part of the release
   branch.

   Coverage shares a concurrency group with the release PR's own
   integration run, so it normally starts once integration finishes —
   roughly twelve minutes in. A `coverage` check still showing nothing
   after that is worth the next paragraph.

   **Release PR blocked on a check that has no run.** A required check
   that was *cancelled* looks exactly like one that is *pending*: the
   PR sits at `BLOCKED` with nothing to click into. It is not a missing
   trigger. GitHub keeps one running plus one pending run per
   concurrency group, so pushing another commit to the release PR while
   coverage is still queued displaces it — and a run displaced before
   any job was assigned creates no check run at all, which is why the
   `coverage` context goes absent rather than red.

   You should not have to notice this yourself: the **Coverage
   presence** check (`.github/workflows/coverage-presence.yml`, #504)
   watches the head and fails with the run id and the exact recovery
   command when the run was evicted. If it is red, do what it says. The
   manual form, for a head it did not cover:

   ```sh
   gh run list --workflow coverage.yml --limit 5   # look for "cancelled"
   gh run rerun <id>                               # once the group is idle
   ```

   Wait for the integration run on the same ref to finish before
   rerunning, or it will just queue and be displaced again. This cost a
   full debugging session on v1.3.5 (#365) — the fix is thirty seconds
   once you know the shape of it.
7. **Assemble the verification evidence, don't hand-write it.**
   ```sh
   scripts/run-evidence.sh "$(git rev-parse 'HEAD^{tree}')"
   ```
   Prints every integration run that tested exactly this tree, with its
   window and what else was on the privileged pool at the time. Paste
   it into the release PR rather than reconstructing it from memory.

   Read the overlap line literally. `none — ran alone` and `unknown`
   are different claims: the second means the concurrent-run list did
   not reach back far enough to judge, which happens once the repo has
   been busy since. Do not upgrade an `unknown` to "ran alone" — the
   v1.4.0 write-up asserted a concurrency caveat that the data did not
   support, in both directions, which is what #432 was filed about.

8. **Merge the release PR.** Squash or merge commit — both fine;
   match what's in `git log`.
9. **Pull main, dry-run, then tag:** first push `vX.Y.Z-rc1` and
   confirm the workflow run is green end-to-end (pre-release mode,
   `:latest` untouched — see "Pre-release dry-run" above). Then:
   ```sh
   git checkout main && git pull --ff-only
   git tag -s vX.Y.Z -m "vX.Y.Z — <one-liner>"   # signed (#175)
   git push origin vX.Y.Z
   ```
   Use `-s` (signed) so the release tag shows **Verified** on GitHub —
   the dev box has `tag.gpgsign=true` so `-a` would also sign, but spell
   it out so it holds from any checkout. Confirm with
   `git tag -v vX.Y.Z` (or the green "Verified" on the tag page).
   The workflow fires on `tags: v*`. Watch it at
   <https://github.com/claymore666/docker-net-dhcp/actions/workflows/release.yml>.
   Expected steps: Resolve tag → checkout → setup-go →
   GHCR login → Hub login (or skip) → Push to GHCR → Push to
   Hub (or skip) → Sync Hub description → Install cosign →
   **Sign published images (cosign)** → **Generate SBOM (syft)** →
   **Package and sign release artifact** → **Attest provenance
   (artifacts + image)** → Workflow summary →
   **verify-install** (separate job: installs the just-published
   plugin from GHCR on a clean hosted runner and asserts it
   enables — a red verify-install means users can't install what
   we just shipped) → **github-release**.
   Since v1.7.0 the run carries a parallel arm64 chain (#507):
   **release-arm64** (native `ubuntu-24.04-arm` build, pushes
   `vX.Y.Z-arm64` / `latest-arm64` — per-arch tags, because a Docker
   plugin cannot install from a manifest list) and
   **verify-install-arm64**. Both gate `github-release` exactly like
   their amd64 twins, and every green checklist below includes them.
10. **Confirm the GitHub Release** — the `github-release` job now cuts
   it automatically once `verify-install` is green (so a plugin that
   doesn't install never gets an advertised Releases page). It attaches
   the cosign-signed artifacts and builds the body as: a generated lead
   line naming the project and version (it becomes the page's
   `og:description`, so it is what link previews show — #469), the
   `## vX.Y.Z` section of `RELEASE_NOTES.md`, a generated Downloads
   table, and a link to [Verifying releases](verifying-releases.md).
   Step 4's notes must therefore already be in place at tag time.
   rc tags produce a **draft** release: the publish path is still
   exercised, but dry-run builds stay out of the public list.
   No manual `gh release create` — instead verify:
   ```sh
   gh release view vX.Y.Z   # body = the RELEASE_NOTES section; assets:
                            #   net-dhcp-plugin-vX.Y.Z-linux-amd64.tar.gz
                            #   net-dhcp-plugin-vX.Y.Z-linux-arm64.tar.gz
                            #   checksums.txt + checksums.txt.sigstore.json
                            #   checksums-arm64.txt + checksums-arm64.txt.sigstore.json
   # Re-verify the signature the way a downstream consumer would:
   cosign verify-blob \
     --bundle checksums.txt.sigstore.json \
     --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     checksums.txt
   ```
   Adjust the title/notes in the UI if the one-liner needs polish.
   The job is idempotent on a tag re-dispatch (re-uploads assets with
   `--clobber`). This satisfies OpenSSF Scorecard **Signed-Releases**;
   an rc dry-run produces an equivalent **pre-release** with the same
   signed assets, which is how this path is exercised before the real
   tag (rc releases never move `:latest` and are marked pre-release).
10b. **Refresh the reference digests in
   [Verifying releases](verifying-releases.md).** The "Rebuilding the
   binaries yourself" section ends with the expected `sha256sum` of
   `net-dhcp` and `dhcp-handler`, prefixed by the version they belong
   to. Those can only be known once the tag has built, so this is the
   one documentation change that cannot happen before the tag.
   Since v1.7.0 there are TWO digest blocks, one per architecture —
   refresh both; the failing gate in each of the `release` and
   `release-arm64` jobs prints its arch's corrected block verbatim:
   ```sh
   gh run download <release-run-id> -n <artifact>   # or from the release
   sha256sum rootfs/usr/sbin/net-dhcp rootfs/usr/lib/net-dhcp/dhcp-handler
   ```
   Update the version name and both digests, and land it on `dev` as a
   normal PR — **then bring it to `main` before the next rc.** Since
   #547 the release run itself checks this block, so the fix has to be
   on the **tagged commit**: leaving it on `dev` makes the next rc fail
   in exactly the same place. (Before #547 it could ride along with the
   next release, which is what this step used to say. v1.6.0 is where
   that stopped being true.) Leaving
   the previous version's digests in place is worse than having none:
   a reader who rebuilds the current tag and compares against them sees
   a mismatch and concludes the release does not match its source.

   **The rc dry-run tells you the digests.** `release.yml` compares this
   block against the binaries it just built and fails the run when they
   disagree, printing the corrected block ready to paste (#502). So the
   first rc of a new version is *expected* to fail on this step — that
   is the check doing its job, and it is why the rc exists. Take the
   block from the failed run's log, land it, and re-tag `-rc2`; the real
   tag then passes silently. A pre-release compares against its base
   version, so `v1.6.0-rc2` validates exactly what `v1.6.0` will publish.
11. **Fast-forward `dev` to `main`** so the release commit (version
   pins, RELEASE_NOTES section) lands on `dev` too:
   ```sh
   git checkout dev && git merge --ff-only main && git push origin dev
   ```
   Skipping this leaves the next feature branch starting from the
   previous version's README/docs, and the next release PR has to
   re-bump them. Forgotten once after v0.9.0 — that's why
   `release.yml`'s header comment carries the same checklist.
12. **Prune merged branches.** The repo has *Automatically delete head
   branches* enabled, so merged PR head branches are removed on merge.
   Two things that setting doesn't cover, so clean them now:
   ```sh
   # the release branch is merged but was never a PR head:
   git push origin --delete release/vX.Y.Z
   # sweep for any other branch already merged into dev that lingered:
   git fetch --prune origin
   git branch -r --merged origin/dev | grep -vE 'origin/(dev|main|HEAD)$'
   ```
   Delete what that sweep lists. Leave alone: open-PR branches,
   Dependabot branches (it recreates its own — close via the PR), and
   the `upstream/*` refs (those are the original fork's remote, not
   ours).

## Verifying

After the workflow succeeds:

- `curl -sI https://hub.docker.com/v2/repositories/claymore666/net-dhcp/tags/vX.Y.Z/`
  returns `HTTP/2 200`.
- `curl -sI https://ghcr.io/v2/claymore666/docker-net-dhcp/manifests/vX.Y.Z`
  returns `HTTP/2 401` (auth required) — the manifest IS there,
  GHCR just won't expose it anonymously. To confirm presence
  authenticated: `gh auth token | docker login ghcr.io -u <you>
  --password-stdin && docker plugin install
  ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z`.
- The Docker Hub page (<https://hub.docker.com/r/claymore666/net-dhcp>)
  shows the new tag in the Tags tab and the README content
  matches GitHub.
- The milestone is closed (every issue moved to Done by the
  release PR's `Closes` list). Verify with
  `gh issue list --milestone vX.Y.Z --state open` — should be
  empty.
- **Anything listed in `.github/dispatch-pending.txt` is now
  dispatchable — exercise it once and remove the entry.** A
  `workflow_dispatch` workflow is only exposed from the default branch,
  so one that merged to `dev` during this cycle has never run, and this
  release is the first moment it can. Dispatch it, confirm it does what
  its documentation claims, then drop the entry —
  `scripts/check-dispatch-reachable.sh` fails on a declaration that has
  stopped being true, so a forgotten one surfaces on the next PR rather
  than a year later.

## Troubleshooting

| Symptom | Likely cause | Fix |
| ------- | ------------ | --- |
| Workflow shows zero-job "failed" runs on every push, tag push doesn't trigger anything | release.yml parse error (often `secrets` context in step-level `if`) | Fix the YAML, dispatch via `gh workflow run release.yml -f tag=<existing> --ref main` to verify, then retry |
| `Push to GHCR` step ends `403 Forbidden` | GHCR package not linked to repo with Write | One-time fix in package settings (see prerequisites) |
| `Push to Docker Hub` step ends `unauthorized: incorrect username or password` | Token revoked / expired / wrong scope | Regenerate at hub.docker.com, update `DOCKERHUB_TOKEN` repo secret |
| `Sync Docker Hub description from README` step ends `401` | Token scope is image-push only, not admin | Regenerate token with broader scope (see prerequisites) |
| Hub page README is stale after a release | Description-sync step skipped (no Hub creds) or 401'd | Check the workflow run; either set creds or fix the token |
| Tag push succeeded but no Hub publish | `HAS_HUB_CREDS` evaluated false (secrets blank) | Set the secrets, dispatch the workflow against the existing tag |

## Backports between `dev` and `main`

When a release-blocking hotfix has to land on `main` without
going through dev (e.g. v0.8.0's release.yml parser bug), the
flow is:

1. Branch off `main`, fix, PR to `main`, merge. Don't push to
   `main` directly — branch protection and the audit trail.
2. Cherry-pick the same commit onto a branch off `dev`, PR to
   `dev`. This keeps `dev` from regressing on the next release
   PR.

The v0.8.0 cycle uses #97 (main hotfix) and #98 (dev backport) as
the canonical example.

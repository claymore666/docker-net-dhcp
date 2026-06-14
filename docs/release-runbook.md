# Release runbook

How to publish a `vX.Y.Z` of `docker-net-dhcp`. Written from the
v0.8.0 cycle, where we hit two operator-side gotchas (GHCR
package-link, Docker Hub token scope) that aren't reproducible
from the workflow file alone — capturing them here so the next
release isn't another archaeology session.

The goal: a clean release is one tag push, no manual steps.

## One-time prerequisites

These are per-account / per-Hub-repo setup, **not** per-release.
Done once when the publishing chain is first wired up.

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

Use it before every real release tag (step 8 below):

```sh
git checkout main && git pull --ff-only      # the release commit
git tag -s v1.0.0-rc1 -m "v1.0.0-rc1" && git push origin v1.0.0-rc1
```

Watch the run; every step including **verify-install** must be
green. The rc window doubles as the final **documentation
checkpoint** (procedure step 3): confirm README, `docs/`, and the
RELEASE_NOTES section describe the version about to ship — if
stale text surfaces now, fix it before the real tag. Then tag the
real release. Naming: rc of the *upcoming*
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
2. **Bump install pins** in:
   - `README.md` — every `docker plugin install ghcr.io/...:vPREV`
     and `docker network create -d ghcr.io/...:vPREV` snippet.
     Leave historical references like `As of vPREV every PR...`
     alone — those are facts about when something started, not
     install instructions.
   - `docs/parent-attached-modes.md` — the `STATE_DIR` override
     example.
   - `docs/reference.md` — install/upgrade snippets, the Health
     `curl` example, and the Compose `driver:` example all pin the
     version.
   Verify with
   `grep -n vPREV README.md docs/parent-attached-modes.md docs/reference.md`.
3. **Documentation review** — read everything user-visible
   top-to-bottom against what this release actually contains, not
   just the version pins from step 2: `README.md` (feature list,
   driver-opt table, examples), every file under `docs/`
   (including this runbook — process changes during the cycle land
   here too), and the coverage table if republished. Anything
   describing the previous version's behaviour, options, or
   numbers gets updated on the release branch now. When the
   project gains a GitHub wiki, it joins this review. The rc
   dry-run (step 8) is the *last checkpoint* for catching stale
   docs — by the real tag, text and code must agree.
4. **Add a `## vX.Y.Z` section** to `RELEASE_NOTES.md`, **above
   the previous version's section**. Summarise what's changing in
   user-visible terms; the workflow doesn't auto-build this from
   commit messages. Include any **operator-visible compatibility
   notes** (e.g. v0.8.0 narrowed the `IsDHCPPlugin` regex — that
   needed a callout).
5. **PR `release/vX.Y.Z` → `dev`.** Required checks: `test`,
   `staticcheck`, `integration` (every PR builds and exercises its
   own plugin on the integration runner). Merge when green.
6. **Open the release PR `dev` → `main`** with title
   `Release vX.Y.Z` and a `Closes #N` line for **every issue** in
   the milestone. The list is what auto-closes them when the PR
   merges; without it the milestone stays open after the tag.
   Release PRs additionally run the **Coverage** workflow with the
   coverage ratchet (`scripts/coverage-ratchet.sh` vs
   `.github/coverage-baseline.txt`): no release ships with less
   per-package coverage than the previous one. If a package beat its
   floor during the cycle, raise the baseline as part of the release
   branch.
7. **Merge the release PR.** Squash or merge commit — both fine;
   match what's in `git log`.
8. **Pull main, dry-run, then tag:** first push `vX.Y.Z-rc1` and
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
9. **Confirm the GitHub Release** — the `github-release` job now cuts
   it automatically once `verify-install` is green (so a plugin that
   doesn't install never gets an advertised Releases page). It attaches
   the cosign-signed artifacts and uses the `## vX.Y.Z` section of
   `RELEASE_NOTES.md` as the body, so step 4's notes must already be in
   place at tag time. No manual `gh release create` — instead verify:
   ```sh
   gh release view vX.Y.Z   # body = the RELEASE_NOTES section; assets:
                            #   net-dhcp-plugin-vX.Y.Z-linux-amd64.tar.gz
                            #   checksums.txt{,.sig,.pem}
   # Re-verify the signature the way a downstream consumer would:
   cosign verify-blob \
     --certificate checksums.txt.pem --signature checksums.txt.sig \
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
10. **Fast-forward `dev` to `main`** so the release commit (version
   pins, RELEASE_NOTES section) lands on `dev` too:
   ```sh
   git checkout dev && git merge --ff-only main && git push origin dev
   ```
   Skipping this leaves the next feature branch starting from the
   previous version's README/docs, and the next release PR has to
   re-bump them. Forgotten once after v0.9.0 — that's why
   `release.yml`'s header comment carries the same checklist.
11. **Prune merged branches.** The repo has *Automatically delete head
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

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

Defence: every release.yml change should be tested with
`gh workflow run release.yml -f tag=vEXISTING.TAG --ref main`
**before** tagging a real release. That dispatches the workflow
against an already-published tag and exercises the full path
without needing a new tag — if the dispatch returns
`HTTP 422: failed to parse workflow`, the file's broken.

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
   Verify with `grep -n vPREV README.md docs/parent-attached-modes.md`.
3. **Add a `## vX.Y.Z` section** to `RELEASE_NOTES.md`, **above
   the previous version's section**. Summarise what's changing in
   user-visible terms; the workflow doesn't auto-build this from
   commit messages. Include any **operator-visible compatibility
   notes** (e.g. v0.8.0 narrowed the `IsDHCPPlugin` regex — that
   needed a callout).
4. **PR `release/vX.Y.Z` → `dev`.** Required checks: `test`,
   `staticcheck`. Integration is informational. Merge when green.
5. **Open the release PR `dev` → `main`** with title
   `Release vX.Y.Z` and a `Closes #N` line for **every issue** in
   the milestone. The list is what auto-closes them when the PR
   merges; without it the milestone stays open after the tag.
6. **Merge the release PR.** Squash or merge commit — both fine;
   match what's in `git log`.
7. **Pull main and tag:**
   ```sh
   git checkout main && git pull --ff-only
   git tag -a vX.Y.Z -m "vX.Y.Z — <one-liner>"
   git push origin vX.Y.Z
   ```
   The workflow fires on `tags: v*`. Watch it at
   <https://github.com/claymore666/docker-net-dhcp/actions/workflows/release.yml>.
   Expected steps: Resolve tag → checkout → setup-go →
   GHCR login → Hub login (or skip) → Push to GHCR → Push to
   Hub (or skip) → Sync Hub description → Workflow summary.
8. **Cut the GitHub Release** — points the Releases page at the
   right artefact. Either:
   ```sh
   awk '/^## vX\.Y\.Z$/{flag=1; next} /^## v/{flag=0} flag' \
       RELEASE_NOTES.md > /tmp/notes.md
   gh release create vX.Y.Z \
       --title "vX.Y.Z — <one-liner>" \
       --notes-file /tmp/notes.md \
       --verify-tag
   ```
   …or via the UI at
   <https://github.com/claymore666/docker-net-dhcp/releases/new?tag=vX.Y.Z>.
   Don't skip this — Hub and the GitHub Releases page diverge
   without it, and downstream consumers checking the Releases tag
   for "is this the real release?" will get confused.

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

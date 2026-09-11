# Contributing

Contributions are welcome. Open a pull request against the **`dev`**
branch, never `main`. Questions belong in
[Discussions](https://github.com/claymore666/docker-net-dhcp/discussions/new?category=q-a),
and a vulnerability belongs in the private process described in
[SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md)
instead of a public issue.

This page is what an acceptable pull request looks like. Everything on it
is enforced by a check, so nothing here depends on a reviewer remembering
it. The
[pull request template](https://github.com/claymore666/docker-net-dhcp/blob/main/.github/PULL_REQUEST_TEMPLATE.md)
carries the same list as a checklist you fill in as you open one.

## Before you push

`make check` runs the whole fast CI lane locally: build, vet, format,
the race suite, the short fuzz, and every gate script. It takes about a
minute, with no privileges and no host mutation. It is the same set the
Test workflow's two fast jobs run (`test` for build, vet, format, race
and fuzz; `policy-gates` for the gate scripts and their self-tests),
kept in step by a gate of its own, so it will not tell you a branch is
green when CI would not.

## Coding standard

Go code is formatted with `gofmt` and passes `go vet` and
[`staticcheck`](https://staticcheck.dev/); shell and workflow files pass
`shellcheck` and `actionlint`. All four run in CI.

## Tests

New functionality is expected to ship with tests, and a per-package
coverage ratchet enforces it at release time: a release cannot merge if a
package's statement coverage drops below its recorded floor.

Run `go test ./...` for the fast loop and `sudo make integration-local`
for the live suites. See
[Running the tests](internals.md#running-the-tests). Use that target and
not `make integration-test` directly: the latter does not rebuild, so it
silently tests whatever plugin is already installed.

A test that only passes once it has been weakened, with a sleep, a
retry, a skip, a longer timeout or a removed assertion, is a bug report
and never a fix.

## Authorship

Commits and pull request descriptions must not carry AI-assistant
attribution: no `Co-authored-by:` trailer naming an assistant or an
assistant's no-reply address, no "Generated with …" line, no assistant
session trailer or link. The commit author must be a person.

Using an assistant to help write a change is fine and needs no
disclosure. What the project asks is that you sign the work as its author
and stand behind it.

This is enforced by the `attribution` check. It reads every commit in
the pull request, message *and* author identity, since a rebase
preserves authorship, and it reads the description. In the description,
code blocks and inline code are stripped before scanning, so a trailer
can be quoted to discuss one, as here; commit messages are scanned in
full and have no such escape.

## Green CI

Every pull request must pass the repository's required checks before it
can merge. Branch protection holds the authoritative list and the checks
panel on the pull request shows it applied to the branch. At the time of
writing that list is: unit tests, `staticcheck`, the live integration
suite, `govulncheck`, `actionlint`, CodeQL (`Analyze (go)` and
`Analyze (actions)`), `attribution`, `policy-gates`, and `docs-site`.

`docs-site` builds the documentation site with `mkdocs build --strict`. It
runs on every pull request, including one that touches no documentation,
because a check that is filtered out of a pull request is absent there, and
an absent required check blocks the merge instead of passing it.

Documentation-only pull requests, meaning diffs touching nothing but
`*.md`, satisfy the integration check through a fast in-job skip. Any
code, script or workflow change runs the full suite.

A separate, **non-required** workflow runs the integration suite on a
stock GitHub-hosted runner weekly and on demand, to validate the plugin
against a vanilla distribution's Docker. It is a portability probe and
never a pull-request gate: a red there flags the hosted environment and
not the change.

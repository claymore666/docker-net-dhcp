<!--
PRs target the `dev` branch, never `main`. Releases are cut by the
maintainer via a dev -> main release PR. See the Contributing section
of the README.
-->

## What this changes

<!-- Brief description of the change and why it's needed. -->

## Related issue

<!-- e.g. Closes #123 -->

## Checklist

- [ ] Branched off and targets `dev` (not `main`)
- [ ] Tests added/updated for the change (the coverage ratchet is enforced at release)
- [ ] Unit tests, `staticcheck`, and the integration suite pass
- [ ] Docs (README / `docs/`) updated if behaviour or options changed
- [ ] No secrets, credentials, or internal host details in the diff
- [ ] **No existing test was weakened to make this pass** — no added sleep,
      retry, skip, raised timeout, removed assertion or opt-out helper. If one
      was, the issue it uncovered is linked above. A test that only passes once
      you weaken it is a bug report: that exact move hid #402 and #408, a
      user-facing `docker restart` failure, for months.

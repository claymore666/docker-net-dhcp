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

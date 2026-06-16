# Project governance

This document describes how `docker-net-dhcp` is governed and who is
responsible for what. It is intentionally lightweight: this is a small,
actively maintained fork, not a foundation project.

## Model

`docker-net-dhcp` is currently a **single-maintainer** project. The
maintainer ([@claymore666](https://github.com/claymore666)) holds final
decision-making authority over the codebase, its releases, and its
infrastructure (the GitHub repository and the GHCR / Docker Hub
registries).

This is an honest statement of the current bus factor, not an
aspiration: there is one maintainer today. Growing that number is an
explicit goal — see "Becoming a maintainer" below.

## Roles and responsibilities

- **Maintainer** — triages issues, reviews and merges pull requests,
  cuts releases (per [`docs/release-runbook.md`](docs/release-runbook.md)),
  responds to security reports (per [`SECURITY.md`](SECURITY.md)), and
  owns the CI/CD and supply-chain configuration.
- **Contributors** — anyone who opens an issue or pull request. There
  is no membership barrier; see [the Contributing section of the
  README](README.md#contributing).

## How decisions are made

- All code changes land through a pull request against the `dev`
  branch and must pass the required CI checks (unit tests,
  `staticcheck`, the integration suite, `govulncheck`, `actionlint`)
  before merge.
- The maintainer reviews and merges. Substantial or user-visible
  changes are discussed in the relevant issue first.
- Releases are deliberate, maintainer-initiated steps; merging a PR is
  not a release. The procedure is documented in the release runbook.

## Becoming a maintainer

Sustained, high-quality contribution is the path to commit/maintainer
access. If you have been contributing regularly and would like to help
maintain the project — including sharing release and infrastructure
responsibility so the project is resilient to any one person becoming
unavailable — open an issue or reach out to the maintainer. Broadening
the maintainer base is welcome.

## Code of conduct

Participation in this project is governed by the
[Code of Conduct](CODE_OF_CONDUCT.md).

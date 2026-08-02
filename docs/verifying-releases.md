# Verifying releases

Every release from **v1.1.0** onward is signed and attested with
[Sigstore](https://www.sigstore.dev/). Nothing here is required to *use*
the plugin — it is here so you can establish that what you downloaded is
what this repository's release workflow built, and not something that
acquired the same filename along the way.

This page is the single copy of these instructions. Each GitHub Release
links here rather than repeating them.

## What is published

| artifact | what it is |
| --- | --- |
| `net-dhcp-plugin-vX.Y.Z-linux-amd64.tar.gz` | the plugin rootfs + `config.json`, `linux/amd64` |
| `checksums.txt` | SHA-256 of every attached artifact |
| `checksums.txt.sigstore.json` | the cosign bundle signing `checksums.txt` |
| `sbom.spdx.json` | SBOM, SPDX format |
| `sbom.cdx.json` | SBOM, CycloneDX format |

The plugin image itself lives at
`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z` (mirrored to Docker Hub as
`claymore666/net-dhcp`), is cosign-signed, and carries SLSA build
provenance.

One signature covers every attached file: `checksums.txt` is signed, and
the checksums inside it cover the artifacts. So verifying the manifest
and then checking the files against it is enough — there is no per-file
signature to chase.

## Verifying the release artifacts

Replace `VERSION` with the release tag (for example `v1.4.0`). You need
[`cosign`](https://docs.sigstore.dev/cosign/installation/) and, for the
provenance step, the [GitHub CLI](https://cli.github.com/).

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt
```

The identity regexp is the point of the exercise: it pins the signature
to this repository's `release.yml`, so a valid Sigstore signature made by
anything else fails.

## Verifying the image

```sh
cosign verify ghcr.io/claymore666/docker-net-dhcp:VERSION \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Verifying build provenance

SLSA provenance covers both the image and the release artifacts:

```sh
gh attestation verify oci://ghcr.io/claymore666/docker-net-dhcp:VERSION \
  --repo claymore666/docker-net-dhcp
gh attestation verify net-dhcp-plugin-VERSION-linux-amd64.tar.gz \
  --repo claymore666/docker-net-dhcp
```

## A note on `:latest`

`:latest` exists and tracks the newest release — it is a retag, sharing
one digest and therefore one signature with the `vX.Y.Z` tag it points
at. It is still worth pinning a version in anything you keep: a Docker
network remembers the exact driver string it was created with, so a
network created against `:latest` breaks the moment `latest` moves.

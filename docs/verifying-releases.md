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

Replace `VERSION` with the release tag (for example `v1.5.0`). You need
[`cosign`](https://docs.sigstore.dev/cosign/installation/) and, for the
provenance step, the [GitHub CLI](https://cli.github.com/).

> **You need cosign v3 or newer.** Check with `cosign version`, and
> install with
> `go install github.com/sigstore/cosign/v3/cmd/cosign@latest`.
>
> `checksums.txt.sigstore.json` is a **Sigstore bundle**, which is the v3
> format. Run the command below with cosign v2 and it fails like this:
>
> ```
> Error: bundle does not contain cert for verification, please provide public key
> ```
>
> That message names the bundle, so it reads as though the release were
> signed wrong. It isn't — v2 simply cannot read the format. If you see
> it, upgrade cosign and run the command again.

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

Also **cosign v3 or newer**.

```sh
cosign verify ghcr.io/claymore666/docker-net-dhcp:VERSION \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

**Verify the tag you actually installed.** From v1.7.0 each architecture
is published as its own tag, and cosign verifies by digest, so the
command above tells you nothing about a tag you did not name. On arm64:

```sh
cosign verify ghcr.io/claymore666/docker-net-dhcp:VERSION-linux-arm64-v8 \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

`VERSION`, `VERSION-linux-amd64` and `latest` are three names for one
digest and one signature; `VERSION-linux-arm64-v8` is a separate build
with a signature of its own. Both are made by the same workflow, so the
identity regexp above is unchanged.

## Verifying build provenance

SLSA provenance covers both the image and the release artifacts:

```sh
gh attestation verify oci://ghcr.io/claymore666/docker-net-dhcp:VERSION \
  --repo claymore666/docker-net-dhcp
gh attestation verify net-dhcp-plugin-VERSION-linux-amd64.tar.gz \
  --repo claymore666/docker-net-dhcp
```

## Rebuilding the binaries yourself

A signature tells you *who* built an artifact. Rebuilding tells you
*what* they built. The build here is reproducible — the same commit
produces byte-identical binaries on any machine — so you can check that
a published release contains the code in this repository, without
trusting the release workflow at all.

You need Docker with `buildx`, and the release tarball you already
downloaded above.

```sh
# 1. The source, exactly as tagged.
git clone https://github.com/claymore666/docker-net-dhcp
cd docker-net-dhcp
git checkout VERSION

# 2. Build on a builder with no caches. --no-cache alone is not enough:
#    the Dockerfile mounts BuildKit caches for Go's module and build
#    caches, and those survive it. A fresh docker-container builder has
#    neither.
docker buildx create --name repro --driver docker-container
docker buildx build --builder repro --no-cache --target builder \
  --output type=local,dest=out .
docker buildx rm repro

# 3. Your binaries.
sha256sum out/usr/local/src/docker-net-dhcp/bin/net-dhcp \
          out/usr/local/src/docker-net-dhcp/bin/dhcp-handler

# 4. The published ones, from the release tarball.
tar -xzf net-dhcp-plugin-VERSION-linux-amd64.tar.gz
sha256sum rootfs/usr/sbin/net-dhcp \
          rootfs/usr/lib/net-dhcp/dhcp-handler
```

The two pairs of digests must match. For **v1.6.0** they are:

```
eade3423468fa55c5f2c82bd16c28f4f6e8e447096c291fe37459b36e22ac78e  net-dhcp
140f4f14dc9a48d7991e33186c824da790f30d184fc6e014ebbeb447f22132ca  dhcp-handler
```

Note that step 4 needs no separate digest list from us: the binaries you
are comparing against are the ones inside the signed tarball, and
`checksums.txt` already covers that tarball. The chain closes on
artifacts you have in hand.

### What the determinism rests on

None of this is accidental, and it is worth knowing what to suspect if a
rebuild ever fails to match:

- base images pinned by digest, not tag — both the Go builder and the
  Alpine runtime;
- Alpine packages pinned to exact versions;
- a fixed build path inside the container, so no host path is embedded;
- no timestamp, VCS revision or build host stamped into the binaries;
- Go's own compiler output, which is deterministic given identical
  inputs.

A run of the `Reproducible build` workflow builds the same commit twice
on two cold builders and fails if the binaries differ, so this is
checked rather than asserted. It runs weekly, and on any pull request
touching the Dockerfile or the Go module files.

## A note on `:latest`

`:latest` exists and tracks the newest release — it is a retag, sharing
one digest and therefore one signature with the `vX.Y.Z` tag it points
at. It is still worth pinning a version in anything you keep: a Docker
network remembers the exact driver string it was created with, so a
network created against `:latest` breaks the moment `latest` moves.

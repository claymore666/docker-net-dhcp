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
| `checksums.txt` | SHA-256 of every attached amd64 artifact, and of the plugin binary inside the tarball |
| `checksums.txt.sigstore.json` | the cosign bundle signing `checksums.txt` |
| `sbom.spdx.json` | SBOM, SPDX format |
| `sbom.cdx.json` | SBOM, CycloneDX format |
| `net-dhcp-plugin-vX.Y.Z-linux-arm64.tar.gz` | the same for `linux/arm64` (v1.7.0 onward) |
| `checksums-arm64.txt`, `checksums-arm64.txt.sigstore.json` | the arm64 checksum manifest, in the same shape, and its cosign bundle |
| `sbom-arm64.spdx.json`, `sbom-arm64.cdx.json` | the arm64 SBOMs |

The plugin image itself lives at
`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z` (mirrored to Docker Hub as
`claymore666/net-dhcp`), is cosign-signed on both registries, and
carries SLSA build provenance **on GHCR only** — the Docker Hub mirror
is signed but not provenance-attested, and the two registries carry
different digests. Verify provenance against the `ghcr.io` reference.
The arm64 image is the same at `:vX.Y.Z-arm64`; every
verification step below applies to it with the `-arm64` tag and the
`-arm64` artifact names substituted, in a directory of its own — see
[One architecture per directory](#one-architecture-per-directory).

One signature covers every attached file: `checksums.txt` is signed, and
the checksums inside it cover the artifacts — and the plugin binary
packed inside the tarball, under the path it has once extracted. So
verifying the manifest and then checking the files against it is enough
— there is no per-file signature to chase.

## Verifying the release artifacts

Replace `VERSION` with the release tag (for example `v1.9.0`). You need
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
# In a directory holding ONE architecture's assets: the tarballs both
# unpack a binary to the same path. See "One architecture per directory".
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
tar -xzf net-dhcp-plugin-VERSION-linux-amd64.tar.gz
sha256sum --ignore-missing -c checksums.txt
```

`checksums.txt` covers **four** files — the plugin tarball, both SBOMs,
and the plugin binary from inside the tarball, listed as
`rootfs/usr/sbin/net-dhcp` — because one signature over one manifest is
what attests all of them at once. That fourth entry is why the `tar
-xzf` is in the block above and not an afterthought: it is a path, not
an attached asset, and it exists only once you have unpacked the
tarball into the directory you run the check from.

`--ignore-missing` is not a weakening: without it, a reader who
downloaded only the tarball and did not unpack it gets

```
net-dhcp-plugin-vX.Y.Z-linux-amd64.tar.gz: OK
sha256sum: sbom.spdx.json: No such file or directory
sbom.spdx.json: FAILED open or read
sha256sum: sbom.cdx.json: No such file or directory
sbom.cdx.json: FAILED open or read
sha256sum: rootfs/usr/sbin/net-dhcp: No such file or directory
rootfs/usr/sbin/net-dhcp: FAILED open or read
sha256sum: WARNING: 3 listed files could not be read
```

and has every reason to conclude the release was tampered with. With
it, every file you actually have is verified and the rest are skipped.
It does not soften the check that matters: a file that IS present and
does not match still fails — `FAILED`, exit status 1.

**What the flag costs you is the binary, and only the binary.** The
three attached assets are either downloaded or obviously absent; the
binary is the one entry a reader can be missing without noticing,
because skipping it looks exactly like not having downloaded an SBOM.
Unpack the tarball first, as the block above does, and
`rootfs/usr/sbin/net-dhcp: OK` appears in the output — that line is the
signed statement about the binary you are going to run. For an
exhaustive check, download the tarball and both SBOMs, unpack the
tarball, and drop the flag.

The identity regexp is the point of the exercise: it pins the signature
to this repository's `release.yml`, so a valid Sigstore signature made by
anything else fails.

### One architecture per directory

The arm64 release ships its own `checksums-arm64.txt` and bundle.
`checksums-arm64.txt` covers **four** files in the same shape: the
arm64 tarball, its two SBOMs, and `rootfs/usr/sbin/net-dhcp` from
inside that tarball. Verify it exactly the same way, unpacking
included — **in a directory that holds the arm64 assets and nothing
else.**

The attached files are named apart; the packed binary is not. A plugin
tarball has the same layout whichever architecture it was built for, so
`checksums.txt` and `checksums-arm64.txt` both record `rootfs/usr/sbin/net-dhcp` — the same name, for two different files.
Unpack both tarballs in one directory and the second extraction
overwrites the first architecture's binary. Checking the first manifest
there then prints

```
rootfs/usr/sbin/net-dhcp: FAILED
sha256sum: WARNING: 1 computed checksum did NOT match
```

on a release that is perfectly good, and `--ignore-missing` does not
soften that one: the file is present, it is simply the other
architecture's. `FAILED` is the line the worked example above tells you
to read as tampering, so this is worth avoiding rather than explaining
away. Two directories, one architecture each, and both manifests verify
four files.

## Verifying the image

Also **cosign v3 or newer**.

```sh
cosign verify ghcr.io/claymore666/docker-net-dhcp:VERSION \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Verifying build provenance

SLSA provenance covers both the image and the release artifacts. The
image half is **GHCR only**: no provenance attestation exists for the
Docker Hub bytes, in any store, so `gh attestation verify` against a
`docker.io` reference finds nothing. Use the `ghcr.io` reference below
even if you installed from Docker Hub.

```sh
gh attestation verify oci://ghcr.io/claymore666/docker-net-dhcp:VERSION \
  --repo claymore666/docker-net-dhcp
gh attestation verify net-dhcp-plugin-VERSION-linux-amd64.tar.gz \
  --repo claymore666/docker-net-dhcp
```

## Rebuilding the binaries yourself

A signature tells you *who* built an artifact. Rebuilding tells you
*what* they built. The build here is reproducible — the same commit,
built for the same tag, produces byte-identical binaries on any machine
— so you can check that a published release contains the code in this
repository, without trusting the release workflow at all.

Since 2.0 the binary carries its own build identity (`version`, `commit`
and the DHCP library revision, all three readable from `/Plugin.Health`
and from the `net_dhcp_build_info` metric). `version` and `commit` reach
it as build arguments, so the rebuild has to pass the same two values —
step 2 below does. The library revision is read out of the source tree
and needs no argument.

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
#
#    VERSION is the tag you checked out; COMMIT is the FULL revision of
#    it, never the abbreviated one — git shortens to a length that
#    depends on the size of your clone, and a different string is a
#    different binary.
docker buildx create --name repro --driver docker-container
docker buildx build --builder repro --no-cache --target builder \
  --build-arg VERSION=VERSION \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --output type=local,dest=out .
docker buildx rm repro

# 3. Your binary.
sha256sum out/usr/local/src/docker-net-dhcp/bin/net-dhcp

# 4. The published one, from the release tarball.
tar -xzf net-dhcp-plugin-VERSION-linux-amd64.tar.gz
sha256sum rootfs/usr/sbin/net-dhcp
```

**One binary is the 2.0 shape; a 1.x tarball carries two.** The second
was `dhcp-handler`, the helper the external DHCP client execed for each
lease event. 2.0 leases in-process and builds `net-dhcp` alone.
Verifying a **1.x** release means adding
`out/usr/local/src/docker-net-dhcp/bin/dhcp-handler` to step 3 and
`rootfs/usr/lib/net-dhcp/dhcp-handler` to step 4, and comparing both
pairs. Everything else in the recipe is the same for either.

The digests must match — one pair for a 2.0 release, two for a 1.x one.
That is the whole check: you built it, we built it, the bytes are the
same.

### Where the published digest comes from

**This repository does not carry a list of per-release digests, and
cannot.** From 2.0 the release tag and the commit are compiled into the
binary, so a commit that recorded the digest of a binary built from
itself would change that binary by being made. Through 1.x such a list
lived in this section; it was removed with 2.0 rather than left to
describe releases it can no longer describe.

Instead the digest is produced by the build that makes it true and
signed with everything else. `checksums.txt` (`checksums-arm64.txt` for
arm64) covers the tarball, both SBOMs, **and the binary inside the
tarball**, recorded under the path it has once extracted — the fourth
entry described above. So step 4 has a signed reference without needing
one from a commit:

```sh
# In the directory holding the downloaded assets:
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
tar -xzf net-dhcp-plugin-VERSION-linux-amd64.tar.gz
sha256sum --ignore-missing -c checksums.txt
```

`--ignore-missing` is there so the manifest still checks whichever
assets you downloaded; drop it once you have all of them **and have
unpacked the tarball**, and it becomes an exhaustive check. Without the
`tar -xzf` the binary's entry has no file to read, so dropping the flag
turns a good release into an exit 1 and keeping it skips the entry in
silence. `rootfs/usr/sbin/net-dhcp: OK` is the line that says the binary
inside the tarball is the one this release signed, and it is the same
number your rebuild in step 3 produces.

Since v1.7.0 each release also ships arm64 binaries under the `-arm64`
tags; rebuild them the same way on an arm64 host (the build follows the
host architecture), unpack `net-dhcp-plugin-VERSION-linux-arm64.tar.gz`
in step 4, and check against `checksums-arm64.txt`, whose fourth entry
is the arm64 binary at the same path — which is why that unpacking, and
this whole block, belong in a directory holding only the arm64 assets.
See [One architecture per directory](#one-architecture-per-directory).

### What the determinism rests on

None of this is accidental, and it is worth knowing what to suspect if a
rebuild ever fails to match:

- base images pinned by digest, not tag — both the Go builder and the
  Alpine runtime;
- Alpine packages pinned to exact versions;
- a fixed build path inside the container, so no host path is embedded;
- no timestamp and no build host stamped into the binaries. **The
  version and the commit ARE stamped in** (`-ldflags -X`, since 2.0),
  which is why step 2 passes them: identical inputs include those two.
  Pass a different `COMMIT` and you get a different, equally
  deterministic binary — that is a correct result, not a reproducibility
  failure, and it is the reason the published digests live in the signed
  manifest rather than in a commit;
- Go's own compiler output, which is deterministic given identical
  inputs.

A run of the `Reproducible build` workflow builds the same commit twice
on two cold builders and fails if the binaries differ, so this is
checked rather than asserted. It runs weekly, on demand, and on any
pull request touching the Dockerfile, the Go module files, or the
workflow and gate script that implement the check.

## A note on `:latest`

`:latest` exists and tracks the newest release — it is a retag, sharing
one digest and therefore one signature with the `vX.Y.Z` tag it points
at. It is still worth pinning a version in anything you keep: a Docker
network remembers the exact driver string it was created with, so a
network created against `:latest` breaks the moment `latest` moves.

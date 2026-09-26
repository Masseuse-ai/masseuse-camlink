# Verifying a release

Every release on the [releases page](https://github.com/FemLed/masseuse-camlink/releases)
carries:

- `checksums.txt`: SHA-256 of every archive and of the raw gateway binaries;
- `checksums.txt.sigstore.json`: a cosign keyless signature bundle over the
  checksum file, signed by this repository's release workflow;
- `multiple.intoto.jsonl`: SLSA v1 provenance for every artifact, produced by
  the SLSA generic generator in a separate, isolated job;
- container images at `ghcr.io/femled/masseuse-camlink`, signed keyless by
  digest, with SBOMs and their own SLSA container provenance.

`sh scripts/verify-release.sh vX.Y.Z` runs the release checks below (1 to 3);
`sh scripts/verify-enclave.sh` runs the enclave check (4). (Both are plain
POSIX sh; the repository stores them without the executable bit.)

## 1. Signature

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-camlink/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

## 2. Provenance

```sh
slsa-verifier verify-artifact masseuse-camlink_X.Y.Z_linux_amd64.tar.gz \
  --provenance-path multiple.intoto.jsonl \
  --source-uri github.com/FemLed/masseuse-camlink \
  --source-tag vX.Y.Z
```

For the image (the digest is the one `docker pull` prints, or
`docker buildx imagetools inspect ghcr.io/femled/masseuse-camlink:vX.Y.Z`):

```sh
cosign verify ghcr.io/femled/masseuse-camlink@sha256:... \
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-camlink/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
slsa-verifier verify-image ghcr.io/femled/masseuse-camlink@sha256:... \
  --source-uri github.com/FemLed/masseuse-camlink --source-tag vX.Y.Z
```

The image signature needs cosign 3 or later: the release workflow signs
with cosign 3, which stores the signature as a Sigstore bundle attached to
the image (an OCI referrer, the `sha256-<digest>` tag on the registry)
rather than the older `.sig` tag, and cosign 2 answers `no signatures
found` even with `--new-bundle-format`. The checksum file's bundle in
step 1 verifies with either major version.

## 3. Reproduce the binaries

Releases are built from the Go module proxy, never from a checkout, with
`GOTOOLCHAIN=go1.27.1`, `CGO_ENABLED=0`, `-trimpath -buildvcs=false` and
`-ldflags='-s -w -buildid='`. There is no `-X` version flag: the version the
binary prints is the module version Go embeds. Rebuilding needs Go (any
recent version; `GOTOOLCHAIN` makes it fetch and use go1.27.1) and takes one
command per binary. The release workflow's `reproduce` job runs both recipes
below on a fresh runner for every tag and fails if a hash differs.

### The gateway

The raw `masseuse-camlink-gateway_X.Y.Z_linux_{amd64,arm64}` files are
exactly what `go install` yields:

```sh
export GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 GOFLAGS=
GOOS=linux GOARCH=amd64 go install -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
  github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink-gateway@vX.Y.Z
sha256sum "$(go env GOPATH)/bin/linux_amd64/masseuse-camlink-gateway"
grep masseuse-camlink-gateway_X.Y.Z_linux_amd64 checksums.txt
```

The two hashes match. (On a Linux amd64 host the binary lands in
`$(go env GOPATH)/bin/` without the `linux_amd64` subdirectory.)

The video enclave image (`masseuse-video-tee`, a separate repository) installs
the gateway with this same command and refuses to build unless the result
matches the release checksum it pins, so the binary running in the enclave is
the one you can rebuild here.

### The connector

goreleaser builds the connector in its proxy mode: a scratch module named
after the build id (`connector`) that requires this module at the tag. That
build is not byte-identical to `go install` (it embeds a different main
module), so repeat it the same way:

```sh
export GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 GOFLAGS=
mkdir connector && cd connector && printf 'module connector\n' > go.mod
go get github.com/FemLed/masseuse-camlink@vX.Y.Z
GOOS=linux GOARCH=amd64 go build -mod=mod -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
  -o masseuse-camlink github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink
sha256sum masseuse-camlink
tar -xzOf ../masseuse-camlink_X.Y.Z_linux_amd64.tar.gz masseuse-camlink | sha256sum
```

Use `GOOS`/`GOARCH` (and `GOARM=7` for `linux_armv7`) to match the archive;
Windows archives are zips and the binary is `masseuse-camlink.exe`. The
container image holds the very same binaries as the archives.

## 4. The enclave your camera streams to

The connector dials one kind of peer: a masseuse.ai video enclave whose
Confidential Space attestation it has verified against the policy the
service publishes at `https://masseuse.ai/api/tee-policy` (`internal/attest`;
the checks are listed in `masseuse-video-tee/VERIFY.md`). The policy names
the image digests an enclave may attest and, per digest, where it was built:

```json
"imageSources": {
  "sha256:…": {
    "repo": "ghcr.io/femled/masseuse-video-tee",
    "tag": "v0.1.0",
    "sourceUri": "github.com/FemLed/masseuse-video-tee"
  }
}
```

That repository's release workflow builds the image on GitHub Actions from
the tagged commit, pushes it to the public registry with SLSA provenance
and a keyless signature, and copies the same digest into the registry the
enclave boots from. So the digest in the attestation, the digest in the
public registry and the digest in the provenance are one value, and:

```sh
slsa-verifier verify-image ghcr.io/femled/masseuse-video-tee@sha256:<digest> \
  --source-uri github.com/FemLed/masseuse-video-tee --source-tag v0.1.0
cosign verify ghcr.io/femled/masseuse-video-tee@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-video-tee/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

proves that the code at that tag is what ran on the frames and the audio. The connector
prints exactly this `slsa-verifier` line (`enclave source ... verify=`) each
time it dials, with the digest it just verified; `sh scripts/verify-enclave.sh`
runs both checks for every digest the policy currently allows, and says so
when a digest has no published build (one from before the source was
public). A digest that is not in the policy at all is one the connector
would refuse.

What this does not cover is the same as for the connector itself: that the
source does what it says is a matter of reading it (`masseuse-video-tee`,
`workload/`), and the analysis of the keypoints and vocalization labels the
enclave derives runs in a module that repository pins by hash but does not
publish (its `README.md`, "What happens to your video and audio").

## 5. What the version string tells you

`masseuse-camlink --version` prints this module's version and the Go
toolchain from the binary's embedded build information, e.g.
`v0.1.0 go1.27.1`. `go version -m <binary>` prints the whole module list,
including `github.com/FemLed/masseuse-camlink vX.Y.Z h1:...`; that `h1:` hash
is the one `go mod download -json github.com/FemLed/masseuse-camlink@vX.Y.Z`
reports from `sum.golang.org`. A build from a working tree prints `(devel)`.

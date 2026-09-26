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

`scripts/verify-release.sh vX.Y.Z` runs all of the checks below.

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

For the image:

```sh
cosign verify ghcr.io/femled/masseuse-camlink@sha256:... \
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-camlink/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
slsa-verifier verify-image ghcr.io/femled/masseuse-camlink@sha256:... \
  --source-uri github.com/FemLed/masseuse-camlink --source-tag vX.Y.Z
```

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

## 4. What the version string tells you

`masseuse-camlink --version` prints this module's version and the Go
toolchain from the binary's embedded build information, e.g.
`v0.1.0 go1.27.1`. `go version -m <binary>` prints the whole module list,
including `github.com/FemLed/masseuse-camlink vX.Y.Z h1:...`; that `h1:` hash
is the one `go mod download -json github.com/FemLed/masseuse-camlink@vX.Y.Z`
reports from `sum.golang.org`. A build from a working tree prints `(devel)`.

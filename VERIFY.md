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

The connectors in the `darwin_*` archives are in addition signed with an
Apple Developer ID and notarized ("The macOS binaries" below says how to
check the signer and how to compare them with a rebuild regardless).

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

### The macOS binaries

The `darwin_arm64` and `darwin_amd64` connectors are signed with FemLed's
Apple Developer ID and notarized, so macOS runs them without a Gatekeeper
warning. The signature is the one input to a release that nobody else can
reproduce, and it is the only difference from the recipe above: strip it
from the published binary and from your rebuild, and the two are identical.
`cmd/machostrip` does the stripping on any operating system, the way the Go
project checks its own macOS distributions (`golang.org/x/build/cmd/gorebuild`):
it removes the `LC_CODE_SIGNATURE` load command, the signature at the end of
the file and the zero filler a signing tool leaves where the linker's ad-hoc
signature was, and shrinks `__LINKEDIT` to match. Nothing else changes. Your
rebuild is stripped too because the Go linker itself signs every
darwin/arm64 binary ad hoc; an unsigned darwin/amd64 rebuild passes through
untouched.

```sh
export GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 GOFLAGS=
mkdir connector && cd connector && printf 'module connector\n' > go.mod
go get github.com/FemLed/masseuse-camlink@vX.Y.Z
GOOS=darwin GOARCH=arm64 go build -mod=mod -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
  -o rebuilt github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink
tar -xzOf ../masseuse-camlink_X.Y.Z_darwin_arm64.tar.gz masseuse-camlink > published
go run github.com/FemLed/masseuse-camlink/cmd/machostrip@vX.Y.Z -sha256 rebuilt published
```

The two hashes match. The release workflow's `reproduce` job runs this for
both architectures on every tag, and also fails if a published darwin
binary turns out not to carry a signature. On a Mac, the signature itself
is checked with Apple's tools:

```sh
codesign --verify --strict --verbose=2 masseuse-camlink
codesign -dvv masseuse-camlink 2>&1 | grep -E '^(TeamIdentifier|Timestamp|CodeDirectory)'
spctl --assess --type open --context context:primary-signature -vv masseuse-camlink
mkdir certs && (cd certs && codesign -d --extract-certificates ../masseuse-camlink)
openssl x509 -inform DER -in certs/codesign0 -noout -fingerprint -sha256
```

The expected signer is Apple Developer Team ID `B8Z4RP3846`
(`TeamIdentifier=B8Z4RP3846`), signing with the hardened runtime
(`flags=0x10000(runtime)`) and an Apple timestamp, and `spctl` answers
`accepted` with `source=Notarized Developer ID` (a bare executable is
assessed as an "open" with its primary signature; `--type execute` only
evaluates app bundles, and answers "does not seem to be an app" for a
command-line binary). The leaf certificate,
issued by Apple's Developer ID Certification Authority (G2) and valid from
September 2026 to September 2031, has the SHA-256 fingerprint

```
8D:A7:D2:FD:A7:BE:3C:AC:CE:6E:20:19:FE:0C:1A:68:B2:C1:B5:DC:A0:14:55:68:52:C6:8E:62:3A:35:44:80
```

(SHA-1 `F2:86:A0:BD:8A:53:56:28:96:4B:F7:14:75:28:B3:F3:E5:E0:28:79`). A
replacement certificate will be listed here alongside this one, with the
first release it signs, before it is used. The signature says who published
the binary and that Apple's notary service scanned it; what the binary does
is established by the rebuild above, not by the signature.

## 4. The enclave your camera streams to

The connector dials one kind of peer: a masseuse.ai video enclave whose
Confidential Space attestation it has verified against the policy the
service publishes at `https://masseuse.ai/api/tee-policy` (`internal/attest`;
the checks are listed in `masseuse-video-tee/VERIFY.md`). The policy does
not list image digests. It names the rule an image must satisfy and where
its source lives:

```json
"imageSignatures": ["cfb085b950e93abb8332cede62fa50df662ef9aebb1533b2ae0bf1403ea4f811"],
"minRelease": "v0.4.0",
"sourceUri": "github.com/FemLed/masseuse-video-tee",
"imageRepo": "ghcr.io/femled/masseuse-video-tee"
```

`imageSignatures` is the key the Confidential Space launcher must have
verified a signature from before it started the image (the token lists the
key ids it checked). Only that repository's release workflow can sign with
the key, so a listed key id means the image was built by that workflow at a
release tag. Which release: the workflow bakes the tag and the source commit
into the image as `TEE_IMAGE_VERSION` and `TEE_IMAGE_COMMIT`, the launcher
attests the whole container environment, and the connector refuses a release
below `minRelease`. Nothing has to be committed after the fact for this to
hold, which is the point: a list of digests kept in a repository is always
one release behind the image it describes.

The served policy is not the last word, though: a service that could
publish a looser policy could send a connector to an enclave of its own
choosing. So this connector carries floors of its own
(`internal/attest/floors.go`, `Production`), and the served policy can only
tighten them. The floors fix the token issuer and key set (Google's
Confidential Space signer), the software and hardware (`CONFIDENTIAL_SPACE`
on `GCP_INTEL_TDX`), the signing key above, `minRelease` `v0.4.0`, the
source repository and public registry above, the Google Cloud project the
enclave must run in (`prod-masseuse-video-tee`, the token's
`submods.gce.project_id`) and the registry image it must have been pulled
from (`submods.container.image_reference`), the `.tee.masseuse.ai` host
suffix, no debug images, `STABLE` Confidential Space releases and GPU
confidential computing on. A served policy that names another issuer,
another key, another project or hosts elsewhere is refused with `policy is
looser than this build's floors`, and the connector dials nothing until the
service serves one that is not. `docs/PROTOCOL.md` lists the rule for every
field. Changing a floor takes a release of this repository, which is
reproducible and signed as described above: what the connector will talk
to is fixed by code you can read and rebuild, not by a document a server
sends. The `enclave verified` line names the key id that matched (`signer=`).

The release workflow builds the image on GitHub Actions from the tagged
commit, pushes it to the public registry with SLSA provenance and a keyless
signature, and copies the same digest into the registry the enclave boots
from. So the digest in the attestation, the digest in the public registry and
the digest in the provenance are one value, and with the digest and release
the token names:

```sh
slsa-verifier verify-image ghcr.io/femled/masseuse-video-tee@sha256:<digest> \
  --source-uri github.com/FemLed/masseuse-video-tee --source-tag <TEE_IMAGE_VERSION>
cosign verify ghcr.io/femled/masseuse-video-tee@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-video-tee/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

proves that the code at that tag is what ran on the frames and the audio.
The connector prints exactly this `slsa-verifier` line (`enclave source ...
verify=`) each time it dials, with the digest and release it just verified
(`enclave verified ... release= commit=`); `sh scripts/verify-enclave.sh
--origin https://slot-N.tee.masseuse.ai` reads the digest and release off a
live enclave's token and runs both checks, and `sh scripts/verify-enclave.sh
sha256:<digest>@<tag>` does the same for a line from the connector's log.

The connector also runs both checks itself, before the first frame leaves
the machine (`internal/provenance`, on
[sigstore-go](https://github.com/sigstore/sigstore-go)). From the public
registry it reads what the release attached to the attested digest: the
Sigstore bundle cosign wrote, and the SLSA provenance the
`slsa-github-generator` builder signed. It verifies the bundle the way
`cosign verify` does (the certificate chains to Fulcio and was logged in a
certificate transparency log; the signature covers a statement about this
digest; the Rekor entry and the timestamp check against the Sigstore trust
root) and requires the certificate to name this repository's release
workflow at the very tag the token names, issued by GitHub Actions for a
run on that repository at that tag and, when the token names one, that
commit. It verifies the provenance the way `slsa-verifier` does (the same
checks, with the builder's identity in the certificate) and then reads the
statement: built by the SLSA generator, from
`git+https://github.com/FemLed/masseuse-video-tee@refs/tags/<TEE_IMAGE_VERSION>`
at `TEE_IMAGE_COMMIT`, through `.github/workflows/release.yml`. The trust
root is the public Sigstore one, embedded at build time
(`internal/provenance/trusted_root.json`) and refreshed through TUF into the
state directory when the network allows. Anything missing, unreadable or
failing refuses the enclave: `enclave provenance: signature over
sha256:...: none of N records is a signature by ...`. What passes is logged
as `enclave provenance ... signed_by= signature_log_index= builder=
provenance_log_index=`, with the Rekor indexes so the entries can be looked
up (`rekor-cli get --log-index N`), and printed once per image. Results are
kept for a week per digest, release and commit (`<state-dir>/provenance/`),
so a reconnect does not ask the registry again; the attestation itself is
verified on every dial regardless.

This closes the gap the attestation alone leaves. The token proves the
launcher checked a signature from the release key over the digest and that
the image says it is release X at commit Y; the provenance proves, on
records no one at the service can alter after the fact, that release X at
commit Y is what the public build produced under that digest. Both come from
sources other than the service the connector is talking to. An image built
before the stamp existed carries no release to check against, and the
connector refuses it.

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

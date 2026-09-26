# Verifying a release

Every release on the [releases page](https://github.com/FemLed/masseuse-camlink/releases)
carries:

- `checksums.txt`: SHA-256 of every archive and of the raw gateway binaries;
- `checksums.txt.sigstore.json`: a cosign keyless signature bundle over the
  checksum file, signed by this repository's release workflow;
- `multiple.intoto.jsonl`: SLSA v1 provenance for every artifact, produced by
  the SLSA generic generator in a separate, isolated job;
- `Masseuse.ai-X.Y.Z.dmg`: the Mac download, the application bundle
  `Masseuse.app` in a disk image, built by a second job of the same
  workflow after the archives are published; listed with the ffmpeg source
  tarballs in `checksums-darwin.txt`, signed the same way
  (`checksums-darwin.txt.sigstore.json`), with its own provenance
  `darwin.intoto.jsonl`. (Releases before v0.8.0 named the image
  `masseuse-camlink_X.Y.Z_darwin_all.dmg` and the app `masseuse-camlink.app`.)
- `Masseuse.exe`: the Windows download, one file (named like the Mac's
  `Masseuse.app`; the program is Masseuse.ai): the published
  `windows_amd64` connector byte for byte, followed by a payload with an
  `ffmpeg.exe` built from the same pinned sources and the unit driver
  helpers, assembled by a third job on a Windows runner and signed with
  Azure Artifact Signing (Principled Labs, Inc.) when the release has the
  credentials; listed in `checksums-windows.txt`, signed the same way
  (`checksums-windows.txt.sigstore.json`), with provenance
  `windows.intoto.jsonl`. `Masseuse.ai-X.Y.Z-windows.zip`, listed and
  attested beside it, holds the same file under the name those releases
  used, `Masseuse.ai.exe`, for the installs of releases before 0.13, which
  update themselves from a zip.

The connectors in the `darwin_*` archives, the application bundle and the
disk image are in addition signed with an Apple Developer ID and notarized
("The macOS binaries" and "The macOS app" below say how to check the signer
and how to compare them with a rebuild regardless); the Windows package
and the programs inside it carry Authenticode signatures ("The Windows
package").

`sh scripts/verify-release.sh vX.Y.Z` runs the release checks below (1 to 3,
the macOS app, the Windows package and the unit driver helpers included); `sh scripts/verify-enclave.sh`
runs the enclave check (4). (Both are plain POSIX sh; the repository stores
them without the executable bit.)

## 1. Signature

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-camlink/[.]github/workflows/release[.]yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

## 2. Provenance

```sh
slsa-verifier verify-artifact masseuse-camlink_X.Y.Z_windows_amd64.zip \
  --provenance-path multiple.intoto.jsonl \
  --source-uri github.com/FemLed/masseuse-camlink \
  --source-tag vX.Y.Z
```

(Releases up to v0.17.0 also published a container image,
`ghcr.io/femled/masseuse-camlink`, signed by digest with its own provenance;
none is published since.)

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
GOOS=windows GOARCH=amd64 go build -mod=mod -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
  -o masseuse-camlink.exe github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink
sha256sum masseuse-camlink.exe
unzip -p ../masseuse-camlink_X.Y.Z_windows_amd64.zip masseuse-camlink.exe | sha256sum
```

Use `GOOS`/`GOARCH` to match the archive (`darwin_arm64`, `darwin_amd64`,
`windows_amd64`, `windows_arm64`); the darwin binaries carry a signature on
top, which the next section strips before comparing.

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

### The desktop window

From the release that brought it, every download opens a window: the
desktop application in `desktop/` (docs/DESKTOP.md), a Go program built
with the platform's own window toolkit (Wails; WebKit on a Mac, WebView2
on Windows) that runs the connector as a child and shows what it says. The
window is built by the release workflow on each platform's runner
(`macos-app`, `windows-app` in `.github/workflows/release.yml`), from the
same commit as everything else, and it is **not** rebuilt byte for byte: it
links against the toolkit and the system's libraries with cgo, which no
proxy rebuild reproduces. What covers it is the same as what covers ffmpeg:
the checksum file for its platform (`checksums-darwin.txt`,
`checksums-windows.txt`), signed keyless by the release workflow at the
tag, the SLSA provenance for that file's subjects (`darwin.intoto.jsonl`,
`windows.intoto.jsonl`), which names the workflow run and the commit, and
the platform's signature (the Developer ID and the notarization;
Authenticode). The window has no
credentials and no network code of its own: it starts the connector,
relays its lines to the page and its requests back, opens the Help pages
in the browser, and starts the program again after an update
(`desktop/connector.go`, `desktop/relaunch.go`).

The connector inside each download is the published binary, byte for
byte, and so the rebuild of section 3; the sections below say where it
sits and how to compare it. Run from a terminal it is the same program as
the archives' (`-console` on a Mac), and `--version` on the window and on
the connector both answer the tag.

### The macOS app

The Mac download, `Masseuse.ai-X.Y.Z.dmg`, holds `Masseuse.app`: the
window under the name people see (`CFBundleIdentifier` stays
`ai.masseuse.camlink`; v0.8.0 and v0.8.1 shipped it as `Masseuse.ai.app`
with `Contents/MacOS/Masseuse.ai`, which the Finder showed extension and
all; `packaging/macos/README.md` says why). Its executable,
`Contents/MacOS/Masseuse`, is the desktop window (above), universal;
beside it, `Contents/MacOS/masseuse-camlink` is the two darwin binaries
above joined into one universal binary with `lipo`, the connector the
window runs; and in `Contents/Helpers/ffmpeg` an ffmpeg built from pinned
upstream sources so that the app needs no install step
(`packaging/ffmpeg/THIRD_PARTY.md`). Until the window, the connector was
the bundle's executable itself, and the comparison below applied to
`Contents/MacOS/Masseuse`. The release workflow's `macos-app` job
(`.github/workflows/release.yml`) builds the bundle on a macOS runner from
the archives it has just published, after verifying them against the signed
`checksums.txt`, and signs, notarizes and staples the app and then the image
(`packaging/macos/`).

The image and the ffmpeg source tarballs have their own checksum file,
signed and attested like the first:

```sh
cosign verify-blob \
  --bundle checksums-darwin.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-camlink/[.]github/workflows/release[.]yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums-darwin.txt
shasum -a 256 -c checksums-darwin.txt
slsa-verifier verify-artifact Masseuse.ai-X.Y.Z.dmg \
  --provenance-path darwin.intoto.jsonl \
  --source-uri github.com/FemLed/masseuse-camlink --source-tag vX.Y.Z
```

The app's connector is the published binaries, and so the rebuild: strip
the universal binary and each architecture's hash is the hash of the
stripped archive binary for that architecture (and of the rebuild).
`machostrip` takes a universal binary apart by itself and prints one line
per architecture, named as the archives are:

```sh
hdiutil attach -readonly -nobrowse Masseuse.ai-X.Y.Z.dmg
go run github.com/FemLed/masseuse-camlink/cmd/machostrip@vX.Y.Z -sha256 \
  /Volumes/Masseuse.ai/Masseuse.app/Contents/MacOS/masseuse-camlink
# two lines, "(arm64)" and "(amd64)": compare each with the stripped
# archive binary or the rebuild of the previous section
hdiutil detach /Volumes/Masseuse.ai
```

The `macos-app` job makes this comparison itself before it uploads
anything, so a release whose bundle did not carry the published binaries
would not have a bundle. The window and ffmpeg are the two components of
the bundle that are not rebuilt byte for byte: the window as "The desktop
window" says; for ffmpeg, `packaging/ffmpeg/build.sh` at the tag is its
recipe (pinned tarballs, hashes checked, LGPL configuration, the components
listed in the script), the tarballs are attached to the release, and the
provenance `darwin.intoto.jsonl` says which workflow run built the image
they sit in. Anyone can rebuild ffmpeg with the script and put their own in
the bundle's `Contents/Helpers/`; the connector runs the one it finds there.

On a Mac, the signatures and the notarization tickets are checked the way
the system checks them at a double-click; the release fails if any of these
does not hold (`packaging/macos/assess.sh`):

```sh
codesign --verify --deep --strict --verbose=2 /Volumes/Masseuse.ai/Masseuse.app
spctl --assess --type execute -vv /Volumes/Masseuse.ai/Masseuse.app
xcrun stapler validate /Volumes/Masseuse.ai/Masseuse.app
codesign --verify --strict --verbose=2 Masseuse.ai-X.Y.Z.dmg
spctl --assess --type open --context context:primary-signature -vv Masseuse.ai-X.Y.Z.dmg
xcrun stapler validate Masseuse.ai-X.Y.Z.dmg
```

Both `spctl` verdicts are `accepted` with `source=Notarized Developer ID`;
`codesign -dvv` on the app, on `Contents/MacOS/masseuse-camlink`
(identifier `ai.masseuse.camlink.connector`) and on
`Contents/Helpers/ffmpeg` shows the same `TeamIdentifier=B8Z4RP3846` and
`flags=0x10000(runtime)` as the bare binaries, signed by the certificate
listed above.

The signatures also say who may open the camera and the microphone. Under
the hardened runtime a process opens either only with the entitlements
`com.apple.security.device.camera` and
`com.apple.security.device.audio-input`, and macOS checks them on the
process that opens the device (ffmpeg) and on the application it runs
under, the bundle's executable, which the system holds responsible; a
responsible process without them is refused without a prompt, and the
application never appears under Privacy & Security. From v0.18.0 the
release signs both with `packaging/macos/device.entitlements` (v0.16.0 and
v0.17.0 signed the executable without them, and the camera stayed off); the
connector and the unit driver helpers carry neither. Read them back (the
dots escaped, or `plutil` takes each as a step down a key path):

```sh
for f in /Volumes/Masseuse.ai/Masseuse.app/Contents/MacOS/Masseuse \
         /Volumes/Masseuse.ai/Masseuse.app/Contents/Helpers/ffmpeg; do
  codesign -d --entitlements - --xml "$f" | plutil -extract 'com\.apple\.security\.device\.camera' raw -o - -
  codesign -d --entitlements - --xml "$f" | plutil -extract 'com\.apple\.security\.device\.audio-input' raw -o - -
done   # four lines of "true"
codesign -d --entitlements - --xml /Volumes/Masseuse.ai/Masseuse.app/Contents/MacOS/masseuse-camlink   # no entitlements
```

`packaging/macos/assess.sh entitlements` makes the same reading, and the
release fails without it, both on the bundle it signs and on the bundle
after it has updated itself. `scripts/verify-release.sh` runs all of this
as step 8 when the release carries `checksums-darwin.txt`.

### The Windows package

The Windows download, `Masseuse.exe`, is one file: the desktop window
(above), then a *payload* appended after the executable's image
(`internal/payload`), then the Authenticode signature. The payload holds
the connector from the `windows_amd64` archive as `masseuse-camlink.exe`,
`ffmpeg.exe`, the unit driver helpers (`units/camlink-unit-*.exe`), the
licence texts and a `README.txt`, with a manifest naming each file's
SHA-256; the window unpacks it under the state directory when it runs and
starts the connector from there, which finds `ffmpeg.exe` and the helpers
beside itself. Until the window, the file itself was the connector and the
payload had no connector in it. `ffmpeg.exe` is built from the same pinned
tarballs as the Mac's ffmpeg by `packaging/ffmpeg/build.sh -t windows`
(cross-compiled with mingw-w64 on a Linux runner, LGPL configuration, the
DirectShow input and the Media Foundation H.264 encoder in place of
AVFoundation and VideoToolbox; `packaging/ffmpeg/THIRD_PARTY.md`). The
release workflow's `windows-app` job builds the window and assembles the
package on a Windows runner after verifying the archive against the signed
`checksums.txt`, verifying the helpers against their signed manifest and
running the connector's own encoding chain through the `ffmpeg.exe` it
packs (`packaging/windows/`). The zip beside it on the release,
`Masseuse.ai-X.Y.Z-windows.zip`, holds the same file as `Masseuse.ai.exe`,
the name releases before 0.13 gave it, for the installs of those releases,
whose updater looks for that name inside the zip.

The package has its own checksum file, signed and attested like the others.
The dots in the identity are written `[.]` rather than `\.` throughout this
document and in `scripts/verify-release.sh` for the sake of Git Bash on
Windows, which rewrites a backslash in an argument to a native program
(cosign) as a path separator and would turn `\.` into `/.`:

```sh
cosign verify-blob \
  --bundle checksums-windows.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-camlink/[.]github/workflows/release[.]yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums-windows.txt
sha256sum -c checksums-windows.txt
slsa-verifier verify-artifact Masseuse.exe \
  --provenance-path windows.intoto.jsonl \
  --source-uri github.com/FemLed/masseuse-camlink --source-tag vX.Y.Z
```

The signature and the payload are the two things the package carries
beyond the window: `cmd/pestrip` (`internal/pesig`) removes both, the way
`machostrip` removes a Mac signature, on any operating system. It cuts the
attribute certificate table off the end of the file with the zero bytes
that padded the file to its alignment, zeroes the security entry of the
data directory and the header's checksum, then cuts the payload off where
its footer says it begins, and writes the payload's files out with their
hashes listed. The connector among them, its own signature stripped the
same way, is the archive's executable, and so the rebuild of "The
connector" above with `GOOS=windows GOARCH=amd64`:

```sh
go run github.com/FemLed/masseuse-camlink/cmd/pestrip@vX.Y.Z -payload payload Masseuse.exe
go run github.com/FemLed/masseuse-camlink/cmd/pestrip@vX.Y.Z -sha256 payload/masseuse-camlink.exe
unzip -p masseuse-camlink_X.Y.Z_windows_amd64.zip masseuse-camlink.exe | sha256sum
```

The two hashes match; `scripts/verify-release.sh` checks this as step 9
when the release carries `checksums-windows.txt` (for releases before the
window it compares `pestrip -sha256 Masseuse.exe` itself with the archive's
executable). A Go-built executable has the security entry and the checksum
at zero to begin with, so `pestrip -sha256` of an unsigned one is its plain
hash; ffmpeg's linker writes a checksum, so ffmpeg is compared `pestrip
-sha256` against `pestrip -sha256`, never against a plain hash:

```sh
go run github.com/FemLed/masseuse-camlink/cmd/pestrip@vX.Y.Z -sha256 payload/ffmpeg.exe   # against your own build of ffmpeg, stripped the same way
go run github.com/FemLed/masseuse-camlink/cmd/pestrip@vX.Y.Z -sha256 Masseuse.exe         # the window, signature and payload off: what the windows-app job built
```

On Windows, the signatures themselves are checked with Windows' own tools:

```powershell
Get-AuthenticodeSignature Masseuse.exe, payload\masseuse-camlink.exe, payload\ffmpeg.exe, payload\units\*.exe | Format-List Status, SignerCertificate, TimeStamperCertificate
```

Every `Status` is `Valid`; the signer's subject names Principled Labs,
Inc., under a certificate issued by Microsoft's Artifact Signing CA
(Microsoft Identity Verification Root Certificate Authority 2020) and
timestamped by `timestamp.acs.microsoft.com`; the certificates are
short-lived by design and the timestamp is what keeps the signature valid.
A release published without the signing credentials carries no signature,
and the checks above are what stand in for it. The signature says who
published the file; what the connector does is established by the rebuild,
not by the signature.

The icon and the file description Explorer shows come from resource
objects committed in the source (`cmd/masseuse-camlink/rsrc_windows_amd64.syso`
for the connector, `desktop/rsrc_windows_amd64.syso` for the window,
generated by `packaging/windows/make-syso.sh`), so the connector's are part
of the rebuild, not added afterwards. On Windows, `go run
./packaging/windows/verinfo Masseuse.exe` prints those strings as Explorer
reads them (ProductName `Masseuse.ai`, CompanyName `FemLed, Inc.`,
FileDescription `Masseuse.ai for your computer`); the resources carry no
version number, the version being what `--version` and the build
information say.

### Unit driver helpers

From v0.10.0 the downloads carry unit driver helpers ([docs/UNITS.md](docs/UNITS.md)):
programs named `camlink-unit-<name>` in `Masseuse.app/Contents/Helpers/units/`,
`units/` in the Windows package's payload and `units/` in the archives, which the
connector runs as child processes to serve stimulation units whose drivers
are not in this repository. They are the one part of a download that is
neither built from this repository nor rebuilt byte for byte here; what
can be checked is that every helper in a release is exactly one named in
a manifest signed by masseuse.ai's helpers key, for the helpers version
the tag pins.

The key's public half is `packaging/units/cosign.pub` at the tag, and
`packaging/units/VERSION` is the helpers version. The manifest and the
helpers themselves are at `https://masseuse.ai/app/units/<version>/`:

```sh
V=$(curl -fsSL https://raw.githubusercontent.com/FemLed/masseuse-camlink/vX.Y.Z/packaging/units/VERSION)
curl -fsSLO https://raw.githubusercontent.com/FemLed/masseuse-camlink/vX.Y.Z/packaging/units/cosign.pub
curl -fsSLO "https://masseuse.ai/app/units/$V/manifest.json"
curl -fsSLO "https://masseuse.ai/app/units/$V/manifest.json.sigstore.json"
cosign verify-blob --key cosign.pub --bundle manifest.json.sigstore.json manifest.json
jq -r '.files[] | "\(.os)/\(.arch)  \(.sha256)  \(.name)"' manifest.json
```

The manifest names each helper once per operating system and
architecture, with the hash of the file as published. In the Mac bundle
the helpers are universal binaries signed with the same Developer ID as
the app, so they are compared the way the app's executable is, with
`machostrip`, which removes the signature and hashes each architecture on
its own; the other side is the thin `darwin/arm64` and `darwin/amd64`
files, fetched and checked against the manifest by `packaging/units/fetch.sh`
and stripped the same way (Go's linker gives a darwin/arm64 binary an
ad-hoc signature, so the thin arm64 file's raw hash is not its stripped
one):

```sh
for arch in arm64 amd64; do
  sh packaging/units/fetch.sh "$V" darwin "$arch" "units-$arch"   # from the tag's checkout
  go run github.com/FemLed/masseuse-camlink/cmd/machostrip@vX.Y.Z -sha256 units-$arch/camlink-unit-*
done
hdiutil attach -readonly -nobrowse Masseuse.ai-X.Y.Z.dmg
for f in /Volumes/Masseuse.ai/Masseuse.app/Contents/Helpers/units/camlink-unit-*; do
  go run github.com/FemLed/masseuse-camlink/cmd/machostrip@vX.Y.Z -sha256 "$f"
done
# each "(arm64)" and "(amd64)" line of a bundled helper is the stripped
# hash of the thin file of the same name for that architecture
hdiutil detach /Volumes/Masseuse.ai
```

The Windows package's helpers are Authenticode-signed and compared
stripped, like the Mac's: `pestrip -payload payload Masseuse.exe`
writes them out, and `pestrip -sha256 payload/units/camlink-unit-<name>.exe`
is the `windows/amd64` entry (a Go binary's stripped hash is its plain
one). The archives' helpers carry no signature and hash as they are:
`unzip -p masseuse-camlink_X.Y.Z_windows_amd64.zip units/camlink-unit-<name>.exe
| sha256sum` against `windows/amd64`. The release workflow runs the manifest
check itself (`packaging/units/fetch.sh`) before it bundles anything, so a
release whose helpers were not the manifest's would not have them, and
`checksums.txt`, `checksums-darwin.txt` and `checksums-windows.txt` cover
the archives and the package the helpers sit in, provenance and all.

`scripts/verify-release.sh` runs the manifest check and the archive and
package comparisons as step 10 when the tag carries `packaging/units/VERSION`.

A connector without helpers is this repository alone: remove
`Contents/Helpers/units` (or run with `-estim-helpers none`) and it serves
the Mastago, and only the Mastago, from the drivers in the tree.

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
  --certificate-identity-regexp '^https://github.com/FemLed/masseuse-video-tee/[.]github/workflows/release[.]yml@refs/tags/v' \
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
reports from `sum.golang.org`. A build from a working tree prints `(devel)`,
or Go's pseudo-version for the commit with `+dirty` when the tree has
changes; neither is a release, and such a build never updates itself.

## 6. What the updater verifies

From v0.11.0 the program replaces itself with a newer release on its own
(README, "Updates"). What it accepts is what this document tells a reader
to accept, checked by the running program (`internal/update`, with
`internal/provenance` doing the Sigstore work it already does for the
enclave image) before any file is moved:

1. `checksums.txt` and `checksums.txt.sigstore.json` from the latest
   release (`releases/latest/download/`). The bundle must verify against
   the Sigstore trust root the program carries (refreshed through TUF into
   its state directory when the network allows, as for the enclave), with a
   certificate issued by GitHub Actions to
   `https://github.com/FemLed/masseuse-camlink/.github/workflows/release.yml@refs/tags/vX.Y.Z`
   for a run on this repository, logged in Rekor: step 1 above. The release
   tag is read from that certificate, never from a file name or a version
   string on the page, and only a tag newer than the running program's own
   goes further; pre-releases never do.
2. The checksum file the artifact is listed in (`checksums-darwin.txt` for
   the disk image, `checksums-windows.txt` for the Windows package,
   `checksums.txt` for a bare archive, each with its own bundle verified
   the same way at that exact tag), and the download's SHA-256 against
   it: step 2.
3. The SLSA provenance for the artifact (`multiple.intoto.jsonl`,
   `darwin.intoto.jsonl` or `windows.intoto.jsonl`), a Sigstore bundle
   signed by the SLSA generic generator for a build configured by this
   repository's release workflow at that tag, whose statement names the
   downloaded file and its digest among its subjects: step 3, as
   `slsa-verifier verify-artifact` does it.
4. On a Mac, the application inside the mounted image: `codesign --verify
   --deep --strict`, `spctl --assess --type execute` answering `accepted`,
   `TeamIdentifier=B8Z4RP3846` and the hardened runtime flag, on the image's
   copy and again on the copy taken with `ditto`: "The macOS app" above.
5. The new program's own `--version`, run from where it was staged, must
   print the tag: the connector's inside the bundle and the desktop
   archive, the window's for the Windows package (it is the file replaced).

Only then is the running install renamed aside (a Mac bundle under the
state directory's `previous/`, the files of the other layouts in
`.previous/`) and the new one renamed in; the new program is started with
the same arguments (by the shell that started the running one when there
is one, the desktop window or the Terminal window's `.command` loop, on its
exit code 75; on a Mac never by an exec of the running program: README.md,
"Updates"), and removes the
previous install after its first connection to the service. A refusal at any step leaves the running program
as it is. The service at masseuse.ai plays no part: it names no version and
serves no file. `masseuse-camlink update` runs the same steps from a
terminal and prints what it verified; the release workflow runs it against
every release it publishes, on the three platforms, both as the release
itself ("up to date") and as an older release told to update
(`update-check*` in `.github/workflows/release.yml`), so the path a
person's computer takes at the next release has run at this one.

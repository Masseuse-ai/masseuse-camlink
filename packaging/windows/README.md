# The Windows package

The Windows download is one file, `Masseuse.exe`: the whole program, run
from wherever it was saved, nothing installed. It is named the way the Mac
bundle is (`Masseuse.app`): the program is Masseuse.ai, the file shows as
Masseuse. Inside it, after the executable's image and before its
signature, is a *payload* (`internal/payload`) with the files the program
needs beside it on Windows:

```
Masseuse.exe               the desktop window (desktop/, docs/DESKTOP.md), built for the GUI subsystem, then:
  masseuse-camlink.exe     the connector, byte for byte goreleaser's windows_amd64 binary; signed
  ffmpeg.exe               packaging/ffmpeg/build.sh -t windows: static, LGPL, dshow + h264_mf + libopus; signed
  units/camlink-unit-*.exe the unit driver helpers (packaging/units/fetch.sh, verified against the signed manifest); signed
  README.txt               what the unpacked files are, the SmartScreen prompt, the N editions
  LICENSE, NOTICE          the connector's licence (Apache-2.0)
  THIRD_PARTY.md           ffmpeg's provenance and licence
  licenses/                the texts from the ffmpeg and opus tarballs built
  <manifest, footer>       each file's name, size and SHA-256; where the payload begins and ends
<Authenticode signature>   Azure Artifact Signing, Principled Labs, Inc., when the release has the credentials
```

The first time it runs, the window unpacks the payload under the state
directory, `%LOCALAPPDATA%\masseuse-camlink\bin\<id>\`
(`internal/payload.UnpackUnder`, from `desktop/locate.go`; the id names the
contents, so a new version unpacks beside the old one's directory, which
is then removed), checks every file against the manifest, and starts the
connector from there, which finds ffmpeg and the helpers beside itself
(`internal/capture`, `helpersDir`). A start that finds the files whole
leaves them; a damaged or missing file is written again. `Masseuse.exe
--version` never unpacks anything. Until the window, the file itself was
the connector, which unpacked its own payload the same way
(`cmd/masseuse-camlink/payload.go` still does, for a connector that
carries one).

Until v0.12 the download was a zip with those files beside an executable
called `Masseuse.ai.exe`. Opened from inside Explorer's zip preview, which
is how many people open a downloaded zip, only that executable was
extracted (to `%TEMP%`), and the program ran without ffmpeg and without
its helpers, telling the person to install ffmpeg. One file has nothing to
lose that way. The zip, `Masseuse.ai-<version>-windows.zip`, is still
published with this same file inside under the old name, because the
installs of those releases update themselves from it (`internal/update`:
their updater runs `--version` on the zip's `Masseuse.ai.exe` and swaps it
in under the running program's name); a package swapped in that way keeps
that name on disk, runs from its payload, ignores the old `ffmpeg.exe` and
`units\` left beside it, and takes its later updates from `Masseuse.exe`
like any other, the package being told by its payload, not its name.

The name people see is **Masseuse.ai**: the executable, Explorer's icon and
Details tab (`ProductName`, `FileDescription`, from `desktop/winres.json`
and `winres.json` here, both made into resource objects by
`make-syso.sh`), the window's title, the signature's description.
`masseuse-camlink` stays the program's name for engineers: the archives'
binary, the state directory, the flags.

## What the program does on Windows

Opened from Explorer, `Masseuse.exe` is the window (no console of its own:
it is built with `-H windowsgui`); the connector runs hidden behind it
(`desktop/hide_windows.go`, `CREATE_NO_WINDOW`), its log going to
`desktop.log` under the state directory. Closing the window ends the
connector cleanly: the unit is put back to zero and released as on Ctrl-C.
The connector alone, `masseuse-camlink.exe` from the payload directory or
the `windows_*` archive, is a console program
(`cmd/masseuse-camlink/console_windows.go`): opened from Explorer it gets a
window of its own titled Masseuse.ai, which waits for Enter after an error
so the message can be read; started from a terminal, none of this shows.

ffmpeg encodes with Media Foundation's H.264 encoder (`h264_mf`): the
graphics chip's when there is one, else the software encoder Windows
carries. ffmpeg loads Media Foundation at run time, so on an N edition the
program starts and the camera fails with ffmpeg's own words until
Microsoft's Media Feature Pack is installed (`README.txt` says so). A
stimulation unit is reached over the Windows Runtime's Bluetooth Low Energy
classes (`internal/ble/ble_windows.go`, winrt-go's bindings over the COM
vtables, no cgo; Windows 10 version 1703 or newer), with no permission
prompt, or, through the MK-312 helper, over a USB serial adapter
(`internal/serialport`, the registry's `SERIALCOMM` and `FTDIBUS` keys;
the FTDI driver comes from Windows Update).

## Signing

Every program in the package is Authenticode-signed when the release runs
with the credentials: `ffmpeg.exe` and each helper before packing (each
runs as a program of its own once unpacked, and Smart App Control judges
every one), then `Masseuse.exe` with the payload inside it. The
signatures are made by [Azure Artifact Signing](https://learn.microsoft.com/azure/artifact-signing/)
(Trusted Signing, renamed): Microsoft's own CA, short-lived certificates,
RFC 3161 timestamps, RSA (Smart App Control does not accept ECC), and no
key anywhere: the workflow logs in to Azure with GitHub's OIDC token, the
same way it signs keyless with cosign. The publisher Windows shows is the
validated legal entity, **Principled Labs, Inc.**

What the repository needs, all set in Settings:

| kind | name | value |
| --- | --- | --- |
| secret | `AZURE_CLIENT_ID` | the Entra application (app registration) that holds the federated credential |
| secret | `AZURE_TENANT_ID` | its tenant |
| secret | `AZURE_SUBSCRIPTION_ID` | the subscription with the Artifact Signing account |
| variable | `ARTIFACT_SIGNING_ENDPOINT` | the account's regional endpoint, `https://<region>.codesigning.azure.net/` |
| variable | `ARTIFACT_SIGNING_ACCOUNT` | the Artifact Signing account name |
| variable | `ARTIFACT_SIGNING_PROFILE` | the certificate profile (Public Trust) name |

On the Azure side: an Artifact Signing account (a pay-as-you-go
subscription; billing starts with the account), an organization identity
validation for Principled Labs, Inc. (the certificate carries the
validated legal name and address), a Public Trust certificate profile, an
app registration with a **federated credential** for GitHub Actions whose
subject is `repo:FemLed/masseuse-camlink:environment:release` (the
`windows-app` job runs in the `release` environment for exactly this; the
environment needs no protection rules), and that application given the
role *Artifact Signing Certificate Profile Signer* on the account. Without
the secret and the endpoint variable, the "Whether this run signs" step
says so and the package ships unsigned.

Signing is a per-build input, so a rebuild can never match a release byte
for byte; `cmd/pestrip` (`internal/pesig`) removes the signature, the way
`cmd/machostrip` does for the Mac, and the payload with it. The workflow
uses it as its gate: each signed file stripped must hash to the file that
was verified a moment before (for ffmpeg, whose linker writes a PE
checksum, both sides are stripped), the package stripped must hash to the
window built in the same run, and the connector in the payload, stripped,
to the published `windows_amd64` connector. `VERIFY.md`, "The Windows
package", says how a reader repeats it.

SmartScreen still asks before the first start of a release until Microsoft
has seen enough of it, but the reputation accrues to the publisher and
carries to the next release; Smart App Control, which blocks unsigned
programs outright on a Windows 11 machine that has it on, accepts a valid
signature from a CA in Microsoft's Trusted Root Program, which Artifact
Signing's is.

## Files here

| File | Role |
| --- | --- |
| `winres.json` | the resources linked into the executable: the icon and the version-information strings (no version numbers, so the object is static) |
| `Masseuse.ai.ico` | the icon, from the same 1024 px master as the Mac's `.icns` (`packaging/macos/masseuse-camlink-icon-1024.png`): 16, 32, 64 and 128 px as bitmaps, 256 px as PNG |
| `make-syso.sh` | runs a pinned `go-winres` on `winres.json` and writes `cmd/masseuse-camlink/rsrc_windows_amd64.syso` and `rsrc_windows_arm64.syso`, which are committed; the Go linker picks them up by name on a windows build, so goreleaser's proxy build and the `reproduce` job link the same bytes. `ci.yml` regenerates them and fails if they differ from what is committed |
| `verinfo/` | `go run ./packaging/windows/verinfo PROGRAM.exe` prints the strings Explorer's Details tab shows, read with the same Win32 calls Explorer uses; the workflows check the built connector with it. PowerShell's `(Get-Item x.exe).VersionInfo` is not used: .NET reports every string empty when the table has no `FileVersion` string, and this resource has none on purpose |
| `README.txt` | goes into the payload as is (with Windows line endings), unpacked beside ffmpeg |
| `pack/` | `go run ./packaging/windows/pack -v VERSION -s SHELL -b CONNECTOR -f FFMPEG_DIR [-u UNITS_DIR] -o OUT.exe` appends the payload (the connector first) to the window and checks the result: the payload located and verified in the file written, and `internal/payload.Strip` giving the window back. Plain Go, so it runs on the Windows release runner and the Linux CI runner alike; nothing is signed here |
| `../../desktop/winres.json` | the window's resources: the same icon and strings, plus the application manifest the window toolkit wants (per-monitor DPI awareness, common controls v6), written by go-winres from the fields there; `make-syso.sh` makes `desktop/rsrc_windows_amd64.syso` from it |

## How the release builds it

`.github/workflows/release.yml`: the `ffmpeg-windows` job cross-compiles
`ffmpeg.exe` on Linux (mingw-w64, the recipe hashed into a cache key) and
hands it to `windows-app` as an artifact. `windows-app`, on a Windows
runner, verifies the published `windows_amd64` archive against the signed
`checksums.txt`, runs the smoke test (`packaging/ffmpeg/smoke.sh -t windows`)
and the connector's real-ffmpeg test against the `ffmpeg.exe` it will pack,
builds the window (the page, then `go build -H windowsgui` with the tag as
its version), checks both executables' resources, fetches and verifies the
helpers, signs the connector, ffmpeg and the helpers (when it can) and
checks them with `pestrip`, packs the payload behind the window, signs the
package and checks it with `pestrip` and with Windows' own verdict
(`Get-AuthenticodeSignature`), runs the window's `--version` from a
directory of its own and the connector from the payload (the helper is
found, ffmpeg runs), writes the zip for older installs, writes `checksums-windows.txt`,
signs it keyless with cosign and uploads all of it; `provenance-windows`
attests the package and the zip as `windows.intoto.jsonl`;
`update-check-windows` has the published package update itself from the
release. `scripts/verify-release.sh` checks the result as step 9;
`VERIFY.md`, "The Windows package", by hand.

## Build it by hand

On Linux with `gcc-mingw-w64-x86-64`, `gcc`, `nasm`, `pkg-config` and
`make`:

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o dist/masseuse-camlink.exe ./cmd/masseuse-camlink
(cd desktop/frontend && npm ci && npm run build)
(cd desktop && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags production -trimpath -buildvcs=false -ldflags='-w -s -H windowsgui -X main.version=v0.0.0' -o ../dist/shell/Masseuse.exe .)
sh packaging/ffmpeg/build.sh -t windows -o dist/ffmpeg-windows    # a few minutes
sh packaging/units/fetch.sh "$(cat packaging/units/VERSION)" windows amd64 dist/units
go run ./packaging/windows/pack -v 0.0.0 -s dist/shell/Masseuse.exe -b dist/masseuse-camlink.exe -f dist/ffmpeg-windows -u dist/units -o dist/Masseuse.exe
go run ./cmd/pestrip -sha256 dist/Masseuse.exe    # the window's hash
go run ./cmd/pestrip -payload dist/payload dist/Masseuse.exe && sha256sum dist/payload/masseuse-camlink.exe    # the connector's
```

Then on a Windows machine, `sh packaging/ffmpeg/smoke.sh -t windows
dist/ffmpeg-windows/ffmpeg.exe` under Git's bash, and open
`dist/Masseuse.exe` from anywhere. Unsigned, it runs after SmartScreen's
"More info", "Run anyway"; a machine with Smart App Control enforcing will
not run an unsigned build at all.

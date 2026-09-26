# The Windows package

The Windows download is `Masseuse.ai-<version>-windows.zip`. Unzipped, it is
one folder that is the whole program; nothing is installed:

```
Masseuse.ai-0.8.0-windows/
  Masseuse.ai.exe        the connector, byte for byte goreleaser's masseuse-camlink.exe
  ffmpeg.exe             packaging/ffmpeg/build.sh -t windows: static, LGPL, dshow + h264_mf + libopus
  README.txt             what the files are, the SmartScreen prompt, the N editions
  LICENSE, NOTICE        the connector's licence (Apache-2.0)
  THIRD_PARTY.md         ffmpeg's provenance and licence
  licenses/              the texts from the ffmpeg and opus tarballs built
```

The name people see is **Masseuse.ai**: the executable, Explorer's icon and
Details tab (`ProductName`, `FileDescription`), the console window's title.
`masseuse-camlink` stays the program's name for engineers: the archives'
binary, the state directory (`%LOCALAPPDATA%\masseuse-camlink`), the flags.

## What the program does on Windows

Opened from Explorer, `Masseuse.ai.exe` is a console program with a window
of its own (`cmd/masseuse-camlink/console_windows.go`): the title is set to
Masseuse.ai, and when the program stops with an error while it is the only
process on that console (so the window would vanish with it), it waits for
Enter first so the message can be read. Closing the window is
`CTRL_CLOSE_EVENT`, which Go delivers as SIGTERM: the unit is put back to
zero and released as on Ctrl-C. Started from a terminal, none of this shows:
the title is the shell's again afterwards and an error returns to the prompt.

The connector finds `ffmpeg.exe` beside its own executable before it looks
on `PATH` (`internal/capture`, `FindFFmpeg`). ffmpeg encodes with Media
Foundation's H.264 encoder (`h264_mf`): the graphics chip's when there is
one, else the software encoder Windows carries. ffmpeg loads Media
Foundation at run time, so on an N edition the program starts and the
camera fails with ffmpeg's own words until Microsoft's Media Feature Pack is
installed (`README.txt` says so). A stimulation unit is reached over the
Windows Runtime's Bluetooth Low Energy classes (`internal/ble/ble_windows.go`,
winrt-go's bindings over the COM vtables, no cgo; Windows 10 version 1703
or newer), with no permission prompt: a program run from its own window may
use Bluetooth as soon as the radio is on.

## Files here

| File | Role |
| --- | --- |
| `winres.json` | the resources linked into the executable: the icon and the version-information strings (no version numbers, so the object is static) |
| `Masseuse.ai.ico` | the icon, from the same 1024 px master as the Mac's `.icns` (`packaging/macos/masseuse-camlink-icon-1024.png`): 16, 32, 64 and 128 px as bitmaps, 256 px as PNG |
| `make-syso.sh` | runs a pinned `go-winres` on `winres.json` and writes `cmd/masseuse-camlink/rsrc_windows_amd64.syso` and `rsrc_windows_arm64.syso`, which are committed; the Go linker picks them up by name on a windows build, so goreleaser's proxy build and the `reproduce` job link the same bytes. `ci.yml` regenerates them and fails if they differ from what is committed |
| `verinfo/` | `go run ./packaging/windows/verinfo PROGRAM.exe` prints the strings Explorer's Details tab shows, read with the same Win32 calls Explorer uses; the workflows check the built connector with it. PowerShell's `(Get-Item x.exe).VersionInfo` is not used: .NET reports every string empty when the table has no `FileVersion` string, and this resource has none on purpose |
| `README.txt` | goes into the zip as is (with Windows line endings) |
| `build-zip.sh` | assembles the zip from a connector binary and `build.sh -t windows`'s output; plain `sh`, runs under Git's bash on the Windows runner (7-Zip) and on Linux (Info-ZIP) in `ci.yml` with a stand-in ffmpeg |

## How the release builds it

`.github/workflows/release.yml`: the `ffmpeg-windows` job cross-compiles
`ffmpeg.exe` on Linux (mingw-w64, the recipe hashed into a cache key) and
hands it to `windows-app` as an artifact. `windows-app`, on a Windows
runner, verifies the published `windows_amd64` archive against the signed
`checksums.txt`, runs the smoke test (`packaging/ffmpeg/smoke.sh -t windows`)
and the connector's real-ffmpeg test against the `ffmpeg.exe` it will pack,
checks the executable's resources, assembles the zip, writes
`checksums-windows.txt`, signs it keyless with cosign and uploads all of it;
`provenance-windows` attests the zip as `windows.intoto.jsonl`.
`scripts/verify-release.sh` checks the result as step 9; `VERIFY.md`, "The
Windows package", by hand.

Nothing is signed with a Windows certificate yet, so SmartScreen warns at
the first start ("More info", "Run anyway"). Authenticode signing is its
own task; the zip's contents will not change for it.

## Build it by hand

On Linux with `gcc-mingw-w64-x86-64`, `gcc`, `nasm`, `pkg-config` and
`make`:

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o dist/masseuse-camlink.exe ./cmd/masseuse-camlink
sh packaging/ffmpeg/build.sh -t windows -o dist/ffmpeg-windows    # a few minutes
sh packaging/windows/build-zip.sh -v 0.0.0 -b dist/masseuse-camlink.exe -f dist/ffmpeg-windows -o dist
```

Then on a Windows machine, `sh packaging/ffmpeg/smoke.sh -t windows
dist/ffmpeg-windows/ffmpeg.exe` under Git's bash, and open
`dist/Masseuse.ai-0.0.0-windows.zip`.

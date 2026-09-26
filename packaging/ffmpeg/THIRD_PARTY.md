# Third-party software in the Masseuse.ai downloads

The Mac app (`Masseuse.app`) and the Windows package
(`Masseuse.ai-<version>-windows.zip`) carry, beside the connector, an
`ffmpeg` program (`Contents/Helpers/ffmpeg` in the app; `ffmpeg.exe` beside
`Masseuse.ai.exe` in the zip) so that the computer's camera and microphone
can be sent without installing anything else. The connector itself is
FemLed's and Apache-2.0 (`LICENSE`, `NOTICE`); the ffmpeg program is built
from the following upstream sources, unmodified, by
`packaging/ffmpeg/build.sh`.

| Component | Version | Licence | Source tarball | SHA-256 |
| --- | --- | --- | --- | --- |
| FFmpeg | 9.0.1 | LGPL 2.1 or later | `ffmpeg-9.0.1.tar.xz` from <https://ffmpeg.org/releases/> | `cf38e0e28c7e5605942c4a77755349b0145804a397af37eb1fb4c77cb237f635` |
| Opus (libopus) | 1.6.1 | BSD-3-Clause | `opus-1.6.1.tar.gz` from <https://downloads.xiph.org/releases/opus/> | `6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1` |

The ffmpeg tarball's signature by the FFmpeg release signing key
(`FCF9 86EA 15E6 E293 A564  4F10 B432 2F04 D676 58D8`) was checked when the
version was pinned; the build verifies the hashes above before it unpacks
anything.

## How it is configured

FFmpeg is built with `--disable-gpl --disable-nonfree`: no GPL component
(there is no libx264; H.264 is encoded by the operating system's own
encoder) and no nonfree component. With `--disable-everything
--disable-autodetect` the program holds only what the connector asks for,
listed in `build.sh` next to the configure flags: raw video and PCM
decoders, the `libopus` encoder, the filters ffmpeg inserts for the output
options, the RTSP output over TCP, plus the `lavfi` test source and `null`
output the smoke test uses; and, per system:

- **macOS** (`build.sh -t darwin`): the AVFoundation input and the
  `h264_videotoolbox` encoder (hardware or, with `-allow_sw 1`, Apple's
  software encoder). The universal binary is the arm64 slice and the x86_64
  slice joined by `lipo`; both slices target macOS 13. Built on a macOS
  runner with the Xcode command line tools.
- **Windows** (`build.sh -t windows`): the DirectShow (`dshow`) input and
  the `h264_mf` encoder (Media Foundation: the graphics chip's encoder, or
  the software H.264 encoder Windows carries). Cross-compiled on a Linux
  runner with mingw-w64 (`x86_64-w64-mingw32`), linked static, so the
  program depends on Windows' own libraries only. ffmpeg loads Media
  Foundation (`mfplat.dll`) at run time, so the program starts on an N
  edition of Windows too; there the encoder is missing until Microsoft's
  Media Feature Pack is installed.

libopus is linked statically in both. The exact flags are the ones in
`build.sh` at the release's tag; the release notes name the tag and the
workflow run that built each download, and each download's provenance
(`darwin.intoto.jsonl`, `windows.intoto.jsonl`) is signed by that workflow.

## Where the notices are

Inside the Mac app, `Contents/Resources/licenses/` holds the texts as they
come from the tarballs built: `ffmpeg-COPYING.LGPLv2.1` and
`ffmpeg-LICENSE.md` for FFmpeg, `opus-COPYING` for Opus; this file is
`Contents/Resources/THIRD_PARTY.md`. In the Windows zip the same texts are in
`licenses/` and this file is `THIRD_PARTY.md`, beside the program.

## Source offer (LGPL 2.1, section 6)

Every release of masseuse-camlink that ships these downloads attaches the two
source tarballs named above to the GitHub release, and lists them in
`checksums-darwin.txt` next to the disk image. Anyone can rebuild the
`ffmpeg` in either download from them with `packaging/ffmpeg/build.sh` at
the same tag, and replace `Contents/Helpers/ffmpeg` or `ffmpeg.exe` with
their own build: the connector runs whatever `ffmpeg` it finds there
(`internal/capture`, `FindFFmpeg`), and a Mac app so modified simply is no
longer signed by FemLed.

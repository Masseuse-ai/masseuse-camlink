# Third-party software in the macOS application bundle

`masseuse-camlink.app` carries, beside the connector, an `ffmpeg` program
(`Contents/Helpers/ffmpeg`) so that the computer's camera and microphone can
be sent without installing anything else. The connector itself is FemLed's
and Apache-2.0 (`LICENSE`, `NOTICE`); the ffmpeg program is built from the
following upstream sources, unmodified, by `packaging/ffmpeg/build.sh`.

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
(there is no libx264; H.264 is encoded by the system's VideoToolbox, in
hardware or, with `-allow_sw 1`, in software) and no nonfree component. With
`--disable-everything --disable-autodetect` the program holds only what the
connector asks for, listed in `build.sh` next to the configure flags: the
AVFoundation input, raw video and PCM decoders, the `h264_videotoolbox` and
`libopus` encoders, the filters ffmpeg inserts for the output options, and
the RTSP output over TCP, plus the `lavfi` test source and `null` output the
smoke test uses. libopus is linked statically. The universal binary is the
arm64 slice and the x86_64 slice joined by `lipo`; both slices target
macOS 13.

The exact flags are the ones in `build.sh` at the release's tag; the release
notes name the tag and the workflow run that built the bundle, and the
bundle's provenance (`darwin.intoto.jsonl`) is signed by that workflow.

## Where the notices are

Inside the bundle, `Contents/Resources/licenses/` holds the texts as they
come from the tarballs built: `ffmpeg-COPYING.LGPLv2.1` and
`ffmpeg-LICENSE.md` for FFmpeg, `opus-COPYING` for Opus. This file is
`Contents/Resources/THIRD_PARTY.md`.

## Source offer (LGPL 2.1, section 6)

Every release of masseuse-camlink that ships the bundle attaches the two
source tarballs named above to the GitHub release, and lists them in
`checksums-darwin.txt` next to the disk image. Anyone can rebuild the
`ffmpeg` in the bundle from them with `packaging/ffmpeg/build.sh` at the
same tag, and replace `Contents/Helpers/ffmpeg` with their own build: the
connector runs whatever `ffmpeg` it finds there (`internal/capture`,
`FindFFmpeg`), and a bundle so modified simply is no longer signed by
FemLed.

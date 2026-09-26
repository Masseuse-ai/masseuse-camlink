# The macOS application bundle and disk image

Mac users get the connector as `Masseuse.app` inside
`Masseuse.ai-<version>.dmg`: one download for Apple silicon and Intel,
signed with FemLed's Apple Developer ID, notarized and stapled, so it opens
from the Finder without a Gatekeeper refusal. Gatekeeper accepts a
double-click only on an application bundle, an installer or a disk image; a
bare executable, however well signed and notarized, is refused as "not an
app". That is why the bundle exists.

Masseuse.ai is the name a person sees: the disk image and its volume, the
Terminal window's title, the first line the program prints, the permission
prompts. The bundle alone is `Masseuse.app`, shown as **Masseuse** in the
Finder, Launchpad and the Dock. It cannot be `Masseuse.ai.app`: the Finder
and Launchpad refuse to hide `.app` when the rest of the name ends in a
file extension the system knows, a guard against `Invoice.pdf.app` passing
for a document, and `.ai` is Adobe Illustrator's, declared by macOS itself
(`com.adobe.illustrator.ai-image`) whether or not Illustrator is
installed. v0.8.0 and v0.8.1 were `Masseuse.ai.app` and sat under the
icon as "Masseuse.ai.app"; a localized `CFBundleDisplayName` of
`Masseuse.ai` is refused by the same rule, and a look-alike dot would be
the very trick the rule exists to catch. `CFBundleName`,
`CFBundleDisplayName` and `CFBundleExecutable` match the bundle's name, as
the Finder expects. masseuse-camlink is the program's name for engineers
and stays in the bundle identifier (`ai.masseuse.camlink`), the state
directory (`~/Library/Application Support/masseuse-camlink`) and the
archives. Releases before v0.8.0 named the app and the image
masseuse-camlink too.

The bundle's executable is the connector itself (`cmd/masseuse-camlink`),
the universal binary made from the two signed release binaries with
`lipo`, under the name `Masseuse`. Opened from the Finder, it has no
terminal to print the pairing code to, so it writes a small
`Masseuse.ai.command` file into its state directory and asks the system to
open it; Terminal runs it, and it runs the connector in that window in
console mode, in a loop that runs it again when it ends with exit code 75,
which is how an update restarts it as the new version at the same path
(`cmd/masseuse-camlink/desktop.go`, `internal/update` `ErrRelaunch`). If a
connector is already running on that state directory, opening the app
again only brings Terminal forward. The bundle is `LSUIElement`, so nothing bounces in the Dock.
`ffmpeg` ships inside (`Contents/Helpers/ffmpeg`, built by
`packaging/ffmpeg/build.sh`), and the connector looks there before it looks
at `PATH`, so there is no Homebrew step. The unit driver helpers ship
beside it (`Contents/Helpers/units/`, fetched and verified by
`packaging/units/fetch.sh`; `docs/UNITS.md`), and the connector looks
there for them; a bundle without that directory serves the Mastago alone.

```
Masseuse.app/Contents/
  Info.plist                     from Info.plist here, version filled in
  PkgInfo
  MacOS/Masseuse                 the connector, universal, Developer ID + hardened runtime
  Helpers/ffmpeg                 universal, Developer ID + hardened runtime + ffmpeg.entitlements
  Helpers/units/camlink-unit-*   unit driver helpers (docs/UNITS.md), universal, Developer ID + hardened runtime; from packaging/units/fetch.sh, absent in a build without them
  Resources/masseuse-camlink.icns  the icon for macOS 13 to 15 (CFBundleIconFile)
  Resources/Assets.car           the icon for macOS 26, compiled from masseuse-camlink.icon (CFBundleIconName)
  Resources/LICENSE, NOTICE      the connector's (Apache-2.0)
  Resources/THIRD_PARTY.md       what ffmpeg is and how it is built
  Resources/licenses/            the LGPL 2.1 text and the other notices, from the tarballs built
```

## Files here

| File | What |
| --- | --- |
| `Info.plist` | the bundle's property list, `@VERSION@` filled in by `build-app.sh`; `CFBundleName`, `CFBundleDisplayName` and `CFBundleExecutable` all `Masseuse`, the bundle's name (above); `LSMinimumSystemVersion` 13.0 (Go's floor for macOS binaries), `LSUIElement`, the camera, microphone and Bluetooth usage strings |
| `ffmpeg.entitlements` | camera and microphone, which a hardened-runtime process may open only with these |
| `build-app.sh` | assembles the bundle from a connector binary, `packaging/ffmpeg/build.sh`'s output, the unit driver helpers (`-u`, `packaging/units/fetch.sh`'s darwin/all output, each checked universal) and the files here, and compiles the macOS 26 icon with `actool` (Xcode 26 or later, on macOS 26; `DEVELOPER_DIR` picks an Xcode when the selected one is older); `sh` otherwise, runs unsigned in `ci.yml` on every pull request |
| `sign-notarize.sh` | temporary keychain from the release secrets, `codesign` (ffmpeg and each `Helpers/units/camlink-unit-*` first, the latter as `ai.masseuse.camlink.unit.<name>` without entitlements, then the bundle, `--options runtime --timestamp`), `notarytool submit --wait`, `stapler staple`; the same for the disk image. The identity is picked by the certificate's SHA-1 from VERIFY.md, never by its subject, and the subject is never printed |
| `assess.sh` | the gates: `codesign --verify --deep --strict`, `spctl --assess --type execute` (bundle) and `--type open --context context:primary-signature` (image) answering `accepted` with `source=Notarized Developer ID`, `stapler validate`; the release fails if any is false |
| `build-dmg.sh` | the app and an `Applications` shortcut on an HFS+ image named `Masseuse.ai` with the volume icon set, compressed read-only (`hdiutil`); file name `Masseuse.ai-<version>.dmg` |
| `masseuse-camlink.icns` | the icon (below) as a bitmap, for macOS 13 to 15; the file keeps its name, `CFBundleIconFile` points at it |
| `masseuse-camlink-icon-1024.png` | its 1024 px master, also the source of the Windows icon (`packaging/windows/`) |
| `masseuse-camlink.icon/` | the icon as layers, for macOS 26 (below): an Icon Composer document, `icon.json` and three SVGs, which `build-app.sh` compiles into the bundle's `Assets.car`; `CFBundleIconName` points at it |

The release workflow (`.github/workflows/release.yml`, job `macos-app`) runs
them in this order on a macOS 26 runner, after goreleaser has published the
archives: verify the two darwin archives against the signed
`checksums.txt`, `lipo -create` the connectors, build or restore ffmpeg,
`build-app.sh`, `sign-notarize.sh app`, `assess.sh app`, `build-dmg.sh`,
`sign-notarize.sh dmg`, `assess.sh dmg`, then the reproducibility check
(`cmd/machostrip -sha256` of the bundle's executable, one hash per
architecture, equals that of the archives' binaries) and the upload with
`checksums-darwin.txt`, its cosign bundle and the SLSA provenance
`darwin.intoto.jsonl`. VERIFY.md, "The macOS app", says how to check all of
it from the outside.

To build an unsigned bundle by hand for a look (the signing needs the
release secrets; the icon needs macOS 26 with Xcode 26 or later selected,
`xcode-select -p`, or named in `DEVELOPER_DIR`):

```sh
go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o dist/masseuse-camlink ./cmd/masseuse-camlink
sh packaging/ffmpeg/build.sh -t darwin -o dist/ffmpeg -a "$(uname -m)"  # a few minutes; one architecture
sh packaging/macos/build-app.sh -v 0.0.0 -b dist/masseuse-camlink -f dist/ffmpeg -o dist
open dist/Masseuse.app                                          # Terminal opens with the connector
```

## The icon

The masseuse.ai mark as a macOS icon for the connector: the brand's serif M
in white over its purple wave, the wave running between two electrode pads,
on the dark tile. It ships twice, because macOS changed how it draws app
icons.

`masseuse-camlink.icns` is the icon as bitmaps, laid out on the app icon
grid of macOS 11 to 15 (the tile 824 of a 1024 canvas with a transparent
margin, corners at 22.37 % of the tile): the iconset (16, 32, 128, 256 and
512 px, each with its `@2x`) folded by `iconutil`, each size rendered from
the vector, not downsampled. It is the bundle's `CFBundleIconFile` and,
copied to the volume root as `.VolumeIcon.icns` with the custom-icon
attribute set, the disk image's icon.

macOS 26 draws app icons itself: from an Icon Composer document (layers on
the full canvas and a fill), it cuts the rounded square, lights its edge
and shades the layers, in every size and appearance. An app that brings
only bitmaps gets them shrunk onto a grey rounded square of the system's,
the shape they did not fill; that is how v0.8.0 to v0.8.2 sat in the
Finder and Launchpad, a small dark tile in a light frame among full-size
icons. `masseuse-camlink.icon/` is the same mark as that document:
`icon.json` (the ink gradient as the fill; one group of three opaque
layers with the system's edge light and a soft shadow, no translucency, so
the M stays the wordmark's white) and `Assets/m.svg`, `wave.svg`,
`pads.svg`, the mark's parts on the 1024 canvas, the pads spanning 80 % of
it as they span 80 % of the old tile. `build-app.sh` compiles it with
`actool` (Xcode 26 or later, and on macOS 26: on macOS 15 the tool's
asset runtime crashes, so the release and CI jobs that build the bundle run
on `macos-26` images) into `Contents/Resources/Assets.car`,
which `CFBundleIconName` (`masseuse-camlink`, the icns's name too) points
at. macOS 26 reads the catalog; macOS 13 to 15 find a flattened rendering
of the same layers in it, which `actool` adds in every size, and the icns
under `CFBundleIconFile` behind that. `actool` also drops a small icns of
its own into its output directory; the script does not take it. The
catalog is not reproducible byte for byte (`actool` names its renditions
with fresh UUIDs), which the release does not claim: the reproducibility
check is the executable's (VERIFY.md).

The icns, its master and the `.icon` document are generated by the
masseuse.ai brand build, which also writes the site's icons from the same
geometry, so they are not edited here: regenerate there and copy. To look
at the document as the system will draw it, Icon Composer (in Xcode 26)
opens it, and its command-line tool renders it:

```sh
"$(xcode-select -p)/../Applications/Icon Composer.app/Contents/Executables/ictool" \
  packaging/macos/masseuse-camlink.icon --export-image --output-file /tmp/icon.png \
  --platform macOS --rendition Default --width 256 --height 256 --scale 2
```

The letter is an outline of a Charis SIL glyph (SIL Open Font License
1.1); rendered as an image it is not a font and carries no naming
obligation.

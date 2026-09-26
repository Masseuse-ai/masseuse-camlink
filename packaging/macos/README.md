# The macOS application bundle and disk image

Mac users get masseuse-camlink as `masseuse-camlink.app` inside
`masseuse-camlink_<version>_darwin_all.dmg`: one download for Apple silicon
and Intel, signed with FemLed's Apple Developer ID, notarized and stapled,
so it opens from the Finder without a Gatekeeper refusal. Gatekeeper
accepts a double-click only on an application bundle, an installer or a
disk image; a bare executable, however well signed and notarized, is
refused as "not an app". That is why the bundle exists.

The bundle's executable is the connector itself (`cmd/masseuse-camlink`),
the universal binary made from the two signed release binaries with
`lipo`. Opened from the Finder, it has no terminal to print the pairing
code to, so it writes a small `.command` file into its state directory and
asks the system to open it; Terminal runs it, and it runs the connector in
that window in console mode (`cmd/masseuse-camlink/desktop.go`). If a
connector is already running on that state directory, opening the app
again only brings Terminal forward. The bundle is `LSUIElement`, so nothing
bounces in the Dock. `ffmpeg` ships inside (`Contents/Helpers/ffmpeg`, built
by `packaging/ffmpeg/build.sh`), and the connector looks there before it
looks at `PATH`, so there is no Homebrew step.

```
masseuse-camlink.app/Contents/
  Info.plist                     from Info.plist here, version filled in
  PkgInfo
  MacOS/masseuse-camlink         the connector, universal, Developer ID + hardened runtime
  Helpers/ffmpeg                 universal, Developer ID + hardened runtime + ffmpeg.entitlements
  Resources/masseuse-camlink.icns
  Resources/LICENSE, NOTICE      the connector's (Apache-2.0)
  Resources/THIRD_PARTY.md       what ffmpeg is and how it is built
  Resources/licenses/            the LGPL 2.1 text and the other notices, from the tarballs built
```

## Files here

| File | What |
| --- | --- |
| `Info.plist` | the bundle's property list, `@VERSION@` filled in by `build-app.sh`; `LSMinimumSystemVersion` 13.0 (Go's floor for macOS binaries), `LSUIElement`, the camera, microphone and Bluetooth usage strings |
| `ffmpeg.entitlements` | camera and microphone, which a hardened-runtime process may open only with these |
| `build-app.sh` | assembles the bundle from a connector binary, `packaging/ffmpeg/build.sh`'s output and the files here; plain `sh`, runs unsigned in `ci.yml` on every pull request |
| `sign-notarize.sh` | temporary keychain from the release secrets, `codesign` (ffmpeg first, then the bundle, `--options runtime --timestamp`), `notarytool submit --wait`, `stapler staple`; the same for the disk image. The identity is picked by the certificate's SHA-1 from VERIFY.md, never by its subject, and the subject is never printed |
| `assess.sh` | the gates: `codesign --verify --deep --strict`, `spctl --assess --type execute` (bundle) and `--type open --context context:primary-signature` (image) answering `accepted` with `source=Notarized Developer ID`, `stapler validate`; the release fails if any is false |
| `build-dmg.sh` | the app and an `Applications` shortcut on an HFS+ image with the volume icon set, compressed read-only (`hdiutil`); file name `masseuse-camlink_<version>_darwin_all.dmg` |
| `masseuse-camlink.icns` | the icon (below) |
| `masseuse-camlink-icon-1024.png` | its 1024 px master |

The release workflow (`.github/workflows/release.yml`, job `macos-app`) runs
them in this order on a macOS runner, after goreleaser has published the
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
release secrets):

```sh
go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o dist/masseuse-camlink ./cmd/masseuse-camlink
sh packaging/ffmpeg/build.sh -o dist/ffmpeg -a "$(uname -m)"  # a few minutes; one architecture
sh packaging/macos/build-app.sh -v 0.0.0 -b dist/masseuse-camlink -f dist/ffmpeg -o dist
open dist/masseuse-camlink.app                                  # Terminal opens with the connector
```

## The icon

The masseuse.ai mark as a macOS icon for the connector: the brand's serif M
in white over its purple wave, the wave running between two electrode pads,
on the dark tile, laid out on Apple's app icon grid (the tile 824 of a 1024
canvas with a transparent margin, corners at 22.37 % of the tile).
`masseuse-camlink.icns` is the iconset (16, 32, 128, 256 and 512 px, each
with its `@2x`) folded by `iconutil`, each size rendered from the vector,
not downsampled; it is the bundle's `CFBundleIconFile` and, copied to the
volume root as `.VolumeIcon.icns` with the custom-icon attribute set, the
disk image's icon.

Both files are generated by the masseuse.ai brand build, which also writes
the site's icons from the same geometry, so they are not edited here:
regenerate there and copy. The letter is an outline of a Charis SIL glyph
(SIL Open Font License 1.1); rendered as an image it is not a font and
carries no naming obligation.

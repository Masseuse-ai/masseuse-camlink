#!/usr/bin/env sh
# Verify a masseuse-camlink release end to end (VERIFY.md):
#   1. the checksum file's keyless signature was made by this repository's
#      release workflow on the tag,
#   2. every downloaded artifact matches the checksum file,
#   3. the SLSA provenance names each artifact and this source at this tag,
#      (4 was the container image, which is no longer published; the
#      numbers below are kept so the docs' references stay right),
#   5. (optional, needs Go) the gateway binary rebuilds to the same bytes,
#   6. (optional, needs Go and unzip) so does the connector in the
#      windows/amd64 archive (the one archive published unsigned),
#   7. (optional, needs Go) so do the darwin connectors once their Apple
#      signature is stripped from both sides; on a Mac, the signature itself
#      is checked (team id, certificate fingerprint, notarization),
#   8. when the release carries the macOS app (checksums-darwin.txt): its
#      checksum file's signature, the disk image's and the ffmpeg source
#      tarballs' hashes and provenance; on a Mac, that the app's connector
#      is the archives' binaries (signature stripped, per architecture),
#      that the app's executable and its ffmpeg are signed with the camera
#      and microphone entitlements (v0.18.0 and later; without them macOS
#      refuses the camera silently) and Gatekeeper's own verdict on the app
#      and the image,
#   9. when the release carries the Windows package (checksums-windows.txt):
#      its checksum file's signature, the package's (and the older
#      installs' zip's) hash and provenance, and, with Go, that the package
#      minus its Authenticode signature and its payload is the windows/amd64
#      archive's executable (cmd/pestrip; v0.13.0 and later, a zip holding
#      the files beside the executable before),
#  10. when the release bundles unit driver helpers (packaging/units/VERSION
#      at the tag; v0.10.0 and later): the helpers manifest's signature by
#      the key whose public half the tag carries, and that every helper in
#      the windows/amd64 archive and, signature stripped, in the Windows
#      package's payload is the manifest's byte for byte (the app's are
#      compared by the release itself, signature stripped; VERIFY.md "Unit
#      driver helpers" says how by hand).
#
# usage: scripts/verify-release.sh vX.Y.Z [download dir]
# needs: curl, sha256sum (or shasum), cosign, slsa-verifier; go and unzip
# optional.
set -eu

REPO="FemLed/masseuse-camlink"
# [.] rather than \. : Git Bash on Windows rewrites a backslash in an argument
# to a native program (cosign) as a path separator.
WORKFLOW_RE='^https://github.com/FemLed/masseuse-camlink/[.]github/workflows/release[.]yml@refs/tags/v'
ISSUER="https://token.actions.githubusercontent.com"
# The Apple Developer ID the darwin binaries are signed with (VERIFY.md,
# "The macOS binaries"): the team and the leaf certificate's SHA-256.
APPLE_TEAM_ID="B8Z4RP3846"
APPLE_CERT_SHA256="8D:A7:D2:FD:A7:BE:3C:AC:CE:6E:20:19:FE:0C:1A:68:B2:C1:B5:DC:A0:14:55:68:52:C6:8E:62:3A:35:44:80"

tag="${1:?usage: $0 vX.Y.Z [dir]}"
version="${tag#v}"
dir="${2:-release-$tag}"
mkdir -p "$dir"
cd "$dir"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 2; }; }
need curl; need cosign; need slsa-verifier
if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum"; else need shasum; SHA="shasum -a 256"; fi

base="https://github.com/$REPO/releases/download/$tag"
fetch() { [ -s "$1" ] || curl -fsSL -o "$1" "$base/$1"; }

# at_least X.Y.Z: whether the release being verified is that version or a
# later one, for the checks that only apply from a release on.
at_least() {
  # shellcheck disable=SC2046
  set -- $(printf '%s %s' "$version" "$1" | tr . ' ')
  [ "$1" -gt "$4" ] && return 0; [ "$1" -lt "$4" ] && return 1
  [ "$2" -gt "$5" ] && return 0; [ "$2" -lt "$5" ] && return 1
  [ "${3%%[!0-9]*}" -ge "${6%%[!0-9]*}" ]
}

echo "==> downloading release files for $tag"
fetch checksums.txt
fetch checksums.txt.sigstore.json
fetch multiple.intoto.jsonl
# Every artifact named in the checksum file.
awk '{print $2}' checksums.txt | while read -r f; do fetch "$f"; done

echo "==> 1. checksum signature (keyless, release workflow at a v* tag)"
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp "$WORKFLOW_RE" \
  --certificate-oidc-issuer "$ISSUER" \
  checksums.txt

echo "==> 2. checksums"
$SHA -c checksums.txt

echo "==> 3. SLSA provenance for every artifact"
awk '{print $2}' checksums.txt | while read -r f; do
  slsa-verifier verify-artifact "$f" \
    --provenance-path multiple.intoto.jsonl \
    --source-uri "github.com/$REPO" \
    --source-tag "$tag" >/dev/null
  echo "    ok  $f"
done

if command -v go >/dev/null 2>&1; then
  echo "==> 5. rebuild the gateway from the module proxy and compare (VERIFY.md 3)"
  export GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org GOFLAGS=
  # A scratch GOPATH so the cross-compiled binary has a known home (GOBIN is
  # not allowed for cross builds); the module cache stays shared, so it is
  # resolved before GOPATH is overridden (a shell applies the assignments
  # before a command left to right).
  work="$(mktemp -d)"
  modcache="$(go env GOMODCACHE)"
  GOPATH="$work/gopath" GOMODCACHE="$modcache" GOOS=linux GOARCH=amd64 \
    go install -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
    "github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink-gateway@$tag"
  built=$(find "$work/gopath/bin" -type f -name masseuse-camlink-gateway | head -n 1)
  [ -n "$built" ] || { echo "    go install produced no binary" >&2; exit 1; }
  rebuilt=$($SHA "$built" | cut -d' ' -f1)
  published=$(grep -F "masseuse-camlink-gateway_${version}_linux_amd64" checksums.txt | cut -d' ' -f1)
  if [ "$rebuilt" = "$published" ]; then
    echo "    ok  linux/amd64 gateway reproduces: $rebuilt"
  else
    echo "    MISMATCH: rebuilt $rebuilt, published $published" >&2
    exit 1
  fi

  echo "==> 6. rebuild the connector the way goreleaser's proxy mode does and compare"
  # The windows/amd64 archive is the one published as built: the darwin
  # binaries are signed (step 7 strips the signature), and the Windows
  # signature goes on the package, not on the archive.
  mkdir -p "$work/connector"
  printf 'module connector\n' > "$work/connector/go.mod"
  (cd "$work/connector" \
    && go get "github.com/FemLed/masseuse-camlink@$tag" >/dev/null 2>&1 \
    && GOOS=windows GOARCH=amd64 go build -mod=mod -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
         -o masseuse-camlink.exe github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink)
  rebuilt=$($SHA "$work/connector/masseuse-camlink.exe" | cut -d' ' -f1)
  if command -v unzip >/dev/null 2>&1; then
    published=$(unzip -p "masseuse-camlink_${version}_windows_amd64.zip" masseuse-camlink.exe | $SHA | cut -d' ' -f1)
    if [ "$rebuilt" = "$published" ]; then
      echo "    ok  windows/amd64 connector reproduces: $rebuilt"
    else
      echo "    MISMATCH: rebuilt $rebuilt, in archive $published" >&2
      exit 1
    fi
  else
    echo "    skipped the comparison with the archive (no unzip); rebuilt $rebuilt"
  fi

  echo "==> 7. rebuild the darwin connectors and compare with the signature stripped (VERIFY.md 3)"
  for arch in arm64 amd64; do
    archive="masseuse-camlink_${version}_darwin_${arch}.tar.gz"
    [ -s "$archive" ] || { echo "    skipped darwin/$arch (no $archive in the release)"; continue; }
    (cd "$work/connector" \
      && GOOS=darwin GOARCH="$arch" go build -mod=mod -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
           -o "rebuilt_darwin_$arch" github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink)
    tar -xzOf "$archive" masseuse-camlink > "$work/published_darwin_$arch"
    rebuilt=$(go run "github.com/FemLed/masseuse-camlink/cmd/machostrip@$tag" -sha256 "$work/connector/rebuilt_darwin_$arch" | cut -d' ' -f1)
    stripped=$(go run "github.com/FemLed/masseuse-camlink/cmd/machostrip@$tag" -sha256 "$work/published_darwin_$arch" | cut -d' ' -f1)
    if [ "$rebuilt" != "$stripped" ]; then
      echo "    MISMATCH darwin/$arch: rebuilt $rebuilt, in archive (signature stripped) $stripped" >&2
      exit 1
    fi
    if [ "$($SHA "$work/published_darwin_$arch" | cut -d' ' -f1)" = "$stripped" ]; then
      echo "    the published darwin/$arch binary carries no code signature" >&2
      exit 1
    fi
    echo "    ok  darwin/$arch connector reproduces once its signature is stripped: $rebuilt"
  done

  if [ "$(uname -s)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
    echo "==> 7b. the Apple signature itself (this is a Mac)"
    for arch in arm64 amd64; do
      bin="$work/published_darwin_$arch"
      [ -s "$bin" ] || continue
      chmod +x "$bin"
      codesign --verify --strict "$bin"
      info=$(codesign -dvv "$bin" 2>&1)
      echo "$info" | grep -q "^TeamIdentifier=$APPLE_TEAM_ID\$" \
        || { echo "    darwin/$arch: signed by another team: $(echo "$info" | grep '^TeamIdentifier=')" >&2; exit 1; }
      echo "$info" | grep -q 'flags=0x10000(runtime)' \
        || { echo "    darwin/$arch: not signed with the hardened runtime" >&2; exit 1; }
      mkdir -p "$work/certs_$arch"
      (cd "$work/certs_$arch" && codesign -d --extract-certificates "$bin" 2>/dev/null)
      fp=$(openssl x509 -inform DER -in "$work/certs_$arch/codesign0" -noout -fingerprint -sha256 | sed 's/^.*=//')
      [ "$fp" = "$APPLE_CERT_SHA256" ] \
        || { echo "    darwin/$arch: unexpected signing certificate $fp" >&2; exit 1; }
      # A bare executable is assessed as an "open" with its primary
      # signature; "--type execute" only ever evaluates app bundles.
      assess=$(spctl --assess --type open --context context:primary-signature -vv "$bin" 2>&1 || true)
      echo "$assess" | grep -q 'source=Notarized Developer ID' \
        || { echo "    darwin/$arch: not accepted as notarized:"; echo "$assess" | sed 's/^/      /'; exit 1; }
      echo "    ok  darwin/$arch: team $APPLE_TEAM_ID, certificate $fp, notarized"
    done
  fi
  chmod -R u+w "$work" 2>/dev/null || true
  rm -rf "$work"
else
  echo "==> 5-7. rebuild: skipped (no go)"
fi

# The macOS app (VERIFY.md 3, "The macOS app"): a second checksum file for
# the disk image and the ffmpeg source tarballs, signed and attested like
# the first. Releases before it have no such file.
if [ -s checksums-darwin.txt ] || curl -fsSL -o checksums-darwin.txt "$base/checksums-darwin.txt" 2>/dev/null; then
  echo "==> 8. the macOS app: checksum signature, hashes and provenance"
  fetch checksums-darwin.txt.sigstore.json
  fetch darwin.intoto.jsonl
  cosign verify-blob \
    --bundle checksums-darwin.txt.sigstore.json \
    --certificate-identity-regexp "$WORKFLOW_RE" \
    --certificate-oidc-issuer "$ISSUER" \
    checksums-darwin.txt
  awk '{print $2}' checksums-darwin.txt | while read -r f; do fetch "$f"; done
  $SHA -c checksums-darwin.txt
  awk '{print $2}' checksums-darwin.txt | while read -r f; do
    slsa-verifier verify-artifact "$f" \
      --provenance-path darwin.intoto.jsonl \
      --source-uri "github.com/$REPO" \
      --source-tag "$tag" >/dev/null
    echo "    ok  $f"
  done
  # The disk image is Masseuse.ai-X.Y.Z.dmg holding Masseuse.app (v0.8.2 on;
  # Masseuse.ai.app in v0.8.0 and v0.8.1, masseuse-camlink.app and
  # masseuse-camlink_*.dmg before). The image is read off the checksum file
  # and the bundle found by its extension, so all of them verify.
  dmg=$(awk '{print $2}' checksums-darwin.txt | grep -E '\.dmg$' | head -n 1)
  if [ "$(uname -s)" = "Darwin" ] && [ -n "$dmg" ] && [ -s "$dmg" ]; then
    echo "==> 8b. the app itself (this is a Mac)"
    mount=$(hdiutil attach -readonly -nobrowse -noautoopen "$dmg" | awk -F'\t' '/\/Volumes\//{print $NF}')
    [ -n "$mount" ] || { echo "    the disk image did not mount" >&2; exit 1; }
    app=$(find "$mount" -maxdepth 1 -name '*.app' | head -n 1)
    [ -n "$app" ] || { echo "    no application bundle in $dmg" >&2; hdiutil detach "$mount" -quiet; exit 1; }
    # The connector inside the bundle: beside the desktop window, which is
    # the bundle's executable, from the release that brought the window;
    # the executable itself before that.
    exe="$app/Contents/MacOS/masseuse-camlink"
    [ -f "$exe" ] || exe="$app/Contents/MacOS/$(plutil -extract CFBundleExecutable raw -o - "$app/Contents/Info.plist")"
    echo "    $(basename "$app"), volume $(basename "$mount"), connector $(basename "$exe")"
    if command -v go >/dev/null 2>&1; then
      # The bundle's connector, stripped, is the archives' binaries stripped,
      # architecture by architecture (and so the rebuild of step 7). The
      # window beside it (Contents/MacOS/Masseuse) is not rebuilt byte for
      # byte; its signature is checked below.
      appwork="$(mktemp -d)"
      go run "github.com/FemLed/masseuse-camlink/cmd/machostrip@$tag" -sha256 "$exe" > "$appwork/bundle.txt"
      for arch in arm64 amd64; do
        archive="masseuse-camlink_${version}_darwin_${arch}.tar.gz"
        [ -s "$archive" ] || { echo "    skipped $arch (no $archive)"; continue; }
        tar -xzOf "$archive" masseuse-camlink > "$appwork/thin_$arch"
        thin=$(go run "github.com/FemLed/masseuse-camlink/cmd/machostrip@$tag" -sha256 "$appwork/thin_$arch" | cut -d' ' -f1)
        grep -q "^$thin  .* ($arch)\$" "$appwork/bundle.txt" \
          || { echo "    MISMATCH: the app's connector's $arch slice is not the published darwin/$arch binary" >&2; cat "$appwork/bundle.txt" >&2; hdiutil detach "$mount" -quiet; exit 1; }
        echo "    ok  the app's connector's $arch slice is the published darwin/$arch binary: $thin"
      done
      rm -rf "$appwork"
    fi
    # Gatekeeper's verdict, as at a double-click; the certificate's subject
    # (spctl's origin line) is left out of the output.
    codesign --verify --deep --strict "$app"
    for f in "$app" "$exe" "$app/Contents/Helpers/ffmpeg"; do
      info=$(codesign -dvv "$f" 2>&1)
      echo "$info" | grep -q "^TeamIdentifier=$APPLE_TEAM_ID\$" \
        || { echo "    $f: signed by another team" >&2; hdiutil detach "$mount" -quiet; exit 1; }
      echo "$info" | grep -q 'flags=0x10000(runtime)' \
        || { echo "    $f: not signed with the hardened runtime" >&2; hdiutil detach "$mount" -quiet; exit 1; }
    done
    # The camera and microphone entitlements, on the app's executable (the
    # process macOS holds responsible for what ffmpeg opens) and on ffmpeg:
    # under the hardened runtime, a responsible process without them is
    # refused the camera without a prompt, which is how v0.16.0 and v0.17.0
    # shipped (VERIFY.md, "The macOS app"). The dots are escaped: plutil
    # reads an unescaped one as a step down a key path.
    if at_least 0.18.0; then
      exe="$app/Contents/MacOS/$(plutil -extract CFBundleExecutable raw -o - "$app/Contents/Info.plist")"
      for f in "$exe" "$app/Contents/Helpers/ffmpeg"; do
        for key in 'com\.apple\.security\.device\.camera' 'com\.apple\.security\.device\.audio-input'; do
          [ "$(codesign -d --entitlements - --xml "$f" 2>/dev/null | plutil -extract "$key" raw -o - - 2>/dev/null)" = true ] \
            || { echo "    $f: not signed with the entitlement $key" >&2; hdiutil detach "$mount" -quiet; exit 1; }
        done
      done
      echo "    ok  the app's executable and its ffmpeg may open the camera and the microphone"
    else
      echo "    (entitlements not checked: releases before v0.18.0 signed the executable without them)"
    fi
    assess=$(spctl --assess --type execute -vv "$app" 2>&1 || true)
    echo "$assess" | grep -q 'source=Notarized Developer ID' \
      || { echo "    the app is not accepted as notarized:"; echo "$assess" | grep -v '^origin=' | sed 's/^/      /'; hdiutil detach "$mount" -quiet; exit 1; }
    xcrun stapler validate -q "$app" || { echo "    no notarization ticket stapled to the app" >&2; hdiutil detach "$mount" -quiet; exit 1; }
    hdiutil detach "$mount" -quiet
    codesign --verify --strict "$dmg"
    assess=$(spctl --assess --type open --context context:primary-signature -vv "$dmg" 2>&1 || true)
    echo "$assess" | grep -q 'source=Notarized Developer ID' \
      || { echo "    the disk image is not accepted as notarized:"; echo "$assess" | grep -v '^origin=' | sed 's/^/      /'; exit 1; }
    xcrun stapler validate -q "$dmg" || { echo "    no notarization ticket stapled to the disk image" >&2; exit 1; }
    echo "    ok  app and disk image: team $APPLE_TEAM_ID, hardened runtime, notarized, tickets stapled"
  fi
else
  rm -f checksums-darwin.txt
  echo "==> 8. the macOS app: none in this release (no checksums-darwin.txt)"
fi

# The Windows package (VERIFY.md 3, "The Windows package"): a third checksum
# file for the zip, signed and attested like the others, and the executable
# in the zip is the windows_amd64 archive's byte for byte. Releases before
# it have no such file.
if [ -s checksums-windows.txt ] || curl -fsSL -o checksums-windows.txt "$base/checksums-windows.txt" 2>/dev/null; then
  echo "==> 9. the Windows package: checksum signature, hash and provenance"
  fetch checksums-windows.txt.sigstore.json
  fetch windows.intoto.jsonl
  cosign verify-blob \
    --bundle checksums-windows.txt.sigstore.json \
    --certificate-identity-regexp "$WORKFLOW_RE" \
    --certificate-oidc-issuer "$ISSUER" \
    checksums-windows.txt
  # v0.8.1's file was written by sha256sum on Windows, in its binary-mode
  # form "<hash> *<name>": the name is taken without the marker (sha256sum -c
  # and shasum -c read either form).
  winfiles() { awk '{print $2}' checksums-windows.txt | sed 's/^\*//'; }
  winfiles | while read -r f; do fetch "$f"; done
  $SHA -c checksums-windows.txt
  winfiles | while read -r f; do
    slsa-verifier verify-artifact "$f" \
      --provenance-path windows.intoto.jsonl \
      --source-uri "github.com/$REPO" \
      --source-tag "$tag" >/dev/null
    echo "    ok  $f"
  done
  archive="masseuse-camlink_${version}_windows_amd64.zip"
  zipfile=$(winfiles | grep -E '\.zip$' | head -n 1)
  if [ -s Masseuse.exe ]; then
    # v0.13.0 and later: the one-file package. pestrip takes the signature
    # and the payload off, and writes the payload out. From the release
    # that brought the desktop window, the file is the window and the
    # published connector is in the payload as masseuse-camlink.exe (its
    # own signature stripped, it is the archive's executable); before, the
    # file itself minus signature and payload was the archive's executable.
    if [ -s "$archive" ] && command -v go >/dev/null 2>&1 && command -v unzip >/dev/null 2>&1; then
      published=$(unzip -p "$archive" masseuse-camlink.exe | $SHA | cut -d' ' -f1)
      rm -rf winpayload
      go run "github.com/FemLed/masseuse-camlink/cmd/pestrip@$tag" -payload winpayload Masseuse.exe > winpayload.txt
      grep -q '  ffmpeg.exe$' winpayload.txt || { echo "    no ffmpeg.exe in the payload" >&2; exit 1; }
      if [ -s winpayload/masseuse-camlink.exe ]; then
        inside=$(go run "github.com/FemLed/masseuse-camlink/cmd/pestrip@$tag" -sha256 winpayload/masseuse-camlink.exe | cut -d' ' -f1)
        [ "$inside" = "$published" ] \
          || { echo "    MISMATCH: the connector in Masseuse.exe's payload, minus its signature ($inside), is not the published windows/amd64 connector ($published)" >&2; exit 1; }
        echo "    ok  the connector in Masseuse.exe's payload minus its signature is the published windows/amd64 connector: $published"
        echo "    ok  Masseuse.exe itself is the desktop window: signed, covered by the checksum file and the provenance, not rebuilt byte for byte (VERIFY.md, \"The desktop window\")"
      else
        stripped=$(go run "github.com/FemLed/masseuse-camlink/cmd/pestrip@$tag" -sha256 Masseuse.exe | cut -d' ' -f1)
        [ "$stripped" = "$published" ] \
          || { echo "    MISMATCH: Masseuse.exe minus signature and payload ($stripped) is not the published windows/amd64 connector ($published)" >&2; exit 1; }
        echo "    ok  Masseuse.exe minus its signature and payload is the published windows/amd64 connector: $stripped"
      fi
      echo "    ok  the payload carries $(wc -l < winpayload.txt | tr -d ' ') files, ffmpeg.exe among them"
    else
      echo "    skipped the executable comparison (needs go and unzip, and $archive)"
    fi
    if [ -n "$zipfile" ] && [ -s "$zipfile" ] && command -v unzip >/dev/null 2>&1; then
      # The zip for installs before 0.13 holds the same file under the name
      # those releases used, Masseuse.ai.exe.
      [ "$(unzip -p "$zipfile" Masseuse.ai.exe | $SHA | cut -d' ' -f1)" = "$($SHA Masseuse.exe | cut -d' ' -f1)" ] \
        || { echo "    MISMATCH: Masseuse.ai.exe in $zipfile is not Masseuse.exe" >&2; exit 1; }
      echo "    ok  $zipfile holds the same file as Masseuse.ai.exe"
    fi
  elif [ -n "$zipfile" ] && [ -s "$zipfile" ] && [ -s "$archive" ] && command -v unzip >/dev/null 2>&1; then
    # Before v0.13.0: the zip held the archive's executable as is.
    packed=$(unzip -p "$zipfile" Masseuse.ai.exe | $SHA | cut -d' ' -f1)
    published=$(unzip -p "$archive" masseuse-camlink.exe | $SHA | cut -d' ' -f1)
    [ "$packed" = "$published" ] \
      || { echo "    MISMATCH: Masseuse.ai.exe in $zipfile is not the published windows/amd64 connector" >&2; exit 1; }
    echo "    ok  Masseuse.ai.exe is the published windows/amd64 connector: $packed"
    unzip -l "$zipfile" | grep -q ' ffmpeg.exe$' || { echo "    no ffmpeg.exe in $zipfile" >&2; exit 1; }
  else
    echo "    skipped the executable comparison (no unzip, or no $archive)"
  fi
else
  rm -f checksums-windows.txt
  echo "==> 9. the Windows package: none in this release (no checksums-windows.txt)"
fi

# Unit driver helpers (VERIFY.md 3, "Unit driver helpers"): programs the
# downloads carry that are not built from this repository; each is named,
# with its hash, in a manifest signed by masseuse.ai's helpers key, whose
# public half the tag carries. Releases before v0.10.0 have none.
raw="https://raw.githubusercontent.com/$REPO/$tag/packaging/units"
if curl -fsSL -o units-VERSION "$raw/VERSION" 2>/dev/null; then
  # One helpers release per line (a bare version, or a family's prefix and
  # its version), each with its own signed manifest; the files in the
  # archives are compared with the manifests of all of them together.
  releases=$(tr -d '\r' < units-VERSION | sed 's/#.*//' | tr -s '[:space:]' '\n' | sed '/^$/d')
  echo "==> 10. unit driver helpers $(echo "$releases" | tr '\n' ' '): manifest signatures, and the files in the archives"
  fetch_units() { curl -fsSL -o "$2" "$1" || { echo "    could not fetch $1" >&2; exit 1; }; }
  fetch_units "$raw/cosign.pub" units-cosign.pub
  i=0
  for release in $releases; do
    i=$((i + 1))
    fetch_units "https://masseuse.ai/app/units/$release/manifest.json" "units-manifest-$i.json"
    fetch_units "https://masseuse.ai/app/units/$release/manifest.json.sigstore.json" "units-bundle-$i.sigstore.json"
    cosign verify-blob --key units-cosign.pub --bundle "units-bundle-$i.sigstore.json" "units-manifest-$i.json" >/dev/null
    echo "    ok  $release/manifest.json signed by the helpers' key at $tag"
  done
  if command -v jq >/dev/null 2>&1; then
    # Every release's files in one list, for the comparisons below (the
    # bundles are named apart so the glob takes manifests alone).
    jq -s '{files: [.[].files[]]}' units-manifest-[0-9]*.json > units-manifest.json
    # windows/amd64 archive: units/<name>.exe hashes to the manifest's
    # windows/amd64 entry (the archive's helpers are unsigned; the
    # package's are signed and compared below with the signature stripped).
    archive="masseuse-camlink_${version}_windows_amd64.zip"
    if [ -s "$archive" ] && command -v unzip >/dev/null 2>&1; then
      unzip -Z1 "$archive" | grep '^units/[^/]' | while read -r f; do
        name=${f#units/}
        want=$(jq -r --arg n "$name" '.files[] | select(.name == $n and .os == "windows" and .arch == "amd64") | .sha256' units-manifest.json | tr -d '\r')
        got=$(unzip -p "$archive" "$f" | $SHA | cut -d' ' -f1)
        [ -n "$want" ] && [ "$got" = "$want" ] \
          || { echo "    MISMATCH: $f in $archive is not the manifest's ($got, manifest $want)" >&2; exit 1; }
        echo "    ok  $f in $archive is the manifest's: $got"
      done
    fi
    # The Windows package's payload (written out by step 9 with pestrip):
    # units/<name>.exe, Authenticode signature stripped, against the
    # windows/amd64 entries; a Go binary's stripped hash is its plain one.
    if [ -d winpayload/units ] && command -v go >/dev/null 2>&1; then
      for p in winpayload/units/camlink-unit-*.exe; do
        [ -f "$p" ] || continue
        name=$(basename "$p")
        want=$(jq -r --arg n "$name" '.files[] | select(.name == $n and .os == "windows" and .arch == "amd64") | .sha256' units-manifest.json | tr -d '\r')
        got=$(go run "github.com/FemLed/masseuse-camlink/cmd/pestrip@$tag" -sha256 "$p" | cut -d' ' -f1)
        [ -n "$want" ] && [ "$got" = "$want" ] \
          || { echo "    MISMATCH: units/$name in Masseuse.exe is not the manifest's ($got, manifest $want)" >&2; exit 1; }
        echo "    ok  units/$name in Masseuse.exe is the manifest's once its signature is stripped: $got"
      done
    fi
    # Before v0.13.0, the Windows zip held units/<name>.exe unsigned.
    zipfile="Masseuse.ai-${version}-windows.zip"
    if [ ! -s Masseuse.exe ] && [ -s "$zipfile" ] && command -v unzip >/dev/null 2>&1; then
      # The zip lists the directory entry units/ too; files only.
      unzip -Z1 "$zipfile" | grep '^units/[^/]' | while read -r f; do
        name=${f#units/}
        want=$(jq -r --arg n "$name" '.files[] | select(.name == $n and .os == "windows" and .arch == "amd64") | .sha256' units-manifest.json | tr -d '\r')
        got=$(unzip -p "$zipfile" "$f" | $SHA | cut -d' ' -f1)
        [ -n "$want" ] && [ "$got" = "$want" ] \
          || { echo "    MISMATCH: $f in $zipfile is not the manifest's ($got, manifest $want)" >&2; exit 1; }
        echo "    ok  $f in $zipfile is the manifest's: $got"
      done
    fi
  else
    echo "    skipped the file comparison (no jq)"
  fi
else
  echo "==> 10. unit driver helpers: none in this release (no packaging/units/VERSION at $tag)"
fi

echo "all checks passed for $tag"

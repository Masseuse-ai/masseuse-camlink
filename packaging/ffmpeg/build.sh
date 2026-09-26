#!/bin/sh
# Build the ffmpeg the macOS application bundle carries: one static,
# universal (arm64 and x86_64) ffmpeg program holding exactly the components
# the connector uses (internal/capture/args.go and devices.go) plus a test
# source for the smoke test, and nothing licensed beyond LGPL 2.1: no GPL
# parts (so no libx264; VideoToolbox with -allow_sw 1 encodes on every Mac),
# no nonfree parts. The one external library is libopus (BSD-3-Clause).
#
# Sources are the upstream release tarballs, pinned by SHA-256 below and in
# THIRD_PARTY.md. The release attaches the same tarballs and this script's
# configure flags are the build recipe, which is the LGPL's source offer.
#
# usage: sh packaging/ffmpeg/build.sh [-o OUTDIR] [-w WORKDIR] [-a ARCHS]
#   OUTDIR   where to put ffmpeg (universal), the source tarballs and the
#            licence texts (default: dist/ffmpeg)
#   WORKDIR  downloads and build trees (default: OUTDIR/work); tarballs
#            already there are verified and used, not downloaded again
#   ARCHS    comma-separated, default arm64,x86_64 (one arch: thin binary)
# needs: macOS with the Xcode command line tools (clang, lipo, vtool), make,
#        pkg-config, nasm (for the x86_64 slice), curl (unless the tarballs
#        are in WORKDIR); about 4 minutes on an Apple silicon runner.
set -eu

FFMPEG_VERSION=9.0.1
FFMPEG_SHA256=cf38e0e28c7e5605942c4a77755349b0145804a397af37eb1fb4c77cb237f635
OPUS_VERSION=1.6.1
OPUS_SHA256=6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1
# The connector binaries need macOS 13 (Go's floor); nothing lower is useful.
MACOS_MIN=13.0

outdir="dist/ffmpeg"
workdir=""
archs="arm64,x86_64"
while [ $# -gt 0 ]; do
  case "$1" in
    -o) outdir="$2"; shift 2 ;;
    -w) workdir="$2"; shift 2 ;;
    -a) archs="$2"; shift 2 ;;
    -h|--help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$workdir" ] || workdir="$outdir/work"
mkdir -p "$outdir" "$workdir"
outdir=$(cd "$outdir" && pwd)
workdir=$(cd "$workdir" && pwd)

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1 ($2)" >&2; exit 2; }; }
need clang "Xcode command line tools"
need lipo "Xcode command line tools"
need make "Xcode command line tools"
need pkg-config "brew install pkg-config"
case ",$archs," in *,x86_64,*) need nasm "brew install nasm" ;; esac
if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum"; else SHA="shasum -a 256"; fi
jobs=$(sysctl -n hw.ncpu 2>/dev/null || echo 4)

# fetch NAME URL SHA256: NAME lands in WORKDIR with that hash, or the build stops.
fetch() {
  if [ ! -s "$workdir/$1" ]; then
    need curl "to download $1"
    echo "==> downloading $1"
    curl -fsSL --retry 3 -o "$workdir/$1.part" "$2"
    mv "$workdir/$1.part" "$workdir/$1"
  fi
  got=$($SHA "$workdir/$1" | cut -d' ' -f1)
  if [ "$got" != "$3" ]; then
    echo "$1: SHA-256 $got, expected $3" >&2
    exit 1
  fi
  echo "verified $1 ($3)"
}

fetch "ffmpeg-$FFMPEG_VERSION.tar.xz" "https://ffmpeg.org/releases/ffmpeg-$FFMPEG_VERSION.tar.xz" "$FFMPEG_SHA256"
fetch "opus-$OPUS_VERSION.tar.gz" "https://downloads.xiph.org/releases/opus/opus-$OPUS_VERSION.tar.gz" "$OPUS_SHA256"

src="$workdir/src"
rm -rf "$src" && mkdir -p "$src"
tar -xJf "$workdir/ffmpeg-$FFMPEG_VERSION.tar.xz" -C "$src"
tar -xzf "$workdir/opus-$OPUS_VERSION.tar.gz" -C "$src"
ffsrc="$src/ffmpeg-$FFMPEG_VERSION"
opussrc="$src/opus-$OPUS_VERSION"

# build_arch ARCH: opus, then ffmpeg against it, into WORKDIR/out/ARCH.
build_arch() {
  arch="$1"
  prefix="$workdir/out/$arch"
  build="$workdir/build/$arch"
  rm -rf "$prefix" "$build" && mkdir -p "$prefix" "$build/opus" "$build/ffmpeg"
  case "$arch" in
    arm64) host=aarch64-apple-darwin ;;
    x86_64) host=x86_64-apple-darwin ;;
    *) echo "unknown architecture $arch" >&2; exit 2 ;;
  esac
  flags="-arch $arch -mmacosx-version-min=$MACOS_MIN"

  echo "==> opus $OPUS_VERSION for $arch"
  (cd "$build/opus" && "$opussrc/configure" --quiet --host="$host" --prefix="$prefix" \
      --disable-shared --enable-static --disable-doc --disable-extra-programs \
      CC=clang CFLAGS="$flags -O2" \
    && build_quietly "$build/opus")

  echo "==> ffmpeg $FFMPEG_VERSION for $arch"
  cross=""
  if [ "$arch" != "$(uname -m)" ]; then
    cross="--enable-cross-compile --arch=$arch --target-os=darwin --pkg-config=pkg-config --x86asmexe=nasm"
  fi
  # Components, and only these:
  #   input   avfoundation (cameras and microphones; what -list_devices reads),
  #           lavfi with testsrc2 and sine (the smoke test's picture and tone)
  #   decode  rawvideo and PCM, which is what avfoundation delivers;
  #           wrapped_avframe, which is what lavfi delivers
  #   encode  h264_videotoolbox, libopus
  #   filter  the ones ffmpeg inserts for -pix_fmt, -r, -ac/-ar and -t,
  #           and the graph's own source and sink filters
  #   output  rtsp over tcp (rtp and http come with it), null for the tests
  # shellcheck disable=SC2086
  (cd "$build/ffmpeg" && PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig" "$ffsrc/configure" \
      --prefix="$prefix" --cc=clang $cross \
      --disable-gpl --disable-nonfree \
      --disable-everything --disable-autodetect \
      --disable-programs --enable-ffmpeg \
      --disable-doc --disable-debug --disable-shared --enable-static --pkg-config-flags=--static \
      --enable-avfoundation --enable-videotoolbox --enable-libopus \
      --enable-indev=avfoundation,lavfi \
      --enable-decoder=rawvideo,wrapped_avframe,pcm_f32le,pcm_f32be,pcm_s16le,pcm_s16be,pcm_s24le,pcm_s32le,pcm_u8 \
      --enable-encoder=h264_videotoolbox,libopus \
      --enable-muxer=rtsp,rtp,null \
      --enable-protocol=tcp,udp,rtp \
      --enable-filter=buffer,buffersink,abuffer,abuffersink,format,aformat,scale,fps,aresample,null,anull,trim,atrim,testsrc2,sine \
      --extra-cflags="$flags" --extra-ldflags="$flags" \
    && build_quietly "$build/ffmpeg")
  echo "built $prefix/bin/ffmpeg"
}

# build_quietly DIR: make and install there, the output kept in DIR/make.log
# and shown only when something fails.
build_quietly() {
  if ! (cd "$1" && make -j"$jobs" && make install) > "$1/make.log" 2>&1; then
    tail -n 60 "$1/make.log" >&2
    echo "build failed in $1; the whole log is $1/make.log" >&2
    exit 1
  fi
}

thin=""
for arch in $(echo "$archs" | tr ',' ' '); do
  build_arch "$arch"
  thin="$thin $workdir/out/$arch/bin/ffmpeg"
done
# shellcheck disable=SC2086
lipo -create -output "$outdir/ffmpeg" $thin
echo "==> $outdir/ffmpeg: $(lipo -archs "$outdir/ffmpeg")"
vtool -show-build "$outdir/ffmpeg" | grep -E '^ *(minos|platform)' | sort -u

# Smoke test: every component the connector asks for must be there, and the
# connector's own encoding chain (internal/capture/args.go, everything after
# the input) must run, on a test picture and tone, through each slice that
# this computer can execute.
smoke() {
  bin="$1"
  echo "==> smoke test: $bin"
  "$bin" -hide_banner -encoders | grep -qE '^ V[^ ]* +h264_videotoolbox ' || { echo "no h264_videotoolbox encoder" >&2; exit 1; }
  "$bin" -hide_banner -encoders | grep -qE '^ A[^ ]* +libopus ' || { echo "no libopus encoder" >&2; exit 1; }
  "$bin" -hide_banner -muxers | grep -qE '^ +E +rtsp ' || { echo "no rtsp muxer" >&2; exit 1; }
  "$bin" -hide_banner -devices | grep -qE '^ +D +avfoundation ' || { echo "no avfoundation input" >&2; exit 1; }
  "$bin" -hide_banner -decoders | grep -qE '^ V[^ ]* +rawvideo ' || { echo "no rawvideo decoder" >&2; exit 1; }
  "$bin" -hide_banner -protocols | grep -qw tcp || { echo "no tcp protocol" >&2; exit 1; }
  if "$bin" -hide_banner -encoders | grep -qE 'libx264|x265|libfdk'; then
    echo "a GPL or nonfree component is present" >&2; exit 1
  fi
  "$bin" -hide_banner -loglevel error -nostats \
    -f lavfi -i testsrc2=size=1280x720:rate=30 -f lavfi -i sine=frequency=440:sample_rate=48000 -t 2 \
    -c:v h264_videotoolbox -realtime 1 -allow_sw 1 -profile:v main -level 3.1 \
    -pix_fmt yuv420p -r 30 -g 60 -force_key_frames 'expr:gte(t,n_forced*2)' \
    -b:v 2500k -maxrate 2500k -bufsize 2500k \
    -c:a libopus -ac 1 -ar 48000 -b:a 64k -application audio \
    -f null -
  echo "ok: components present, two seconds of 1280x720 encoded with h264_videotoolbox and libopus"
}
smoke "$outdir/ffmpeg"
for arch in $(echo "$archs" | tr ',' ' '); do
  [ "$arch" != "$(uname -m)" ] || continue
  if arch -"$arch" /usr/bin/true 2>/dev/null; then
    echo "==> the $arch slice, under arch(1)"
    smoke "$workdir/out/$arch/bin/ffmpeg" 2>&1 | sed "s/^/  /"
  else
    echo "==> the $arch slice cannot run on this computer; not smoke-tested here"
  fi
done

# What ships beside the binary: the licence texts from the very tarballs
# built, and the tarballs themselves for the release to attach.
mkdir -p "$outdir/licenses" "$outdir/sources"
cp "$ffsrc/COPYING.LGPLv2.1" "$outdir/licenses/ffmpeg-COPYING.LGPLv2.1"
cp "$ffsrc/LICENSE.md" "$outdir/licenses/ffmpeg-LICENSE.md"
cp "$opussrc/COPYING" "$outdir/licenses/opus-COPYING"
cp "$workdir/ffmpeg-$FFMPEG_VERSION.tar.xz" "$workdir/opus-$OPUS_VERSION.tar.gz" "$outdir/sources/"
echo "==> done: $outdir/ffmpeg, $outdir/licenses/, $outdir/sources/"

#!/bin/sh
# Build the ffmpeg that ships with the connector, one static program holding
# exactly the components the connector uses (internal/capture/args.go and
# devices.go) plus a test source for the smoke test, and nothing licensed
# beyond LGPL 2.1: no GPL parts (so no libx264: the system's H.264 encoder
# does the encoding, VideoToolbox on a Mac, Media Foundation on Windows), no
# nonfree parts. The one external library is libopus (BSD-3-Clause).
#
#   -t darwin   for Masseuse.ai.app: universal (arm64 and x86_64), built on
#               a Mac with the Xcode tools; AVFoundation input,
#               h264_videotoolbox encoder
#   -t windows  for the Masseuse.ai zip: ffmpeg.exe (x86_64), cross-compiled
#               on Linux with mingw-w64; DirectShow input, h264_mf encoder
#               (Media Foundation: the machine's hardware encoder or
#               Windows' own software one)
#
# Sources are the upstream release tarballs, pinned by SHA-256 below and in
# THIRD_PARTY.md. The release attaches the same tarballs and this script's
# configure flags are the build recipe, which is the LGPL's source offer.
#
# usage: sh packaging/ffmpeg/build.sh [-t darwin|windows] [-o OUTDIR] [-w WORKDIR] [-a ARCHS]
#   TARGET   darwin (the default on a Mac) or windows (the default elsewhere)
#   OUTDIR   where to put ffmpeg (or ffmpeg.exe), the source tarballs and
#            the licence texts (default: dist/ffmpeg)
#   WORKDIR  downloads and build trees (default: OUTDIR/work); tarballs
#            already there are verified and used, not downloaded again
#   ARCHS    darwin only: comma-separated, default arm64,x86_64 (one arch:
#            thin binary)
# needs: darwin: macOS with the Xcode command line tools (clang, lipo,
#        vtool), make, pkg-config, nasm (for the x86_64 slice); about 4
#        minutes on an Apple silicon runner. windows: x86_64-w64-mingw32-gcc
#        (Debian/Ubuntu: gcc-mingw-w64-x86-64), a native gcc for ffmpeg's
#        build tools, make, pkg-config, nasm; a few minutes on a Linux
#        runner. Both: curl unless the tarballs are in WORKDIR. The Windows
#        program cannot run where it is built: smoke.sh runs it on a Windows
#        machine (the release workflow does).
set -eu

FFMPEG_VERSION=9.0.1
FFMPEG_SHA256=cf38e0e28c7e5605942c4a77755349b0145804a397af37eb1fb4c77cb237f635
OPUS_VERSION=1.6.1
OPUS_SHA256=6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1
# The connector binaries need macOS 13 (Go's floor); nothing lower is useful.
MACOS_MIN=13.0
MINGW=x86_64-w64-mingw32

here=$(cd "$(dirname "$0")" && pwd)
target=""
outdir="dist/ffmpeg"
workdir=""
archs="arm64,x86_64"
while [ $# -gt 0 ]; do
  case "$1" in
    -t) target="$2"; shift 2 ;;
    -o) outdir="$2"; shift 2 ;;
    -w) workdir="$2"; shift 2 ;;
    -a) archs="$2"; shift 2 ;;
    -h|--help) sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
if [ -z "$target" ]; then
  if [ "$(uname -s)" = Darwin ]; then target=darwin; else target=windows; fi
fi
case "$target" in darwin|windows) ;; *) echo "-t must be darwin or windows" >&2; exit 2 ;; esac
[ -n "$workdir" ] || workdir="$outdir/work"
mkdir -p "$outdir" "$workdir"
outdir=$(cd "$outdir" && pwd)
workdir=$(cd "$workdir" && pwd)

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1 ($2)" >&2; exit 2; }; }
need make "a C toolchain"
need pkg-config "brew install pkg-config, or apt install pkg-config"
case "$target" in
  darwin)
    [ "$(uname -s)" = Darwin ] || { echo "-t darwin builds on a Mac only (clang, lipo, vtool)" >&2; exit 2; }
    need clang "Xcode command line tools"
    need lipo "Xcode command line tools"
    case ",$archs," in *,x86_64,*) need nasm "brew install nasm" ;; esac
    jobs=$(sysctl -n hw.ncpu 2>/dev/null || echo 4)
    ;;
  windows)
    need "$MINGW-gcc" "apt install gcc-mingw-w64-x86-64"
    need gcc "a native C compiler, for ffmpeg's own build tools (apt install gcc)"
    need nasm "apt install nasm"
    jobs=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
    ;;
esac
if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum"; else SHA="shasum -a 256"; fi

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

# The components every build holds, and only these (the system-specific
# input and encoder are added per target below):
#   input   lavfi with testsrc2 and sine (the smoke test's picture and tone)
#   decode  rawvideo and PCM, which is what a capture input delivers;
#           wrapped_avframe, which is what lavfi delivers
#   encode  libopus
#   filter  the ones ffmpeg inserts for -pix_fmt, -r, -ac/-ar and -t, and
#           the graph's own source and sink filters
#   output  rtsp over tcp (rtp and http come with it), null for the tests
COMMON_CONFIGURE="
  --disable-gpl --disable-nonfree
  --disable-everything --disable-autodetect
  --disable-programs --enable-ffmpeg
  --disable-doc --disable-debug --disable-shared --enable-static --pkg-config-flags=--static
  --enable-libopus
  --enable-decoder=rawvideo,wrapped_avframe,pcm_f32le,pcm_f32be,pcm_s16le,pcm_s16be,pcm_s24le,pcm_s32le,pcm_u8
  --enable-muxer=rtsp,rtp,null
  --enable-protocol=tcp,udp,rtp
  --enable-filter=buffer,buffersink,abuffer,abuffersink,format,aformat,scale,fps,aresample,null,anull,trim,atrim,testsrc2,sine
"
# darwin: avfoundation (cameras and microphones; what -list_devices reads)
# and the VideoToolbox encoder.
DARWIN_CONFIGURE="
  --enable-avfoundation --enable-videotoolbox
  --enable-indev=avfoundation,lavfi
  --enable-encoder=h264_videotoolbox,libopus
"
# windows: dshow (DirectShow cameras and microphones; what -list_devices
# reads) and the Media Foundation encoder, which ffmpeg loads at run time
# (mfplat.dll), so the program starts on an N edition too and only the
# encoder is missing there until the Media Feature Pack is installed. The
# encoder's source needs the Direct3D 11 device context type (ffmpeg 9), so
# the d3d11va hardware context is enabled with it: Windows' own headers and
# libraries, nothing external. Win32 threads are ffmpeg's own on Windows;
# --disable-autodetect leaves them off unless asked for.
WINDOWS_CONFIGURE="
  --enable-mediafoundation --enable-d3d11va --enable-w32threads
  --enable-indev=dshow,lavfi
  --enable-encoder=h264_mf,libopus
"

# build_quietly DIR: make and install there, the output kept in DIR/make.log
# and shown only when something fails.
build_quietly() {
  if ! (cd "$1" && make -j"$jobs" && make install) > "$1/make.log" 2>&1; then
    tail -n 60 "$1/make.log" >&2
    echo "build failed in $1; the whole log is $1/make.log" >&2
    exit 1
  fi
}

# build_darwin ARCH: opus, then ffmpeg against it, into WORKDIR/out/ARCH.
build_darwin() {
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

  echo "==> opus $OPUS_VERSION for darwin/$arch"
  (cd "$build/opus" && "$opussrc/configure" --quiet --host="$host" --prefix="$prefix" \
      --disable-shared --enable-static --disable-doc --disable-extra-programs \
      CC=clang CFLAGS="$flags -O2" \
    && build_quietly "$build/opus")

  echo "==> ffmpeg $FFMPEG_VERSION for darwin/$arch"
  cross=""
  if [ "$arch" != "$(uname -m)" ]; then
    cross="--enable-cross-compile --arch=$arch --target-os=darwin --pkg-config=pkg-config --x86asmexe=nasm"
  fi
  # shellcheck disable=SC2086
  (cd "$build/ffmpeg" && PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig" "$ffsrc/configure" \
      --prefix="$prefix" --cc=clang $cross \
      $COMMON_CONFIGURE $DARWIN_CONFIGURE \
      --extra-cflags="$flags" --extra-ldflags="$flags" \
    && build_quietly "$build/ffmpeg")
  echo "built $prefix/bin/ffmpeg"
}

# build_windows: opus, then ffmpeg against it, cross-compiled for x86_64
# Windows into WORKDIR/out/windows; the program is linked static so it
# depends on nothing but Windows' own libraries (no libwinpthread-1.dll).
build_windows() {
  prefix="$workdir/out/windows"
  build="$workdir/build/windows"
  rm -rf "$prefix" "$build" && mkdir -p "$prefix" "$build/opus" "$build/ffmpeg"

  echo "==> opus $OPUS_VERSION for windows/x86_64"
  (cd "$build/opus" && "$opussrc/configure" --quiet --host="$MINGW" --prefix="$prefix" \
      --disable-shared --enable-static --disable-doc --disable-extra-programs \
      CFLAGS="-O2" \
    && build_quietly "$build/opus")

  echo "==> ffmpeg $FFMPEG_VERSION for windows/x86_64"
  # shellcheck disable=SC2086
  (cd "$build/ffmpeg" && PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig" "$ffsrc/configure" \
      --prefix="$prefix" \
      --enable-cross-compile --arch=x86_64 --target-os=mingw32 --cross-prefix="$MINGW-" \
      --pkg-config=pkg-config --x86asmexe=nasm \
      $COMMON_CONFIGURE $WINDOWS_CONFIGURE \
      --extra-ldflags="-static -static-libgcc" \
    && build_quietly "$build/ffmpeg")
  echo "built $prefix/bin/ffmpeg.exe"
}

case "$target" in
  darwin)
    thin=""
    for arch in $(echo "$archs" | tr ',' ' '); do
      build_darwin "$arch"
      thin="$thin $workdir/out/$arch/bin/ffmpeg"
    done
    # shellcheck disable=SC2086
    lipo -create -output "$outdir/ffmpeg" $thin
    echo "==> $outdir/ffmpeg: $(lipo -archs "$outdir/ffmpeg")"
    vtool -show-build "$outdir/ffmpeg" | grep -E '^ *(minos|platform)' | sort -u
    # Every component the connector asks for must be there, and its own
    # encoding chain must run, through each slice this computer can execute.
    sh "$here/smoke.sh" -t darwin "$outdir/ffmpeg"
    for arch in $(echo "$archs" | tr ',' ' '); do
      [ "$arch" != "$(uname -m)" ] || continue
      if arch -"$arch" /usr/bin/true 2>/dev/null; then
        echo "==> the $arch slice, under arch(1)"
        sh "$here/smoke.sh" -t darwin "$workdir/out/$arch/bin/ffmpeg" 2>&1 | sed "s/^/  /"
      else
        echo "==> the $arch slice cannot run on this computer; not smoke-tested here"
      fi
    done
    program="$outdir/ffmpeg"
    ;;
  windows)
    build_windows
    cp "$workdir/out/windows/bin/ffmpeg.exe" "$outdir/ffmpeg.exe"
    program="$outdir/ffmpeg.exe"
    echo "==> $program: $(file -b "$program" 2>/dev/null || echo 'PE32+ (file(1) not installed)')"
    # The import table is the one check possible here: the program may
    # depend on Windows' own libraries only, never on a mingw runtime or a
    # library of its own (mfplat is loaded at run time by ffmpeg itself, so
    # it does not appear).
    if command -v "$MINGW-objdump" >/dev/null 2>&1; then
      dlls=$("$MINGW-objdump" -p "$program" | sed -n 's/^[[:space:]]*DLL Name: //p' | tr '[:upper:]' '[:lower:]' | sort -u)
      echo "$dlls" | sed 's/^/  imports /'
      for dll in $dlls; do
        case "$dll" in
          libwinpthread*|libgcc*|libstdc*|libopus*|avcodec*|avformat*|avutil*|avfilter*|avdevice*|swresample*|swscale*)
            echo "$program imports $dll: not a Windows system library; the link is not static" >&2; exit 1 ;;
        esac
      done
    fi
    echo "==> ffmpeg.exe is not run here: sh packaging/ffmpeg/smoke.sh -t windows $program, on Windows"
    ;;
esac

# What ships beside the binary: the licence texts from the very tarballs
# built, and the tarballs themselves for the release to attach.
mkdir -p "$outdir/licenses" "$outdir/sources"
cp "$ffsrc/COPYING.LGPLv2.1" "$outdir/licenses/ffmpeg-COPYING.LGPLv2.1"
cp "$ffsrc/LICENSE.md" "$outdir/licenses/ffmpeg-LICENSE.md"
cp "$opussrc/COPYING" "$outdir/licenses/opus-COPYING"
cp "$workdir/ffmpeg-$FFMPEG_VERSION.tar.xz" "$workdir/opus-$OPUS_VERSION.tar.gz" "$outdir/sources/"
echo "==> done: $program, $outdir/licenses/, $outdir/sources/"

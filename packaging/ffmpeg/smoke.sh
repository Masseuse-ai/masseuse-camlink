#!/bin/sh
# Smoke-test an ffmpeg built by build.sh: every component the connector asks
# for must be there (internal/capture/args.go and devices.go), nothing GPL
# or nonfree may be, and the connector's own encoding chain (everything
# after the input) must run on a test picture and tone. build.sh runs it
# for the Mac build; the release workflow runs it on a Windows runner for
# ffmpeg.exe, which cannot run where it is built.
#
# usage: sh packaging/ffmpeg/smoke.sh -t darwin|windows FFMPEG
#   -t darwin   expects avfoundation and h264_videotoolbox
#   -t windows  expects dshow and h264_mf (Media Foundation must be present
#               on the machine running this: Windows Server needs the
#               Server-Media-Foundation feature, an N edition the Media
#               Feature Pack)
set -eu

target="" bin=""
while [ $# -gt 0 ]; do
  case "$1" in
    -t) target="$2"; shift 2 ;;
    -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown argument: $1" >&2; exit 2 ;;
    *) bin="$1"; shift ;;
  esac
done
[ -n "$target" ] && [ -n "$bin" ] || { echo "usage: $0 -t darwin|windows FFMPEG" >&2; exit 2; }
[ -f "$bin" ] || { echo "no ffmpeg at $bin" >&2; exit 2; }
case "$target" in
  darwin)
    input=avfoundation
    encoder=h264_videotoolbox
    encoder_args="-realtime 1 -allow_sw 1 -profile:v main -level 3.1"
    ;;
  windows)
    input=dshow
    encoder=h264_mf
    encoder_args="-rate_control cbr -scenario video_conference"
    ;;
  *) echo "-t must be darwin or windows" >&2; exit 2 ;;
esac

echo "==> smoke test: $bin ($target)"
"$bin" -hide_banner -version | head -n 1
"$bin" -hide_banner -encoders | grep -qE "^ V[^ ]* +$encoder " || { echo "no $encoder encoder" >&2; exit 1; }
"$bin" -hide_banner -encoders | grep -qE '^ A[^ ]* +libopus ' || { echo "no libopus encoder" >&2; exit 1; }
"$bin" -hide_banner -muxers | grep -qE '^ +E +rtsp ' || { echo "no rtsp muxer" >&2; exit 1; }
"$bin" -hide_banner -devices | grep -qE "^ +D +$input " || { echo "no $input input" >&2; exit 1; }
"$bin" -hide_banner -decoders | grep -qE '^ V[^ ]* +rawvideo ' || { echo "no rawvideo decoder" >&2; exit 1; }
"$bin" -hide_banner -protocols | grep -qw tcp || { echo "no tcp protocol" >&2; exit 1; }
if "$bin" -hide_banner -encoders | grep -qE 'libx264|x265|libfdk'; then
  echo "a GPL or nonfree component is present" >&2; exit 1
fi
if "$bin" -hide_banner -version | grep -qE -- '--enable-(gpl|nonfree)'; then
  echo "configured with --enable-gpl or --enable-nonfree" >&2; exit 1
fi
# The connector's chain (internal/capture/args.go, after the input), two
# seconds of it, to the null output.
# shellcheck disable=SC2086
"$bin" -hide_banner -loglevel error -nostats \
  -f lavfi -i testsrc2=size=1280x720:rate=30 -f lavfi -i sine=frequency=440:sample_rate=48000 -t 2 \
  -c:v "$encoder" $encoder_args \
  -pix_fmt yuv420p -r 30 -g 60 -force_key_frames 'expr:gte(t,n_forced*2)' \
  -b:v 2500k -maxrate 2500k -bufsize 2500k \
  -c:a libopus -ac 1 -ar 48000 -b:a 64k -application audio \
  -f null -
echo "ok: components present, two seconds of 1280x720 encoded with $encoder and libopus"

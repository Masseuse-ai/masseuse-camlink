#!/bin/sh
# Smoke-test an ffmpeg built by build.sh: every component the connector asks
# for must be there (internal/capture/args.go, devices.go and formats.go),
# nothing GPL or nonfree may be, and the connector's own encoding chain
# (everything after the input) must run on a test picture and tone. build.sh
# runs it for the Mac build; the release workflow runs it on a Windows runner
# for ffmpeg.exe, which cannot run where it is built.
#
# usage: sh packaging/ffmpeg/smoke.sh -t darwin|windows FFMPEG
#   -t darwin   expects avfoundation and h264_videotoolbox
#   -t windows  expects dshow, h264_mf (Media Foundation must be present
#               on the machine running this: Windows Server needs the
#               Server-Media-Foundation feature, an N edition the Media
#               Feature Pack) and the decoders for what cameras send
#               through DirectShow compressed
set -eu

target="" bin=""
while [ $# -gt 0 ]; do
  case "$1" in
    -t) target="$2"; shift 2 ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
    # avfoundation hands over finished frames whatever the camera sends.
    decoders="rawvideo"
    ;;
  windows)
    input=dshow
    encoder=h264_mf
    encoder_args="-rate_control cbr -scenario video_conference"
    # dshow hands over what the camera sends: raw, or compressed in one of
    # these (internal/capture/formats.go chooses among them).
    decoders="rawvideo mjpeg h264 hevc v210"
    ;;
  *) echo "-t must be darwin or windows" >&2; exit 2 ;;
esac

echo "==> smoke test: $bin ($target)"
"$bin" -hide_banner -version | head -n 1
"$bin" -hide_banner -encoders | grep -qE "^ V[^ ]* +$encoder " || { echo "no $encoder encoder" >&2; exit 1; }
"$bin" -hide_banner -encoders | grep -qE '^ A[^ ]* +libopus ' || { echo "no libopus encoder" >&2; exit 1; }
"$bin" -hide_banner -muxers | grep -qE '^ +E +rtsp ' || { echo "no rtsp muxer" >&2; exit 1; }
"$bin" -hide_banner -devices | grep -qE "^ +D +$input " || { echo "no $input input" >&2; exit 1; }
for decoder in $decoders; do
  "$bin" -hide_banner -decoders | grep -qE "^ V[^ ]* +$decoder " || { echo "no $decoder decoder" >&2; exit 1; }
done
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
  -pix_fmt yuv420p -color_range tv -r 30 -g 60 -force_key_frames 'expr:gte(t,n_forced*2)' \
  -b:v 2500k -maxrate 2500k -bufsize 2500k \
  -c:a libopus -ac 1 -ar 48000 -b:a 64k -application audio \
  -f null -
echo "ok: components present, two seconds of 1280x720 encoded with $encoder and libopus"

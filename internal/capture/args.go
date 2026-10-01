package capture

import (
	"fmt"
	"regexp"
	"strconv"
)

// Encoder names accepted by Options.Encoder.
const (
	EncoderAuto         = "auto"
	EncoderVideoToolbox = "h264_videotoolbox" // macOS hardware
	EncoderMediaFound   = "h264_mf"           // Windows hardware (Media Foundation)
	EncoderX264         = "libx264"           // software, everywhere
)

// Options shapes the capture. Zero values mean the defaults noted.
type Options struct {
	// Camera and Mic select devices (see Select); empty picks the first of
	// each. Mic "none" sends video only.
	Camera string
	Mic    string
	// Fallback lets a named device that is not connected give way to the
	// first of its kind (Source.Substitutions says which), for a remembered
	// choice; a device named on the command line is an error when absent.
	Fallback bool
	// VideoSize is WxH; empty is the camera's largest picture that reaches
	// MinFPS (choose), or Defaults.VideoSize when its modes cannot be read.
	VideoSize string
	// FPS is the stream's frame rate; 0 is the most the camera's picture
	// has up to MaxFPS, or Defaults.FPS when its modes cannot be read.
	FPS int
	// Bitrate is the video bit rate ceiling, ffmpeg syntax; empty is the
	// picture's (BitrateFor).
	Bitrate string
	// Encoder is one of the Encoder constants; default auto: the system's
	// hardware encoder, falling back to libx264 when it fails to start.
	Encoder string
	// FFmpeg is the executable; empty finds it (FindFFmpeg).
	FFmpeg string

	// ceiling is set by a Source whose rate in force (Bitrate) a step down
	// lowered (Reshape): the H.264 level is chosen for it, so the parameter
	// sets the enclave learnt from the SDP stay valid. Empty is Bitrate.
	ceiling string
}

// Defaults are the values Options' zero fields take when the camera's modes
// cannot be read (Source.shape takes them from its mode otherwise); the bit
// rate is the picture's.
var Defaults = Options{VideoSize: "1280x720", FPS: 30, Encoder: EncoderAuto}

// WithDefaults fills zero fields: Defaults, and the bit rate for the
// picture (BitrateFor).
func (o Options) WithDefaults() Options {
	if o.VideoSize == "" {
		o.VideoSize = Defaults.VideoSize
	}
	if o.FPS == 0 {
		o.FPS = Defaults.FPS
	}
	if o.Bitrate == "" {
		o.Bitrate = BitrateFor(o.VideoSize, o.FPS)
	}
	if o.Encoder == "" {
		o.Encoder = Defaults.Encoder
	}
	return o
}

// bitsPerPixel is the bit rate ceiling per pixel per frame (BitrateFor).
const bitsPerPixel = 0.1

// BitrateFor is the bit rate ceiling for a picture of size (WxH) at fps: a
// tenth of a bit per pixel per frame, rounded to 100 kb/s and never below
// the Adapter's floor (BitrateFloor). 1280x720 at 30 fps is 2800k,
// 1920x1080 6200k, 3840x2160 24900k.
func BitrateFor(size string, fps int) string {
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 || fps <= 0 {
		return formatRate(BitrateFloor)
	}
	bits := int64(float64(w)*float64(h)*float64(fps)*bitsPerPixel+50_000) / 100_000 * 100_000
	return formatRate(max(bits, BitrateFloor))
}

var (
	videoSizeRe = regexp.MustCompile(`^\d{2,5}x\d{2,5}$`)
	bitrateRe   = regexp.MustCompile(`^(\d+(?:\.\d+)?)([kKmM]?)$`)
)

// Validate checks the shaping options.
func (o Options) Validate() error {
	o = o.WithDefaults()
	if !videoSizeRe.MatchString(o.VideoSize) {
		return fmt.Errorf("capture: video size %q is not WxH", o.VideoSize)
	}
	if o.FPS < 1 || o.FPS > 120 {
		return fmt.Errorf("capture: fps %d out of range", o.FPS)
	}
	if !bitrateRe.MatchString(o.Bitrate) {
		return fmt.Errorf("capture: bitrate %q is not a rate like 2500k", o.Bitrate)
	}
	switch o.Encoder {
	case EncoderAuto, EncoderVideoToolbox, EncoderMediaFound, EncoderX264:
	default:
		return fmt.Errorf("capture: encoder %q is not one of %s, %s, %s, %s", o.Encoder, EncoderAuto, EncoderVideoToolbox, EncoderMediaFound, EncoderX264)
	}
	return nil
}

// HardwareEncoder is the system's hardware H.264 encoder, or libx264 where
// there is none to pick blind.
func HardwareEncoder(goos string) string {
	switch goos {
	case "darwin":
		return EncoderVideoToolbox
	case "windows":
		return EncoderMediaFound
	default:
		return EncoderX264
	}
}

// Args builds ffmpeg's arguments: capture cam (and mic, unless nil) on goos
// as in says, encode H.264 at the options' size, rate and bit rate with
// encoder, Opus mono for audio, and publish over RTSP (TCP) to url. A
// keyframe every two seconds lets the enclave's reader join promptly.
func Args(goos string, o Options, in Input, cam Device, mic *Device, encoder, url string) []string {
	o = o.WithDefaults()
	fps := strconv.Itoa(o.FPS)
	rate := in.Rate
	if rate == "" {
		rate = fps
	}
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostats"}
	switch goos {
	case "darwin":
		// By name where the name stands alone (Device.Input), so the input
		// survives the list being renumbered between start and session.
		input := cam.input()
		if mic != nil {
			input += ":" + mic.input()
		}
		args = append(args, "-f", "avfoundation", "-framerate", rate, "-video_size", o.VideoSize)
		args = append(args, in.Format...)
		args = append(args, "-thread_queue_size", "512", "-i", input)
	case "windows":
		input := "video=" + cam.ID
		if mic != nil {
			input += ":audio=" + mic.ID
		}
		args = append(args, "-f", "dshow", "-rtbufsize", "100M", "-framerate", rate, "-video_size", o.VideoSize)
		args = append(args, in.Format...)
		args = append(args, "-thread_queue_size", "512", "-i", input)
	default:
		args = append(args, "-f", "v4l2", "-framerate", rate, "-video_size", o.VideoSize,
			"-thread_queue_size", "512", "-i", cam.ID)
		if mic != nil {
			args = append(args, "-f", "pulse", "-thread_queue_size", "512", "-i", mic.ID,
				"-map", "0:v:0", "-map", "1:a:0")
		}
	}
	ceiling := o.ceiling
	if ceiling == "" {
		ceiling = o.Bitrate
	}
	args = append(args, encoderArgs(encoder, h264Level(o.VideoSize, o.FPS, ceiling))...)
	gop := strconv.Itoa(2 * o.FPS)
	// The picture goes out in limited range, the range the enclave reads it
	// in: MJPEG, what most webcams send, decodes to full range, which the
	// scaler ffmpeg inserts for -pix_fmt then converts rather than passing
	// on under a limited-range label (its shadows and highlights clipped).
	// The rate-control buffer is one second of the bit rate: what a bit
	// rate change (Reshape) may swing by before the next keyframe, and
	// small enough that a low rung is really low.
	args = append(args, "-pix_fmt", "yuv420p", "-color_range", "tv", "-r", fps, "-g", gop, "-force_key_frames", "expr:gte(t,n_forced*2)",
		"-b:v", o.Bitrate, "-maxrate", o.Bitrate, "-bufsize", o.Bitrate)
	if mic != nil {
		args = append(args, "-c:a", "libopus", "-ac", "1", "-ar", "48000", "-b:a", "64k", "-application", "audio")
	} else {
		args = append(args, "-an")
	}
	// RTP packets stay under what the stream's RTSPS server can forward
	// (1472 bytes less the SRTP overhead); ffmpeg's default is exactly 1472.
	return append(args, "-f", "rtsp", "-rtsp_transport", "tcp", "-pkt_size", "1200", url)
}

// encoderArgs selects the encoder. Profile and level are pinned where the
// encoder lets them be, so the parameter sets the enclave learnt from the
// SDP stay valid when the bit rate changes underneath (Reshape): left to
// itself an encoder picks the level from the bit rate.
func encoderArgs(encoder, level string) []string {
	switch encoder {
	case EncoderVideoToolbox:
		return []string{"-c:v", EncoderVideoToolbox, "-realtime", "1", "-allow_sw", "1", "-profile:v", "main", "-level", level}
	case EncoderMediaFound:
		return []string{"-c:v", EncoderMediaFound, "-rate_control", "cbr", "-scenario", "video_conference"}
	default:
		return []string{"-c:v", EncoderX264, "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "main", "-level", level}
	}
}

// h264Level is the lowest H.264 level from 3.1 up that fits the picture
// size and frame rate (macroblocks per frame and per second) and the bit
// rate ceiling (the level's most for Main profile), so a high ceiling is
// not held to a lower level's rate. It is the ceiling's, never the rate a
// step down left in force (Options.ceiling): the level stays put under
// Reshape.
func h264Level(videoSize string, fps int, ceiling string) string {
	var w, h int
	if _, err := fmt.Sscanf(videoSize, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return "3.1"
	}
	bits, _ := parseRate(ceiling)
	mbs := ((w + 15) / 16) * ((h + 15) / 16)
	mbps := mbs * fps
	for _, l := range h264Levels {
		if mbs <= l.maxFrameMBs && mbps <= l.maxMBPerSec && bits <= l.maxBitrate {
			return l.name
		}
	}
	return h264Levels[len(h264Levels)-1].name
}

// h264Levels are the levels' limits (H.264 Annex A, table A-1), from 3.1:
// frame size and macroblock rate, and the bit rate in bits per second for
// Main profile (MaxBR x 1000).
var h264Levels = []struct {
	name        string
	maxFrameMBs int
	maxMBPerSec int
	maxBitrate  int64
}{
	{"3.1", 3600, 108000, 14_000_000},
	{"3.2", 5120, 216000, 20_000_000},
	{"4.0", 8192, 245760, 20_000_000},
	{"4.1", 8192, 245760, 50_000_000},
	{"4.2", 8704, 522240, 50_000_000},
	{"5.0", 22080, 589824, 135_000_000},
	{"5.1", 36864, 983040, 240_000_000},
	{"5.2", 36864, 2073600, 240_000_000},
}

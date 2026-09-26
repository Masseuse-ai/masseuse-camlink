package capture

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	// VideoSize is WxH; default 1280x720.
	VideoSize string
	// FPS is the capture rate; default 30.
	FPS int
	// Bitrate is the video bit rate, ffmpeg syntax; default 2500k.
	Bitrate string
	// Encoder is one of the Encoder constants; default auto: the system's
	// hardware encoder, falling back to libx264 when it fails to start.
	Encoder string
	// FFmpeg is the executable; empty finds it (FindFFmpeg).
	FFmpeg string
}

// Defaults are the values Options' zero fields take.
var Defaults = Options{VideoSize: "1280x720", FPS: 30, Bitrate: "2500k", Encoder: EncoderAuto}

// WithDefaults fills zero fields.
func (o Options) WithDefaults() Options {
	if o.VideoSize == "" {
		o.VideoSize = Defaults.VideoSize
	}
	if o.FPS == 0 {
		o.FPS = Defaults.FPS
	}
	if o.Bitrate == "" {
		o.Bitrate = Defaults.Bitrate
	}
	if o.Encoder == "" {
		o.Encoder = Defaults.Encoder
	}
	return o
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

// Args builds ffmpeg's arguments: capture cam (and mic, unless nil) on goos,
// encode H.264 at the options' size, rate and bit rate with encoder, Opus
// mono for audio, and publish over RTSP (TCP) to url. A keyframe every two
// seconds lets the enclave's reader join promptly.
func Args(goos string, o Options, cam Device, mic *Device, encoder, url string) []string {
	o = o.WithDefaults()
	fps := strconv.Itoa(o.FPS)
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostats"}
	switch goos {
	case "darwin":
		input := cam.ID
		if mic != nil {
			input += ":" + mic.ID
		}
		args = append(args, "-f", "avfoundation", "-framerate", fps, "-video_size", o.VideoSize,
			"-thread_queue_size", "512", "-i", input)
	case "windows":
		input := "video=" + cam.ID
		if mic != nil {
			input += ":audio=" + mic.ID
		}
		args = append(args, "-f", "dshow", "-rtbufsize", "100M", "-framerate", fps, "-video_size", o.VideoSize,
			"-thread_queue_size", "512", "-i", input)
	default:
		args = append(args, "-f", "v4l2", "-framerate", fps, "-video_size", o.VideoSize,
			"-thread_queue_size", "512", "-i", cam.ID)
		if mic != nil {
			args = append(args, "-f", "pulse", "-thread_queue_size", "512", "-i", mic.ID,
				"-map", "0:v:0", "-map", "1:a:0")
		}
	}
	args = append(args, encoderArgs(encoder)...)
	gop := strconv.Itoa(2 * o.FPS)
	args = append(args, "-pix_fmt", "yuv420p", "-r", fps, "-g", gop, "-force_key_frames", "expr:gte(t,n_forced*2)",
		"-b:v", o.Bitrate, "-maxrate", o.Bitrate)
	if buf := scaleRate(o.Bitrate, 2); buf != "" {
		args = append(args, "-bufsize", buf)
	}
	if mic != nil {
		args = append(args, "-c:a", "libopus", "-ac", "1", "-ar", "48000", "-b:a", "64k", "-application", "audio")
	} else {
		args = append(args, "-an")
	}
	// RTP packets stay under what the stream's RTSPS server can forward
	// (1472 bytes less the SRTP overhead); ffmpeg's default is exactly 1472.
	return append(args, "-f", "rtsp", "-rtsp_transport", "tcp", "-pkt_size", "1200", url)
}

func encoderArgs(encoder string) []string {
	switch encoder {
	case EncoderVideoToolbox:
		return []string{"-c:v", EncoderVideoToolbox, "-realtime", "1", "-allow_sw", "1", "-profile:v", "main"}
	case EncoderMediaFound:
		return []string{"-c:v", EncoderMediaFound, "-rate_control", "cbr", "-scenario", "video_conference"}
	default:
		return []string{"-c:v", EncoderX264, "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "main"}
	}
}

// scaleRate multiplies an ffmpeg rate ("2500k", "2.5M") by n, or returns ""
// when it cannot read it.
func scaleRate(rate string, n float64) string {
	m := bitrateRe.FindStringSubmatch(rate)
	if m == nil {
		return ""
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return ""
	}
	v *= n
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10) + m[2]
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 3, 64), "0"), ".") + m[2]
}

package capture

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxFPS is the most frames per second the connector sends, and MinFPS the
// rate a camera's larger picture must reach to be chosen over a smaller one
// that runs faster (choose): a still-photo mode at a few frames a second is
// never preferred to a moving picture.
const (
	MaxFPS = 30
	MinFPS = 24
)

// maxFrameMBs is the largest picture H.264 carries (level 5.2: 36864
// macroblocks, 4096x2304); a larger mode is not chosen.
const maxFrameMBs = 36864

// A Mode is one way a camera can deliver its picture, as ffmpeg lists it:
// a format, a size range and a frame rate range. Most cameras list fixed
// sizes, the range's two ends the same.
type Mode struct {
	// Raw is an uncompressed mode: rawvideo decodes it (dshow), or the
	// system hands ffmpeg finished frames (avfoundation, every mode).
	Raw bool
	// Pixel is a raw dshow mode's pixel format, as ffmpeg names it
	// ("yuyv422").
	Pixel string
	// Codec is a compressed mode's decoder in this ffmpeg ("mjpeg"); ""
	// when this ffmpeg has none for it, and Tag then says what it is.
	Codec string
	// Tag is the compression tag (FourCC) of a compressed mode this ffmpeg
	// cannot decode.
	Tag                    uint32
	MinW, MinH, MaxW, MaxH int
	MinFPS, MaxFPS         float64
	// top is the mode's top frame rate as avfoundation lists it, which is
	// what it must be asked for, to the hundredth.
	top string
}

// decodable reports whether this ffmpeg can turn the mode into frames.
func (m Mode) decodable() bool { return m.Raw || m.Codec != "" }

// Format names the mode's format: the pixel format, the decoder, or the
// compression tag ("H264") when this ffmpeg has no decoder for it.
func (m Mode) Format() string {
	switch {
	case m.Pixel != "":
		return m.Pixel
	case m.Codec != "":
		return m.Codec
	case m.Tag != 0:
		return fourCC(m.Tag)
	default:
		return "raw"
	}
}

func (m Mode) String() string {
	size := fmt.Sprintf("%dx%d", m.MaxW, m.MaxH)
	if m.MinW != m.MaxW || m.MinH != m.MaxH {
		size = fmt.Sprintf("%dx%d-%s", m.MinW, m.MinH, size)
	}
	fps := formatFPS(m.MaxFPS)
	if lo := formatFPS(m.MinFPS); lo != fps {
		fps = lo + "-" + fps
	}
	return m.Format() + " " + size + " " + fps + " fps"
}

func formatFPS(f float64) string { return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64) }

// fourCC writes a compression tag as its four characters, little-endian as
// Windows stores them (0x47504A4D is "MJPG").
func fourCC(tag uint32) string {
	b := []byte{byte(tag), byte(tag >> 8), byte(tag >> 16), byte(tag >> 24)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("0x%08X", tag)
		}
	}
	return string(b)
}

var (
	// dshowMode is one entry of `ffmpeg -f dshow -list_options true`: dshow
	// names a compressed format by its decoder only when this ffmpeg has
	// one, and by its tag otherwise.
	dshowMode = regexp.MustCompile(`(?:pixel_format=(\S+)|vcodec=(\S+)|unknown compression type 0x([0-9A-Fa-f]+))\s+min s=(\d+)x(\d+) fps=(\S+) max s=(\d+)x(\d+) fps=(\S+)`)
	// avfMode is one of the modes avfoundation lists when asked for one
	// the camera does not have: a size and a frame rate range.
	avfMode = regexp.MustCompile(`(\d+)x(\d+)@\[([0-9.]+) ([0-9.]+)\]fps`)
)

// parseDShowModes reads `ffmpeg -f dshow -list_options true -i video=...`.
// A camera that lists each mode twice (with and without color
// information) gives two entries; the choice is the same either way.
func parseDShowModes(out string) []Mode {
	var modes []Mode
	for _, line := range strings.Split(out, "\n") {
		m := dshowMode.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var mode Mode
		switch {
		case m[1] != "":
			mode.Raw, mode.Pixel = true, m[1]
		case m[2] != "":
			mode.Codec = m[2]
		default:
			tag, err := strconv.ParseUint(m[3], 16, 32)
			if err != nil {
				continue
			}
			mode.Tag = uint32(tag)
		}
		var err error
		mode.MinW, _ = strconv.Atoi(m[4])
		mode.MinH, _ = strconv.Atoi(m[5])
		mode.MaxW, _ = strconv.Atoi(m[7])
		mode.MaxH, _ = strconv.Atoi(m[8])
		if mode.MinFPS, err = strconv.ParseFloat(m[6], 64); err != nil {
			continue
		}
		if mode.MaxFPS, err = strconv.ParseFloat(m[9], 64); err != nil {
			continue
		}
		modes = append(modes, mode)
	}
	return modes
}

// parseAVFoundationModes reads the modes avfoundation lists after
// "Supported modes:". Every one is raw: the system decodes what the
// camera sends.
func parseAVFoundationModes(out string) []Mode {
	var modes []Mode
	for _, line := range strings.Split(out, "\n") {
		m := avfMode.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		w, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		lo, err1 := strconv.ParseFloat(m[3], 64)
		hi, err2 := strconv.ParseFloat(m[4], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		modes = append(modes, Mode{Raw: true, MinW: w, MinH: h, MaxW: w, MaxH: h, MinFPS: lo, MaxFPS: hi, top: m[4]})
	}
	return modes
}

// describeModes is the modes for a log line, each once.
func describeModes(modes []Mode) string {
	seen := map[string]bool{}
	var out []string
	for _, m := range modes {
		s := m.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

// cameraModes lists the modes of cam. On Windows they are dshow's options
// for it. On a Mac they are the modes avfoundation lists when asked for one
// the camera does not have (a 1x1 picture), the one way it lists them; the
// camera is not started for that. Elsewhere, and when the listing says
// nothing, there are none, and the camera is opened as configured.
func cameraModes(ctx context.Context, goos, ffmpeg string, cam Device) ([]Mode, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch goos {
	case "windows":
		out, err := run(ctx, ffmpeg, "-hide_banner", "-f", "dshow", "-list_options", "true", "-i", "video="+cam.ID)
		if err != nil {
			return nil, err
		}
		return parseDShowModes(out), nil
	case "darwin":
		out, err := run(ctx, ffmpeg, "-hide_banner", "-f", "avfoundation", "-video_size", "1x1", "-framerate", "1", "-i", cam.input())
		if err != nil {
			return nil, err
		}
		return parseAVFoundationModes(out), nil
	default:
		return nil, nil
	}
}

// An Input is how ffmpeg opens the camera, beyond its size.
type Input struct {
	// Rate is what -framerate asks of the camera; "" means the stream's
	// rate (Options.FPS). It differs where the camera must be asked for
	// its own rate: avfoundation opens a mode only at its top rate (to the
	// hundredth), dshow a mode whose rate is not whole (29.97) only at that
	// rate (dshowRate), and a mode that runs only faster than MaxFPS is
	// opened at its rate and thinned to the stream's.
	Rate string
	// Format pins the mode's format: -pixel_format for a raw dshow mode,
	// -vcodec for a compressed one. Unpinned, dshow takes the first mode of
	// the size and rate whatever its format, compressed into one this
	// ffmpeg may not decode, or raw in one dshow cannot open (P010).
	Format []string
}

// A Choice is the mode chosen for a camera and what follows from it: the
// picture the stream carries and how the camera is opened.
type Choice struct {
	Mode          Mode
	Width, Height int
	FPS           int
	Input         Input
}

// Size is the picture as Options.VideoSize writes it.
func (c Choice) Size() string { return fmt.Sprintf("%dx%d", c.Width, c.Height) }

func (c Choice) String() string {
	s := fmt.Sprintf("%s %s at %d fps", c.Mode.Format(), c.Size(), c.FPS)
	if r, err := strconv.ParseFloat(c.Input.Rate, 64); err == nil && math.Abs(r-float64(c.FPS)) >= 0.5 {
		s += " (the camera at " + formatFPS(r) + ")"
	}
	return s
}

// choose picks the mode the camera is opened in: the largest picture that
// reaches MinFPS, at the most frames per second it has up to MaxFPS; when
// no mode reaches MinFPS, the fastest, and the largest of those. Between
// modes of the same picture and rate the format decides: raw (nothing to
// decode), then MJPEG (every frame its own), H.264, HEVC, anything else
// this ffmpeg decodes. A mode this ffmpeg cannot decode, or larger than
// H.264 carries, is never chosen. A configured size or rate (conf) is kept
// and the mode chosen among those that have it. It reports false when no
// mode will do.
func choose(goos string, modes []Mode, conf Options) (Choice, bool) {
	var wantW, wantH int
	if conf.VideoSize != "" {
		if _, err := fmt.Sscanf(conf.VideoSize, "%dx%d", &wantW, &wantH); err != nil {
			wantW, wantH = 0, 0
		}
	}
	type candidate struct {
		c           Choice
		area, order int
	}
	var cands []candidate
	for i, m := range modes {
		if !m.decodable() {
			continue
		}
		w, h := m.MaxW, m.MaxH
		if wantW > 0 {
			if wantW < m.MinW || wantW > m.MaxW || wantH < m.MinH || wantH > m.MaxH {
				continue
			}
			w, h = wantW, wantH
		}
		if w <= 0 || h <= 0 || ((w+15)/16)*((h+15)/16) > maxFrameMBs {
			continue
		}
		fps, rate, ok := modeRates(goos, m, conf.FPS)
		if !ok {
			continue
		}
		c := Choice{Mode: m, Width: w, Height: h, FPS: fps, Input: Input{Rate: rate, Format: pinFormat(goos, m)}}
		cands = append(cands, candidate{c: c, area: w * h, order: i})
	}
	if len(cands) == 0 {
		return Choice{}, false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		aMoving, bMoving := a.c.FPS >= MinFPS, b.c.FPS >= MinFPS
		if aMoving != bMoving {
			return aMoving
		}
		if aMoving {
			if a.area != b.area {
				return a.area > b.area
			}
			if a.c.FPS != b.c.FPS {
				return a.c.FPS > b.c.FPS
			}
		} else {
			if a.c.FPS != b.c.FPS {
				return a.c.FPS > b.c.FPS
			}
			if a.area != b.area {
				return a.area > b.area
			}
		}
		if ra, rb := formatRank(a.c.Mode), formatRank(b.c.Mode); ra != rb {
			return ra < rb
		}
		return a.order < b.order
	})
	return cands[0].c, true
}

// fpsSlack is how close to a whole rate a camera's listed rate counts as
// it (29.97 is 30, 30.000030 is 30).
const fpsSlack = 0.05

// modeRates is the stream's frame rate in mode m (want when configured,
// else the mode's top rate up to MaxFPS) and the rate the camera is opened
// at, which dshow takes anywhere in the mode's range and avfoundation only
// at its top. It reports false when the mode cannot give the stream that
// rate.
func modeRates(goos string, m Mode, want int) (fps int, rate string, ok bool) {
	if want > 0 {
		if m.MaxFPS+fpsSlack < float64(want) {
			return 0, "", false
		}
		fps = want
	} else {
		fps = min(MaxFPS, int(math.Floor(m.MaxFPS+fpsSlack)))
	}
	if fps < 1 {
		return 0, "", false
	}
	switch goos {
	case "darwin":
		top := m.top
		if top == "" {
			top = strconv.FormatFloat(m.MaxFPS, 'f', -1, 64)
		}
		return fps, top, true
	case "windows":
		switch {
		case float64(fps) > m.MaxFPS:
			return fps, dshowRate(m.MaxFPS), true
		case float64(fps) < m.MinFPS:
			return fps, dshowRate(m.MinFPS), true
		default:
			return fps, strconv.Itoa(fps), true
		}
	default:
		return fps, "", true
	}
}

// dshowRate is the -framerate that opens a dshow mode at the rate it lists
// (to six digits, as fps): dshow turns a rate into a frame interval in
// 100 ns units by truncating, and a mode that runs only at 29.97 matches
// that one interval, which 29.97 itself can miss by a unit. Half a unit
// over the mode's interval truncates back onto it.
func dshowRate(fps float64) string {
	interval := math.Round(1e7 / fps)
	return strconv.FormatFloat(1e7/(interval+0.5), 'f', 6, 64)
}

// pinFormat is the input options that make dshow open mode m in m's own
// format; nil where the format is not chosen (avfoundation picks the
// pixels itself).
func pinFormat(goos string, m Mode) []string {
	if goos != "windows" {
		return nil
	}
	switch {
	case m.Pixel != "":
		return []string{"-pixel_format", m.Pixel}
	case m.Codec != "":
		return []string{"-vcodec", m.Codec}
	default:
		return nil
	}
}

// formatRank orders formats from the cheapest to decode.
func formatRank(m Mode) int {
	if m.Raw {
		return 0
	}
	switch m.Codec {
	case "mjpeg":
		return 1
	case "h264":
		return 2
	case "hevc":
		return 3
	default:
		return 4
	}
}

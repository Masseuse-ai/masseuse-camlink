package capture

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// instaLinkListing is what v0.33.0's ffmpeg listed for an Insta360 Link on
// Windows (2026-09-30): MJPEG and H.264 only, every mode twice (without
// and with color information), each as an unknown compression type for
// want of a decoder.
const instaLinkListing = `[in#0 @ 0000019a824f47c0] DirectShow video device options (from video devices)
[in#0 @ 0000019a824f47c0]  Pin "Capture" (alternative pin name "0")
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=1920x1080 fps=24 max s=1920x1080 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=1920x1080 fps=24 max s=1920x1080 fps=30 (pc, bt470bg/bt709/unknown, center)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=1920x1440 fps=24 max s=1920x1440 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=1920x1440 fps=24 max s=1920x1440 fps=30 (pc, bt470bg/bt709/unknown, center)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=1280x720 fps=24 max s=1280x720 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=1280x720 fps=24 max s=1280x720 fps=30 (pc, bt470bg/bt709/unknown, center)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=1280x960 fps=24 max s=1280x960 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=1280x960 fps=24 max s=1280x960 fps=30 (pc, bt470bg/bt709/unknown, center)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=3840x2160 fps=24 max s=3840x2160 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x47504A4D  min s=3840x2160 fps=24 max s=3840x2160 fps=30 (pc, bt470bg/bt709/unknown, center)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=1920x1440 fps=24 max s=1920x1440 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=1920x1440 fps=24 max s=1920x1440 fps=30 (tv, bt470bg/bt709/unknown, topleft)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=3840x2160 fps=24 max s=3840x2160 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=3840x2160 fps=24 max s=3840x2160 fps=30 (tv, bt470bg/bt709/unknown, topleft)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=1920x1080 fps=24 max s=1920x1080 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=1920x1080 fps=24 max s=1920x1080 fps=30 (tv, bt470bg/bt709/unknown, topleft)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=1280x720 fps=24 max s=1280x720 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=1280x720 fps=24 max s=1280x720 fps=30 (tv, bt470bg/bt709/unknown, topleft)
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=1280x960 fps=24 max s=1280x960 fps=30
[in#0 @ 0000019a824f47c0]   unknown compression type 0x34363248  min s=1280x960 fps=24 max s=1280x960 fps=30 (tv, bt470bg/bt709/unknown, topleft)
Error opening input file video=Insta360 Link.
`

// withDecoders is a listing as an ffmpeg with the mjpeg and h264 decoders
// prints it: dshow names a format by its decoder when there is one.
func withDecoders(listing string) string {
	return strings.NewReplacer("unknown compression type 0x47504A4D", "vcodec=mjpeg",
		"unknown compression type 0x34363248", "vcodec=h264").Replace(listing)
}

// optionsListing builds a -list_options listing from entries as dshow
// prints them.
func optionsListing(entries ...string) string {
	var b strings.Builder
	b.WriteString("[dshow @ 000001] DirectShow video device options (from video devices)\n")
	b.WriteString("[dshow @ 000001]  Pin \"Capture\" (alternative pin name \"0\")\n")
	for _, e := range entries {
		b.WriteString("[dshow @ 000001]   " + e + "\n")
	}
	return b.String()
}

func TestParseDShowModes(t *testing.T) {
	modes := parseDShowModes(instaLinkListing)
	if len(modes) != 20 {
		t.Fatalf("%d modes", len(modes))
	}
	want := Mode{Tag: 0x47504A4D, MinW: 3840, MinH: 2160, MaxW: 3840, MaxH: 2160, MinFPS: 24, MaxFPS: 30}
	if !reflect.DeepEqual(modes[8], want) {
		t.Fatalf("mode 8: %+v", modes[8])
	}
	for _, m := range modes {
		if m.decodable() {
			t.Fatalf("%v decodable without a decoder", m)
		}
	}
	if got := describeModes(modes[:2]); got != "MJPG 1920x1080 24-30 fps" {
		t.Fatalf("describe %q", got)
	}
	decoded := parseDShowModes(withDecoders(instaLinkListing))
	if decoded[0].Codec != "mjpeg" || decoded[10].Codec != "h264" || !decoded[0].decodable() {
		t.Fatalf("decoded %+v %+v", decoded[0], decoded[10])
	}
	raw := parseDShowModes(optionsListing("pixel_format=yuyv422  min s=640x480 fps=5 max s=1280x720 fps=7.5"))
	if len(raw) != 1 || !raw[0].Raw || raw[0].Pixel != "yuyv422" || raw[0].MinW != 640 || raw[0].MaxW != 1280 || raw[0].MaxFPS != 7.5 {
		t.Fatalf("raw %+v", raw)
	}
	if s := raw[0].String(); s != "yuyv422 640x480-1280x720 5-7.5 fps" {
		t.Fatalf("raw described %q", s)
	}
	if got := fourCC(0x34363248) + " " + fourCC(0x30313050) + " " + fourCC(3); got != "H264 P010 0x00000003" {
		t.Fatalf("fourCC %q", got)
	}
}

// avfModesListing is what avfoundation lists after a mode the camera does
// not have: a size and a frame rate range per format.
const avfModesListing = `[AVFoundation indev @ 0x7f8] Selected video size (1x1) is not supported by the device.
[AVFoundation indev @ 0x7f8] Supported modes:
[AVFoundation indev @ 0x7f8]   1920x1080@[1.000000 30.000030]fps
[AVFoundation indev @ 0x7f8]   1280x720@[1.000000 30.000030]fps
[AVFoundation indev @ 0x7f8]   3840x2160@[1.000000 30.000030]fps
[AVFoundation indev @ 0x7f8]   3840x2160@[1.000000 60.000240]fps
[AVFoundation indev @ 0x7f8]   640x480@[1.000000 30.000030]fps
[in#0 @ 0x7f9] Error opening input: Invalid argument
`

func TestParseAVFoundationModes(t *testing.T) {
	modes := parseAVFoundationModes(avfModesListing)
	if len(modes) != 5 {
		t.Fatalf("%d modes", len(modes))
	}
	want := Mode{Raw: true, MinW: 3840, MinH: 2160, MaxW: 3840, MaxH: 2160, MinFPS: 1, MaxFPS: 60.00024, top: "60.000240"}
	if !reflect.DeepEqual(modes[3], want) {
		t.Fatalf("mode 3: %+v", modes[3])
	}
}

func TestChooseMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		goos    string
		listing string
		conf    Options
		want    string // Choice.String, "" for none
		input   Input
	}{
		{
			name: "Insta360 Link: the largest picture, as MJPEG", goos: "windows",
			listing: withDecoders(instaLinkListing),
			want:    "mjpeg 3840x2160 at 30 fps", input: Input{Rate: "30", Format: []string{"-vcodec", "mjpeg"}},
		},
		{
			name: "Insta360 Link through an ffmpeg with no decoder for it", goos: "windows",
			listing: instaLinkListing,
		},
		{
			name: "a USB 2.0 webcam: its raw still sizes run at a few fps", goos: "windows",
			listing: optionsListing(
				"pixel_format=yuyv422  min s=640x480 fps=5 max s=640x480 fps=30",
				"pixel_format=yuyv422  min s=1280x720 fps=5 max s=1280x720 fps=10",
				"pixel_format=yuyv422  min s=2304x1536 fps=2 max s=2304x1536 fps=2",
				"vcodec=mjpeg  min s=1280x720 fps=5 max s=1280x720 fps=30",
				"vcodec=mjpeg  min s=1920x1080 fps=5 max s=1920x1080 fps=30",
			),
			want: "mjpeg 1920x1080 at 30 fps", input: Input{Rate: "30", Format: []string{"-vcodec", "mjpeg"}},
		},
		{
			name: "H.264 listed first: MJPEG all the same", goos: "windows",
			listing: optionsListing(
				"vcodec=h264  min s=1920x1080 fps=30 max s=1920x1080 fps=30",
				"vcodec=mjpeg  min s=1920x1080 fps=30 max s=1920x1080 fps=30",
			),
			want: "mjpeg 1920x1080 at 30 fps", input: Input{Rate: "30", Format: []string{"-vcodec", "mjpeg"}},
		},
		{
			name: "raw beats MJPEG at the same picture and rate", goos: "windows",
			listing: optionsListing(
				"vcodec=mjpeg  min s=1920x1080 fps=30 max s=1920x1080 fps=30",
				"pixel_format=nv12  min s=1920x1080 fps=30 max s=1920x1080 fps=30",
			),
			want: "nv12 1920x1080 at 30 fps", input: Input{Rate: "30", Format: []string{"-pixel_format", "nv12"}},
		},
		{
			name: "a capture card listing P010 first", goos: "windows",
			listing: optionsListing(
				"unknown compression type 0x30313050  min s=3840x2160 fps=30 max s=3840x2160 fps=30",
				"pixel_format=nv12  min s=3840x2160 fps=30 max s=3840x2160 fps=30",
			),
			want: "nv12 3840x2160 at 30 fps", input: Input{Rate: "30", Format: []string{"-pixel_format", "nv12"}},
		},
		{
			name: "H.264 only", goos: "windows",
			listing: optionsListing("vcodec=h264  min s=1920x1080 fps=15 max s=1920x1080 fps=30"),
			want:    "h264 1920x1080 at 30 fps", input: Input{Rate: "30", Format: []string{"-vcodec", "h264"}},
		},
		{
			name: "4K only at 15 fps: the 1080p that reaches 30 wins", goos: "windows",
			listing: optionsListing(
				"vcodec=mjpeg  min s=3840x2160 fps=5 max s=3840x2160 fps=15",
				"vcodec=mjpeg  min s=1920x1080 fps=5 max s=1920x1080 fps=30",
			),
			want: "mjpeg 1920x1080 at 30 fps", input: Input{Rate: "30", Format: []string{"-vcodec", "mjpeg"}},
		},
		{
			name: "4K at 24 fps beats 1080p at 30", goos: "windows",
			listing: optionsListing(
				"vcodec=mjpeg  min s=1920x1080 fps=5 max s=1920x1080 fps=30",
				"vcodec=mjpeg  min s=3840x2160 fps=5 max s=3840x2160 fps=24",
			),
			want: "mjpeg 3840x2160 at 24 fps", input: Input{Rate: "24", Format: []string{"-vcodec", "mjpeg"}},
		},
		{
			name: "nothing reaches 24 fps: the fastest, then the largest", goos: "windows",
			listing: optionsListing(
				"vcodec=mjpeg  min s=3840x2160 fps=5 max s=3840x2160 fps=15",
				"vcodec=mjpeg  min s=1920x1080 fps=5 max s=1920x1080 fps=20",
				"vcodec=mjpeg  min s=1280x720 fps=5 max s=1280x720 fps=20",
			),
			want: "mjpeg 1920x1080 at 20 fps", input: Input{Rate: "20", Format: []string{"-vcodec", "mjpeg"}},
		},
		{
			// dshow's interval for 29.97 is 333667; the rate asked is half a
			// unit over it, which truncates back onto it.
			name: "a mode that runs only at 29.97 streams at 30", goos: "windows",
			listing: optionsListing("pixel_format=yuyv422  min s=1920x1080 fps=29.97 max s=1920x1080 fps=29.97"),
			want:    "yuyv422 1920x1080 at 30 fps", input: Input{Rate: "29.969955", Format: []string{"-pixel_format", "yuyv422"}},
		},
		{
			name: "a capture card that only runs at 60: opened at 60, thinned to 30", goos: "windows",
			listing: optionsListing("pixel_format=nv12  min s=1920x1080 fps=60.0002 max s=1920x1080 fps=60.0002"),
			want:    "nv12 1920x1080 at 30 fps (the camera at 60)", input: Input{Rate: "60.000060", Format: []string{"-pixel_format", "nv12"}},
		},
		{
			name: "larger than H.264 carries: the next size", goos: "windows",
			listing: optionsListing(
				"vcodec=mjpeg  min s=5120x2880 fps=30 max s=5120x2880 fps=30",
				"vcodec=mjpeg  min s=3840x2160 fps=30 max s=3840x2160 fps=30",
			),
			want: "mjpeg 3840x2160 at 30 fps", input: Input{Rate: "30", Format: []string{"-vcodec", "mjpeg"}},
		},
		{
			name: "a configured size and rate are kept", goos: "windows",
			listing: withDecoders(instaLinkListing), conf: Options{VideoSize: "1280x720", FPS: 25},
			want: "mjpeg 1280x720 at 25 fps", input: Input{Rate: "25", Format: []string{"-vcodec", "mjpeg"}},
		},
		{
			name: "a configured size the camera does not have", goos: "windows",
			listing: withDecoders(instaLinkListing), conf: Options{VideoSize: "800x600"},
		},
		{
			name: "a size range: its largest", goos: "windows",
			listing: optionsListing("pixel_format=nv12  min s=320x240 fps=1 max s=1920x1080 fps=30"),
			want:    "nv12 1920x1080 at 30 fps", input: Input{Rate: "30", Format: []string{"-pixel_format", "nv12"}},
		},
		{
			name: "a Mac camera: 4K at its 30 fps range, asked for the range's top", goos: "darwin",
			listing: avfModesListing,
			want:    "raw 3840x2160 at 30 fps", input: Input{Rate: "30.000030"},
		},
		{
			name: "a Mac camera asked for 15 fps: its 30 fps range, thinned", goos: "darwin",
			listing: avfModesListing, conf: Options{VideoSize: "1280x720", FPS: 15},
			want: "raw 1280x720 at 15 fps (the camera at 30)", input: Input{Rate: "30.000030"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var modes []Mode
			if tc.goos == "darwin" {
				modes = parseAVFoundationModes(tc.listing)
			} else {
				modes = parseDShowModes(tc.listing)
			}
			c, ok := choose(tc.goos, modes, tc.conf)
			if tc.want == "" {
				if ok {
					t.Fatalf("chose %s", c)
				}
				return
			}
			if !ok {
				t.Fatalf("nothing chosen from %s", describeModes(modes))
			}
			if c.String() != tc.want || !reflect.DeepEqual(c.Input, tc.input) {
				t.Fatalf("chose %s %+v, want %s %+v", c, c.Input, tc.want, tc.input)
			}
		})
	}
}

func TestBitrateFor(t *testing.T) {
	for _, tc := range []struct {
		size string
		fps  int
		want string
	}{
		{"1280x720", 30, "2800k"}, {"1920x1080", 30, "6200k"}, {"3840x2160", 30, "24900k"},
		{"3840x2160", 24, "19900k"}, {"640x480", 30, "900k"}, {"320x240", 30, "600k"}, {"bad", 30, "600k"},
	} {
		if got := BitrateFor(tc.size, tc.fps); got != tc.want {
			t.Errorf("BitrateFor(%s, %d) = %s, want %s", tc.size, tc.fps, got, tc.want)
		}
	}
	if got := (Options{}).WithDefaults().Bitrate; got != "2800k" {
		t.Errorf("default bit rate %s", got)
	}
}

// The source reads the camera's modes once its devices are chosen: what is
// not configured comes from the mode chosen, the bit rate ceiling follows
// the picture, and ffmpeg is told how to open the camera.
func TestSourceShapesThePictureFromTheCamerasModes(t *testing.T) {
	t.Setenv("CAPTURE_FAKE_FFMPEG", "1")
	_, src := testSourceOn(t, "windows", Options{}, "ffmpeg")
	var asked Device
	src.modes = func(_ context.Context, goos, _ string, cam Device) ([]Mode, error) {
		asked = cam
		return parseDShowModes(withDecoders(instaLinkListing)), nil
	}
	src.shape(context.Background())
	if asked.ID != "Insta360 Link" {
		t.Fatalf("modes read of %+v", asked)
	}
	o := src.Options()
	if o.VideoSize != "3840x2160" || o.FPS != 30 || o.Bitrate != "24900k" || src.Bitrate() != "24900k" {
		t.Fatalf("options %+v, bit rate %s", o, src.Bitrate())
	}
	src.mu.Lock()
	args := strings.Join(Args(src.goos, src.opts, src.input, src.cam, src.mic, src.encoder, "rtsp://127.0.0.1:1/x"), " ")
	src.mu.Unlock()
	for _, want := range []string{"-framerate 30 -video_size 3840x2160 -vcodec mjpeg -thread_queue_size 512 -i video=Insta360 Link:", "-b:v 24900k"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}

	// Configured values stand; the rest is the camera's.
	_, set := testSourceOn(t, "windows", Options{VideoSize: "1920x1080", Bitrate: "8M"}, "ffmpeg")
	set.modes = src.modes
	set.shape(context.Background())
	if o := set.Options(); o.VideoSize != "1920x1080" || o.FPS != 30 || o.Bitrate != "8M" || set.Input().Rate != "30" {
		t.Fatalf("configured: options %+v, input %+v", o, set.Input())
	}

	// No mode will do (no decoder for any): the camera is opened as
	// configured, at the defaults, unpinned.
	_, none := testSourceOn(t, "windows", Options{}, "ffmpeg")
	none.modes = func(context.Context, string, string, Device) ([]Mode, error) {
		return parseDShowModes(instaLinkListing), nil
	}
	none.shape(context.Background())
	if o := none.Options(); o.VideoSize != Defaults.VideoSize || o.FPS != Defaults.FPS || o.Bitrate != "2800k" || none.Input().Format != nil {
		t.Fatalf("unusable modes: options %+v, input %+v", o, none.Input())
	}
}

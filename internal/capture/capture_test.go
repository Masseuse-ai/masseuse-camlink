package capture

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// TestMain doubles as the fake ffmpeg: the test binary re-executed with
// CAPTURE_FAKE_FFMPEG set behaves like ffmpeg publishing to the intake.
// With CAPTURE_REAL_FFMPEG_WRAP set instead it stands in for a real ffmpeg
// long enough to swap the camera for a test source (realffmpeg_test.go).
func TestMain(m *testing.M) {
	if os.Getenv("CAPTURE_FAKE_FFMPEG") == "1" {
		os.Exit(fakeFFmpeg(os.Args[1:]))
	}
	if real := os.Getenv(realFFmpegEnv + "_WRAP"); real != "" {
		os.Exit(execFFmpeg(real, lavfiInputs(os.Args[1:])))
	}
	os.Exit(m.Run())
}

func fakeFFmpeg(args []string) int {
	if slices.Contains(args, "-encoders") {
		// `ffmpeg -encoders`: a build with libx264 unless told otherwise.
		fmt.Println("Encoders:\n V..... = Video\n A..... = Audio\n ------\n V....D h264_videotoolbox    VideoToolbox H.264 Encoder (codec h264)")
		if os.Getenv("CAPTURE_FAKE_NO_X264") != "1" {
			fmt.Println(" V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)")
		}
		return 0
	}
	url, encoder, bitrate, input := args[len(args)-1], "", "", ""
	for i, a := range args {
		if a == "-c:v" && i+1 < len(args) {
			encoder = args[i+1]
		}
		if a == "-b:v" && i+1 < len(args) {
			bitrate = args[i+1]
		}
		if a == "-i" && i+1 < len(args) {
			input = args[i+1]
		}
	}
	if os.Getenv("CAPTURE_FAKE_HW_FAILS") == "1" && encoder != EncoderX264 {
		fmt.Fprintln(os.Stderr, "[vost#0:0 @ 0x1] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height")
		return 1
	}
	if encoder == EncoderX264 && (os.Getenv("CAPTURE_FAKE_NO_X264") == "1" || os.Getenv("CAPTURE_FAKE_X264_UNKNOWN") == "1") {
		// What the connector's own ffmpeg says to -preset: it is built
		// without libx264, so nothing knows the option.
		fmt.Fprintln(os.Stderr, "Unrecognized option 'preset'.\nError splitting the argument list: Option not found")
		return 8
	}
	if bad := os.Getenv("CAPTURE_FAKE_BAD_INPUT"); bad != "" && input == bad {
		// avfoundation given an index the renumbered list no longer has.
		fmt.Fprintf(os.Stderr, "[AVFoundation indev @ 0x1] Invalid audio device index\n[in#0 @ 0x2] Error opening input: Input/output error\nError opening input file %s.\nError opening input files: Input/output error\n", input)
		return 251
	}
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 97, ChannelCount: 1}}},
	}}
	c := gortsplib.Client{}
	if err := c.StartRecording(url, desc); err != nil {
		fmt.Fprintln(os.Stderr, "announce:", err)
		return 1
	}
	defer c.Close()
	quit := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "q" {
				break
			}
		}
		close(quit)
	}()
	// The fake's clock, as its sender reports tell it, sits
	// CAPTURE_FAKE_CLOCK_BEHIND_MS behind this computer's: the tests check
	// that time reaches the stream's readers instead of the forwarding time.
	behind, _ := strconv.Atoi(os.Getenv("CAPTURE_FAKE_CLOCK_BEHIND_MS"))
	start := time.Now()
	var seq uint16
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-quit:
			return 0
		case <-tick.C:
		}
		seq++
		now := time.Now()
		// Timestamps follow the wall clock, as an encoder's do.
		ts := uint32(now.Sub(start).Seconds() * 90000)
		for _, m := range desc.Medias {
			pt := m.Formats[0].PayloadType()
			// The video payload carries the bit rate this ffmpeg was started
			// with, after the IDR NAL header, so tests can see which
			// generation a packet came from.
			pkt := &rtp.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: uint32(pt)},
				Payload: append([]byte{0x65}, bitrate...),
			}
			if err := c.WritePacketRTPWithNTP(m, pkt, now.Add(-time.Duration(behind)*time.Millisecond)); err != nil {
				return 1
			}
		}
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const avfListing = `[AVFoundation indev @ 0x7f8] AVFoundation video devices:
[AVFoundation indev @ 0x7f8] [0] Insta360 Link
[AVFoundation indev @ 0x7f8] [1] FaceTime HD Camera
[AVFoundation indev @ 0x7f8] [2] Capture screen 0
[AVFoundation indev @ 0x7f8] AVFoundation audio devices:
[AVFoundation indev @ 0x7f8] [0] Yeti Stereo Microphone
[AVFoundation indev @ 0x7f8] [1] Insta360 Link
[AVFoundation indev @ 0x7f8] [2] MacBook Pro Microphone
[in#0 @ 0x7f9] Error opening input: Input/output error
Error opening input file .
`

const dshowListing = `[dshow @ 000001] "Insta360 Link" (video)
[dshow @ 000001]   Alternative name "@device_pnp_\\?\usb#vid_2e1a&pid_4c01&mi_00#7&2b7a9c0&0&0000#{65e8773d-8f56-11d0-a3b9-00a0c9223196}\global"
[dshow @ 000001] "OBS Virtual Camera" (video)
[dshow @ 000001]   Alternative name "@device_sw_{860BB310-5D01-11D0-BD3B-00A0C911CE86}\{A3FCE0F5-3493-419F-958A-ABA1250EC20B}"
[dshow @ 000001] "Microphone (Yeti Stereo Microphone)" (audio)
[dshow @ 000001]   Alternative name "@device_cm_{33D9A762-90C8-11D0-BD43-00A0C911CE86}\wave_{6D0B8A5E-1}"
[dshow @ 000001] "Microphone Array (Realtek(R) Audio)" (audio)
[dshow @ 000001]   Alternative name "@device_cm_{33D9A762-90C8-11D0-BD43-00A0C911CE86}\wave_{4A1C7B2D-2}"
dummy: Immediate exit requested
`

const pactlListing = `Source #0
	State: SUSPENDED
	Name: alsa_output.pci-0000_00_1f.3.analog-stereo.monitor
	Description: Monitor of Built-in Audio Analog Stereo
	Driver: PipeWire
Source #1
	State: SUSPENDED
	Name: alsa_input.usb-Blue_Microphones_Yeti_Stereo_Microphone-00.analog-stereo
	Description: Yeti Stereo Microphone Analog Stereo
	Driver: PipeWire
Source #2
	State: RUNNING
	Name: alsa_input.pci-0000_00_1f.3.analog-stereo
	Description: Built-in Audio Analog Stereo
`

func TestParseAVFoundation(t *testing.T) {
	got := parseAVFoundation(avfListing)
	// Every name stands alone in its kind, so each device is addressed by
	// name: the index moves when a Continuity Camera phone comes or goes.
	want := []Device{
		{Kind: Video, ID: "0", Name: "Insta360 Link", Input: "Insta360 Link"},
		{Kind: Video, ID: "1", Name: "FaceTime HD Camera", Input: "FaceTime HD Camera"},
		{Kind: Audio, ID: "0", Name: "Yeti Stereo Microphone", Input: "Yeti Stereo Microphone"},
		{Kind: Audio, ID: "1", Name: "Insta360 Link", Input: "Insta360 Link"},
		{Kind: Audio, ID: "2", Name: "MacBook Pro Microphone", Input: "MacBook Pro Microphone"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}

	// A name with a colon (avfoundation's separator), one that begins
	// another device's name (avfoundation matches the prefix) and twins
	// keep the index; the longer name is still alone.
	odd := parseAVFoundation(`[AVFoundation indev @ 0x7f8] AVFoundation video devices:
[AVFoundation indev @ 0x7f8] [0] Logitech BRIO
[AVFoundation indev @ 0x7f8] [1] Logitech BRIO
[AVFoundation indev @ 0x7f8] [2] Cam Link: 4K
[AVFoundation indev @ 0x7f8] AVFoundation audio devices:
[AVFoundation indev @ 0x7f8] [0] Yeti
[AVFoundation indev @ 0x7f8] [1] Yeti Stereo Microphone
`)
	wantOdd := []Device{
		{Kind: Video, ID: "0", Name: "Logitech BRIO"},
		{Kind: Video, ID: "1", Name: "Logitech BRIO"},
		{Kind: Video, ID: "2", Name: "Cam Link: 4K"},
		{Kind: Audio, ID: "0", Name: "Yeti"},
		{Kind: Audio, ID: "1", Name: "Yeti Stereo Microphone", Input: "Yeti Stereo Microphone"},
	}
	if !reflect.DeepEqual(odd, wantOdd) {
		t.Fatalf("got %+v", odd)
	}
	if d := (Device{ID: "2", Name: "Cam Link: 4K"}); d.input() != "2" {
		t.Fatalf("input %q", d.input())
	}
}

func TestClassifyExit(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want exitKind
	}{
		// What the connector's ffmpeg wrote when the microphone's index
		// had moved, then when it was pointed at libx264 it does not have.
		{"ffmpeg: exit status 251: [AVFoundation indev @ 0x1] Invalid audio device index | [in#0 @ 0x2] Error opening input: Input/output error | Error opening input file 0:1. | Error opening input files: Input/output error", exitInput},
		{"ffmpeg: exit status 8: Unrecognized option 'preset'. | Error splitting the argument list: Option not found", exitOption},
		{"ffmpeg: exit status 1: Unknown encoder 'libx264'", exitOption},
		{"ffmpeg: exit status 1: [vost#0:0 @ 0x1] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height", exitEncoder},
		{"ffmpeg: exit status 1: [h264_videotoolbox @ 0x1] Error: cannot create compression session: -12908", exitEncoder},
		{"ffmpeg: exit status 1: [dshow @ 0x1] Could not find video device with name [Insta360 Link] among source devices of type video. | Error opening input file video=Insta360 Link.", exitInput},
		{"ffmpeg: exit status 1: [video4linux2,v4l2 @ 0x1] Cannot open video device /dev/video0: No such file or directory | Error opening input file /dev/video0.", exitInput},
		// An avfoundation warning in the tail does not make an encoder
		// failure an input one.
		{"ffmpeg: exit status 1: [AVFoundation indev @ 0x1] Selected framerate (29.970030) is not supported by the device. | [vost#0:0 @ 0x2] Error while opening encoder - maybe incorrect parameters", exitEncoder},
		{"ffmpeg: signal: killed: ", exitUnknown},
	} {
		if got := classifyExit(errors.New(tc.msg)); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.msg, got, tc.want)
		}
	}
	if classifyExit(nil) != exitUnknown {
		t.Error("nil")
	}
	list := "Encoders:\n V..... = Video\n ------\n V....D h264_videotoolbox    VideoToolbox H.264 Encoder (codec h264)\n V....D libx264              libx264 H.264 / AVC (codec h264)\n"
	if !listsEncoder(list, "libx264") || !listsEncoder(list, "h264_videotoolbox") || listsEncoder(list, "libx265") || listsEncoder(list, "Video") {
		t.Error("listsEncoder")
	}
}

func TestParseDShow(t *testing.T) {
	got := parseDShow(dshowListing)
	want := []Device{
		{Kind: Video, ID: "Insta360 Link", Name: "Insta360 Link"},
		{Kind: Video, ID: "OBS Virtual Camera", Name: "OBS Virtual Camera"},
		{Kind: Audio, ID: "Microphone (Yeti Stereo Microphone)", Name: "Microphone (Yeti Stereo Microphone)"},
		{Kind: Audio, ID: "Microphone Array (Realtek(R) Audio)", Name: "Microphone Array (Realtek(R) Audio)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestLinuxDevices(t *testing.T) {
	root := t.TempDir()
	for node, name := range map[string]string{
		"video0": "Insta360 Link: Insta360 Link", "video1": "Insta360 Link: Insta360 Link", // metadata node
		"video2": "Integrated Camera: Integrated C", "v4l-subdev0": "sensor",
	} {
		if err := os.MkdirAll(filepath.Join(root, node), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, node, "name"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readV4L2(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{
		{Kind: Video, ID: "/dev/video0", Name: "Insta360 Link: Insta360 Link"},
		{Kind: Video, ID: "/dev/video2", Name: "Integrated Camera: Integrated C"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v4l2 got %+v", got)
	}
	if got, err := readV4L2(filepath.Join(root, "missing")); err != nil || got != nil {
		t.Fatalf("missing sysfs: %v %v", got, err)
	}
	mics := parsePactlSources(pactlListing)
	wantMics := []Device{
		{Kind: Audio, ID: "alsa_input.usb-Blue_Microphones_Yeti_Stereo_Microphone-00.analog-stereo", Name: "Yeti Stereo Microphone Analog Stereo"},
		{Kind: Audio, ID: "alsa_input.pci-0000_00_1f.3.analog-stereo", Name: "Built-in Audio Analog Stereo"},
	}
	if !reflect.DeepEqual(mics, wantMics) {
		t.Fatalf("pactl got %+v", mics)
	}
}

func TestSelect(t *testing.T) {
	devs := parseAVFoundation(avfListing)
	cases := []struct {
		kind    Kind
		sel     string
		wantID  string
		wantErr bool
	}{
		{Video, "", "0", false},
		{Video, "1", "1", false},
		{Video, "facetime", "1", false},
		{Video, "FaceTime HD Camera", "1", false},
		{Video, "2", "", true},        // screen recorders are not listed
		{Video, "camera", "1", false}, // one camera has "camera" in its name
		{Audio, "yeti", "0", false},
		{Audio, "insta360", "1", false},
		{Audio, "microphone", "", true}, // Yeti Stereo Microphone and MacBook Pro Microphone
		{Audio, "nothing", "", true},
	}
	for _, tc := range cases {
		d, err := Select(devs, tc.kind, tc.sel)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s %q: err %v", tc.kind, tc.sel, err)
			continue
		}
		if !tc.wantErr && d.ID != tc.wantID {
			t.Errorf("%s %q: got %s want %s", tc.kind, tc.sel, d.ID, tc.wantID)
		}
	}
	if _, err := Select(nil, Video, ""); err == nil {
		t.Error("no devices: nil error")
	}
}

// avfContinuityListing is a Mac with an iPhone joined through Continuity
// Camera: macOS lists the phone's microphone before the computer's own.
const avfContinuityListing = `[AVFoundation indev @ 0x7f8] AVFoundation video devices:
[AVFoundation indev @ 0x7f8] [0] iPhone Desk View
[AVFoundation indev @ 0x7f8] [1] iPhone Camera
[AVFoundation indev @ 0x7f8] [2] FaceTime HD Camera
[AVFoundation indev @ 0x7f8] [3] Capture screen 0
[AVFoundation indev @ 0x7f8] AVFoundation audio devices:
[AVFoundation indev @ 0x7f8] [0] Someone’s iPhone Microphone
[AVFoundation indev @ 0x7f8] [1] MacBook Pro Microphone
[in#0 @ 0x7f9] Error opening input: Input/output error
Error opening input file .
`

func TestDefaultPassesOverContinuityDevices(t *testing.T) {
	devs := parseAVFoundation(avfContinuityListing)
	cases := []struct {
		kind   Kind
		sel    string
		wantID string
	}{
		{Video, "", "2"},  // the computer's own camera, not the phone's
		{Audio, "", "1"},  // the computer's own microphone, not the phone's
		{Video, "0", "0"}, // numbers keep the listing's order
		{Audio, "0", "0"},
		{Audio, "iphone", "0"}, // and the phone can be asked for by name
		{Video, "desk view", "0"},
	}
	for _, tc := range cases {
		d, err := Select(devs, tc.kind, tc.sel)
		if err != nil || d.ID != tc.wantID {
			t.Errorf("%s %q: got %s, %v; want %s", tc.kind, tc.sel, d.ID, err, tc.wantID)
		}
	}
	// A remembered device that is absent gives way to the same default.
	cam, mic, subs, err := Resolve(devs, Options{Camera: "Studio Cam 4K", Mic: "USB Mic", Fallback: true})
	if err != nil || cam.ID != "2" || mic == nil || mic.ID != "1" {
		t.Fatalf("cam %+v mic %+v err %v", cam, mic, err)
	}
	if len(subs) != 2 || subs[0].Using.Name != "FaceTime HD Camera" || subs[1].Using.Name != "MacBook Pro Microphone" {
		t.Fatalf("substitutions %+v", subs)
	}
	// With nothing but the phone connected, the phone it is.
	phoneOnly := []Device{
		{Kind: Video, ID: "0", Name: "iPhone Camera"},
		{Kind: Audio, ID: "0", Name: "Someone’s iPhone Microphone"},
	}
	for _, kind := range []Kind{Video, Audio} {
		if d, err := Select(phoneOnly, kind, ""); err != nil || d.ID != "0" {
			t.Errorf("phone only, %s: got %+v, %v", kind, d, err)
		}
	}
}

func TestResolveFallsBackForARememberedDeviceOnly(t *testing.T) {
	devs := parseAVFoundation(avfListing)
	// Named on the command line and absent: an error, whatever else is there.
	if _, _, _, err := Resolve(devs, Options{Camera: "Studio Cam 4K"}); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("flag naming an absent camera: err %v", err)
	}
	if _, _, _, err := Resolve(devs, Options{Mic: "USB Mic"}); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("flag naming an absent microphone: err %v", err)
	}
	// Remembered and absent: the first of its kind stands in, and says so.
	cam, mic, subs, err := Resolve(devs, Options{Camera: "Studio Cam 4K", Mic: "USB Mic", Fallback: true})
	if err != nil {
		t.Fatal(err)
	}
	if cam.ID != "0" || mic == nil || mic.ID != "0" {
		t.Fatalf("cam %+v mic %+v", cam, mic)
	}
	if len(subs) != 2 || subs[0].Kind != Video || subs[0].Wanted != "Studio Cam 4K" || subs[0].Using.ID != "0" ||
		subs[1].Kind != Audio || subs[1].Wanted != "USB Mic" || subs[1].Using.ID != "0" {
		t.Fatalf("substitutions %+v", subs)
	}
	// Remembered and present: no substitution; "none" is not a device.
	cam, mic, subs, err = Resolve(devs, Options{Camera: "FaceTime HD Camera", Mic: "none", Fallback: true})
	if err != nil || cam.ID != "1" || mic != nil || subs != nil {
		t.Fatalf("cam %+v mic %+v subs %+v err %v", cam, mic, subs, err)
	}
	// No device of the kind at all: the error stands even with the fallback.
	var audioOnly []Device
	for _, d := range devs {
		if d.Kind == Audio {
			audioOnly = append(audioOnly, d)
		}
	}
	if _, _, _, err := Resolve(audioOnly, Options{Camera: "Studio Cam 4K", Fallback: true}); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("no cameras: err %v", err)
	}
}

func TestArgs(t *testing.T) {
	cam := Device{Kind: Video, ID: "0", Name: "Insta360 Link"}
	mic := Device{Kind: Audio, ID: "1", Name: "Yeti"}
	url := "rtsp://127.0.0.1:5000/secret"
	join := func(a []string) string { return strings.Join(a, " ") }

	mac := join(Args("darwin", Options{}, cam, &mic, EncoderVideoToolbox, url))
	for _, want := range []string{
		"-f avfoundation -framerate 30 -video_size 1280x720 -thread_queue_size 512 -i 0:1",
		"-c:v h264_videotoolbox -realtime 1", "-profile:v main -level 3.1",
		"-pix_fmt yuv420p -r 30 -g 60 -force_key_frames expr:gte(t,n_forced*2)",
		"-b:v 2500k -maxrate 2500k -bufsize 2500k", "-c:a libopus -ac 1 -ar 48000 -b:a 64k",
		"-f rtsp -rtsp_transport tcp -pkt_size 1200 " + url,
	} {
		if !strings.Contains(mac, want) {
			t.Errorf("darwin args missing %q:\n%s", want, mac)
		}
	}
	if !strings.HasSuffix(mac, url) {
		t.Errorf("url must be last: %s", mac)
	}
	// A device the listing gave a name to (Device.Input) is opened by that
	// name; one it did not, by index.
	named := Args("darwin", Options{}, Device{Kind: Video, ID: "0", Name: "Insta360 Link", Input: "Insta360 Link"},
		&Device{Kind: Audio, ID: "2", Name: "MacBook Pro Microphone", Input: "MacBook Pro Microphone"}, EncoderVideoToolbox, url)
	if i := slices.Index(named, "-i"); i < 0 || named[i+1] != "Insta360 Link:MacBook Pro Microphone" {
		t.Errorf("darwin input by name: %q", named)
	}
	mixed := Args("darwin", Options{}, Device{Kind: Video, ID: "0", Name: "Insta360 Link", Input: "Insta360 Link"},
		&Device{Kind: Audio, ID: "2", Name: "Cam Link: 4K"}, EncoderVideoToolbox, url)
	if i := slices.Index(mixed, "-i"); i < 0 || mixed[i+1] != "Insta360 Link:2" {
		t.Errorf("darwin input by name and index: %q", mixed)
	}

	win := join(Args("windows", Options{FPS: 25, VideoSize: "1920x1080", Bitrate: "4M"},
		Device{ID: "Insta360 Link"}, &Device{ID: "Microphone (Yeti)"}, EncoderMediaFound, url))
	for _, want := range []string{
		"-f dshow -rtbufsize 100M -framerate 25 -video_size 1920x1080 -thread_queue_size 512 -i video=Insta360 Link:audio=Microphone (Yeti)",
		"-c:v h264_mf", "-g 50", "-b:v 4M -maxrate 4M -bufsize 4M",
	} {
		if !strings.Contains(win, want) {
			t.Errorf("windows args missing %q:\n%s", want, win)
		}
	}
	if strings.Contains(win, "-level") {
		t.Errorf("h264_mf takes no level:\n%s", win)
	}

	lin := join(Args("linux", Options{}, Device{ID: "/dev/video0"}, &Device{ID: "alsa_input.usb-yeti"}, EncoderX264, url))
	for _, want := range []string{
		"-f v4l2 -framerate 30 -video_size 1280x720 -thread_queue_size 512 -i /dev/video0",
		"-f pulse -thread_queue_size 512 -i alsa_input.usb-yeti -map 0:v:0 -map 1:a:0",
		"-c:v libx264 -preset veryfast -tune zerolatency -profile:v main -level 3.1",
	} {
		if !strings.Contains(lin, want) {
			t.Errorf("linux args missing %q:\n%s", want, lin)
		}
	}
	// The level follows the picture, not the bit rate.
	hd := join(Args("linux", Options{VideoSize: "1920x1080", Bitrate: "600k"}, Device{ID: "/dev/video0"}, nil, EncoderX264, url))
	if !strings.Contains(hd, "-level 4.0") {
		t.Errorf("1080p30 level:\n%s", hd)
	}
	for _, tc := range []struct {
		size string
		fps  int
		want string
	}{
		{"1280x720", 30, "3.1"}, {"640x480", 30, "3.1"}, {"1280x720", 60, "3.2"}, {"1920x1080", 30, "4.0"},
		{"1920x1080", 60, "4.2"}, {"3840x2160", 30, "5.1"}, {"3840x2160", 60, "5.2"}, {"bad", 30, "3.1"},
	} {
		if got := h264Level(tc.size, tc.fps); got != tc.want {
			t.Errorf("level for %s@%d: %s, want %s", tc.size, tc.fps, got, tc.want)
		}
	}

	solo := join(Args("darwin", Options{}, cam, nil, EncoderX264, url))
	if !strings.Contains(solo, "-i 0 ") || !strings.Contains(solo, " -an ") || strings.Contains(solo, "libopus") {
		t.Errorf("video only: %s", solo)
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := (Options{}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Options{
		{VideoSize: "720p"}, {FPS: 500}, {Bitrate: "fast"}, {Encoder: "hevc_videotoolbox"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if HardwareEncoder("darwin") != EncoderVideoToolbox || HardwareEncoder("windows") != EncoderMediaFound || HardwareEncoder("linux") != EncoderX264 {
		t.Error("hardware encoders")
	}
	for _, goos := range []string{"darwin", "windows", "linux"} {
		if installHint(goos) == "" {
			t.Errorf("no hint for %s", goos)
		}
	}
}

func TestFindFFmpegMissing(t *testing.T) {
	if _, err := FindFFmpeg(filepath.Join(t.TempDir(), "nope")); !IsNoFFmpeg(err) {
		t.Fatalf("err %v", err)
	}
}

// TestBundledFFmpeg: the ffmpeg shipped with the program is found in a
// macOS bundle's Contents/Helpers and beside the executable anywhere, and
// nowhere else.
func TestBundledFFmpeg(t *testing.T) {
	app := filepath.Join("/Applications", "masseuse-camlink.app", "Contents")
	appExe := filepath.Join(app, "MacOS", "masseuse-camlink")
	helper := filepath.Join(app, "Helpers", "ffmpeg")
	folderExe := filepath.Join("/Users", "me", "masseuse-camlink", "masseuse-camlink")
	winExe := filepath.Join("C:", "camlink", "masseuse-camlink.exe")
	has := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, want := range paths {
				if p == want {
					return true
				}
			}
			return false
		}
	}
	for _, tc := range []struct {
		name, exe, goos string
		exists          func(string) bool
		want            string
	}{
		{"bundle helper", appExe, "darwin", has(helper), helper},
		{"bundle, helper preferred over a neighbour", appExe, "darwin", has(helper, filepath.Join(app, "MacOS", "ffmpeg")), helper},
		{"bundle without ffmpeg", appExe, "darwin", has(), ""},
		{"beside the executable on a Mac", folderExe, "darwin", has(filepath.Join(filepath.Dir(folderExe), "ffmpeg")), filepath.Join(filepath.Dir(folderExe), "ffmpeg")},
		{"beside the executable on Linux", folderExe, "linux", has(filepath.Join(filepath.Dir(folderExe), "ffmpeg")), filepath.Join(filepath.Dir(folderExe), "ffmpeg")},
		{"beside the executable on Windows", winExe, "windows", has(filepath.Join(filepath.Dir(winExe), "ffmpeg.exe")), filepath.Join(filepath.Dir(winExe), "ffmpeg.exe")},
		{"Windows wants the .exe", winExe, "windows", has(filepath.Join(filepath.Dir(winExe), "ffmpeg")), ""},
		{"a Helpers directory is only a bundle's", folderExe, "linux", has(filepath.Join("/Users", "me", "Helpers", "ffmpeg")), ""},
	} {
		if got := bundledFFmpeg(tc.exe, tc.goos, tc.exists); got != tc.want {
			t.Errorf("%s: bundledFFmpeg(%q, %s) = %q, want %q", tc.name, tc.exe, tc.goos, got, tc.want)
		}
	}
}

// TestFindFFmpegBeside runs the real lookup with a stand-in placed beside a
// copy of the test binary's own path: the neighbour wins over PATH.
func TestFindFFmpegBeside(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	dir := filepath.Dir(exe)
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	beside := filepath.Join(dir, name)
	if _, err := os.Stat(beside); err == nil {
		t.Skipf("%s exists already", beside)
	}
	if err := os.WriteFile(beside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Skipf("cannot write beside the test binary: %v", err)
	}
	defer os.Remove(beside)
	got, err := FindFFmpeg("")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(beside); err == nil {
		beside = resolved
	}
	if got != beside {
		t.Fatalf("FindFFmpeg = %q, want the neighbour %q", got, beside)
	}
}

// playback is what a reader saw: packets by count, the latest video
// payload, whether video sequence numbers ever skipped, and how far behind
// this computer's clock the stream's sender reports timed the latest video
// packet (lag, nanoseconds; timed is whether a report has arrived).
type playback struct {
	n           atomic.Int64
	lastVideo   atomic.Pointer[[]byte]
	seqGaps     atomic.Int64
	haveSeq     atomic.Bool
	lastSeq     atomic.Uint32
	firstVideoP atomic.Pointer[[]byte]
	timed       atomic.Bool
	lag         atomic.Int64
}

// video is the bit rate the latest video packet's publisher was started
// with (fakeFFmpeg puts it after the NAL header).
func (p *playback) video() string {
	if b := p.lastVideo.Load(); b != nil && len(*b) > 1 {
		return string((*b)[1:])
	}
	return ""
}

// play reads the connector's stream over the server's in-process dial.
func play(t *testing.T, srv *serve.Server) (*gortsplib.Client, *playback) {
	t.Helper()
	p := &playback{}
	c := &gortsplib.Client{
		Scheme: "rtsps", Host: serve.Target, ReadTimeout: 10 * time.Second, Protocol: &tcp,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return srv.Dial() },
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	u, _ := base.ParseURL(serve.Link)
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(desc.Medias) != 2 {
		t.Fatalf("medias %d", len(desc.Medias))
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatal(err)
	}
	c.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		p.n.Add(1)
		if medi.Type != description.MediaTypeVideo {
			return
		}
		if ntp, ok := c.PacketNTP(medi, pkt); ok {
			p.lag.Store(int64(time.Since(ntp)))
			p.timed.Store(true)
		}
		payload := append([]byte(nil), pkt.Payload...)
		p.lastVideo.Store(&payload)
		if p.haveSeq.Load() && uint16(p.lastSeq.Load())+1 != pkt.SequenceNumber {
			p.seqGaps.Add(1)
		}
		p.lastSeq.Store(uint32(pkt.SequenceNumber))
		p.haveSeq.Store(true)
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatal(err)
	}
	return c, p
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testSource(t *testing.T, opts Options) (*serve.Server, *Source) {
	t.Helper()
	t.Setenv("CAPTURE_FAKE_FFMPEG", "1")
	t.Setenv("CAPTURE_FAKE_CLOCK_BEHIND_MS", "800")
	return testSourceWith(t, opts, os.Args[0])
}

// testSourceWith is a darwin Source on the given ffmpeg: the test binary as
// the fake (testSource) or as the wrapper of a real one (realffmpeg_test.go).
func testSourceWith(t *testing.T, opts Options, ffmpeg string) (*serve.Server, *Source) {
	t.Helper()
	return testSourceOn(t, "darwin", opts, ffmpeg)
}

// testSourceOn is testSourceWith for the given system: the device ids take
// the form that system's ffmpeg input wants.
func testSourceOn(t *testing.T, goos string, opts Options, ffmpeg string) (*serve.Server, *Source) {
	t.Helper()
	srv, err := serve.New(serve.Config{StateDir: t.TempDir(), Logger: quiet(), DescribeWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	cam := Device{Kind: Video, ID: "0", Name: "Insta360 Link"}
	mic := Device{Kind: Audio, ID: "1", Name: "Yeti Stereo Microphone"}
	if goos == "windows" {
		cam.ID, mic.ID = cam.Name, mic.Name
	}
	src := newSource(srv, opts, quiet(), goos, ffmpeg, cam, &mic)
	src.earlyExit = 2 * time.Second
	t.Cleanup(src.Stop)
	return srv, src
}

func TestSourceStartsFFmpegAndPublishes(t *testing.T) {
	srv, src := testSource(t, Options{})
	if src.Label() != "Insta360 Link + Yeti Stereo Microphone" {
		t.Fatalf("label %q", src.Label())
	}
	if src.Running() || src.Publishing() {
		t.Fatal("running before Start")
	}
	src.Start()
	src.Start() // idempotent
	if !src.Running() {
		t.Fatal("not running")
	}
	// The enclave's DESCRIBE arrives while ffmpeg is still starting.
	c, n := play(t, srv)
	defer c.Close()
	waitFor(t, "packets", func() bool { return n.n.Load() >= 20 })
	if !src.Publishing() || !srv.Stats().Publishing {
		t.Fatal("not publishing")
	}
	if src.Encoder() != EncoderVideoToolbox {
		t.Fatalf("encoder %s", src.Encoder())
	}
	// The stream's readers are told ffmpeg's own time for each frame (its
	// sender reports, here a clock 800 ms behind), not when the connector
	// forwarded the packet.
	waitFor(t, "timed packets", n.timed.Load)
	if lag := time.Duration(n.lag.Load()); lag < 700*time.Millisecond || lag > 1500*time.Millisecond {
		t.Fatalf("reader timed the latest frame %s ago; ffmpeg's clock is 800 ms behind", lag)
	}

	src.Stop()
	if src.Running() {
		t.Fatal("running after Stop")
	}
	waitFor(t, "publication end", func() bool { return !src.Publishing() && !srv.Stats().Publishing })
	waitFor(t, "reader gone", func() bool { return srv.Readers() == 0 })

	// It starts again for the next session.
	src.Start()
	c2, n2 := play(t, srv)
	defer c2.Close()
	waitFor(t, "packets again", func() bool { return n2.n.Load() >= 5 })
	src.Stop()
}

func TestReshapeRestartsTheEncoderWithoutLosingTheReader(t *testing.T) {
	srv, src := testSource(t, Options{})
	if src.Bitrate() != Defaults.Bitrate {
		t.Fatalf("bitrate %q before anything", src.Bitrate())
	}
	// Idle: only the rate for the next start changes.
	if src.Reshape("1600k") {
		t.Fatal("reshaped an idle source")
	}
	if src.Bitrate() != "1600k" || src.Options().Bitrate != Defaults.Bitrate {
		t.Fatalf("bitrate %q, configured %q", src.Bitrate(), src.Options().Bitrate)
	}
	src.Start()
	c, p := play(t, srv)
	defer c.Close()
	waitFor(t, "packets at 1600k", func() bool { return p.n.Load() >= 20 && p.video() == "1600k" })

	// Running: ffmpeg is replaced at the new rate; the reader keeps playing
	// one stream, with no sequence gap and no reconnect.
	if !src.Reshape("1000k") {
		t.Fatal("did not reshape a running source")
	}
	waitFor(t, "packets at 1000k", func() bool { return p.video() == "1000k" })
	if st := srv.Stats(); st.Generations != 2 || !st.Publishing {
		t.Fatalf("stats %+v", st)
	}
	if srv.Readers() != 1 || !src.Publishing() || !src.Running() {
		t.Fatalf("readers %d publishing %v running %v", srv.Readers(), src.Publishing(), src.Running())
	}
	if p.seqGaps.Load() != 0 {
		t.Fatalf("%d sequence gaps across the handover", p.seqGaps.Load())
	}
	if src.Reshape("1000k") {
		t.Fatal("reshaped to the same rate")
	}

	// Stop still turns everything off, and the next start is at the ceiling.
	src.Stop()
	waitFor(t, "publication end", func() bool { return !srv.Stats().Publishing })
	if src.Bitrate() != Defaults.Bitrate {
		t.Fatalf("bitrate %q after Stop", src.Bitrate())
	}
}

func TestSourceFallsBackToSoftwareEncoder(t *testing.T) {
	t.Setenv("CAPTURE_FAKE_HW_FAILS", "1")
	srv, src := testSource(t, Options{})
	src.Start()
	c, n := play(t, srv)
	defer c.Close()
	waitFor(t, "packets", func() bool { return n.n.Load() >= 5 })
	if src.Encoder() != EncoderX264 {
		t.Fatalf("encoder %s after hardware failure", src.Encoder())
	}
}

// The hardware encoder fails and this ffmpeg has no libx264 (the shipped
// one is built without GPL parts): the encoder is not swapped for one that
// is not there, the person is told, and the hardware encoder is retried.
func TestSourceDoesNotFallBackToAnEncoderTheBuildLacks(t *testing.T) {
	t.Setenv("CAPTURE_FAKE_HW_FAILS", "1")
	t.Setenv("CAPTURE_FAKE_NO_X264", "1")
	_, src := testSource(t, Options{})
	src.earlyExit = 100 * time.Millisecond
	src.Start()
	waitFor(t, "trouble", func() bool { return src.Trouble() != "" })
	if src.Encoder() != EncoderVideoToolbox {
		t.Fatalf("encoder %s", src.Encoder())
	}
	if src.Trouble() != "the video encoder failed to start" {
		t.Fatalf("trouble %q", src.Trouble())
	}
	if src.Publishing() {
		t.Fatal("publishing")
	}
}

// `ffmpeg -encoders` lists libx264 but the encoder does not run (the
// options are not known): the fallback is undone at once, the hardware
// encoder is tried again, and libx264 is not tried a second time.
func TestSourceRevertsAFallbackThatDoesNotRun(t *testing.T) {
	t.Setenv("CAPTURE_FAKE_HW_FAILS", "1")
	t.Setenv("CAPTURE_FAKE_X264_UNKNOWN", "1")
	_, src := testSource(t, Options{})
	src.earlyExit = 100 * time.Millisecond
	src.Start()
	waitFor(t, "trouble", func() bool { return src.Trouble() != "" })
	src.mu.Lock()
	enc, no := src.encoder, src.noX264
	src.mu.Unlock()
	if enc != EncoderVideoToolbox || !no {
		t.Fatalf("encoder %s, noX264 %v", enc, no)
	}
	// Once the hardware encoder works (a second attempt), it publishes.
	os.Unsetenv("CAPTURE_FAKE_HW_FAILS")
	srv := src.sink
	c, n := play(t, srv)
	defer c.Close()
	waitFor(t, "packets", func() bool { return n.n.Load() >= 5 })
	if src.Encoder() != EncoderVideoToolbox || src.Trouble() != "" {
		t.Fatalf("encoder %s, trouble %q", src.Encoder(), src.Trouble())
	}
}

// The microphone's index moved between start and session (a Continuity
// Camera phone went out of reach): ffmpeg cannot open input 0:1. That is
// not an encoder failure; the devices are listed and chosen again, and
// the next ffmpeg opens what is there, on the same encoder.
func TestSourceChoosesTheDevicesAgainWhenTheInputCannotBeOpened(t *testing.T) {
	t.Setenv("CAPTURE_FAKE_BAD_INPUT", "0:1")
	srv, src := testSource(t, Options{})
	src.earlyExit = 100 * time.Millisecond
	var listed atomic.Int32
	src.list = func(context.Context, string) ([]Device, error) {
		listed.Add(1)
		return []Device{
			{Kind: Video, ID: "0", Name: "Insta360 Link"},
			{Kind: Audio, ID: "0", Name: "Yeti Stereo Microphone"},
		}, nil
	}
	src.Start()
	c, n := play(t, srv)
	defer c.Close()
	waitFor(t, "packets", func() bool { return n.n.Load() >= 5 })
	if src.Encoder() != EncoderVideoToolbox {
		t.Fatalf("encoder %s: an input failure is not the encoder's", src.Encoder())
	}
	if listed.Load() == 0 {
		t.Fatal("devices were not listed again")
	}
	if mic := src.Mic(); mic == nil || mic.ID != "0" {
		t.Fatalf("mic %+v", mic)
	}
	if src.Trouble() != "" {
		t.Fatalf("trouble %q while publishing", src.Trouble())
	}
}

// When the device cannot be chosen again (it is gone), the choice stands
// and the retry goes on; the person is told what is missing.
func TestSourceKeepsTheDevicesWhenNoneCanBeChosen(t *testing.T) {
	t.Setenv("CAPTURE_FAKE_BAD_INPUT", "0:1")
	_, src := testSource(t, Options{})
	src.earlyExit = 100 * time.Millisecond
	src.list = func(context.Context, string) ([]Device, error) {
		return []Device{{Kind: Video, ID: "0", Name: "Insta360 Link"}}, nil
	}
	src.Start()
	waitFor(t, "trouble", func() bool { return strings.HasPrefix(src.Trouble(), "the camera or microphone is not connected") })
	if mic := src.Mic(); mic == nil || mic.ID != "1" {
		t.Fatalf("mic %+v", mic)
	}
}

func TestSourceKeepsChosenEncoder(t *testing.T) {
	t.Setenv("CAPTURE_FAKE_HW_FAILS", "1")
	srv, src := testSource(t, Options{Encoder: EncoderVideoToolbox})
	src.earlyExit = 100 * time.Millisecond
	src.Start()
	// The explicit encoder keeps failing: nothing is published, the
	// DESCRIBE times out, and the encoder is not swapped behind the
	// person's back.
	c := &gortsplib.Client{
		Scheme: "rtsps", Host: serve.Target,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return srv.Dial() },
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	u, _ := base.ParseURL(serve.Link)
	if _, _, err := c.Describe(u); err == nil {
		t.Fatal("described with a failing encoder")
	}
	if src.Encoder() != EncoderVideoToolbox {
		t.Fatalf("encoder %s", src.Encoder())
	}
}

// tcp is the transport the enclave uses.
var tcp = gortsplib.ProtocolTCP

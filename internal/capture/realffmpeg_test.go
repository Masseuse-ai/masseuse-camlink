package capture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The bundled ffmpeg (packaging/ffmpeg/build.sh) is a minimal build, so a
// missing component would only show up at the first session. This test,
// run by the release workflow with CAPTURE_REAL_FFMPEG naming that binary,
// starts a Source on it exactly as the connector does and reads the stream
// back: same arguments (Args), same intake, same encoders and muxer. The
// one substitution is the input: the test binary stands in for ffmpeg,
// swaps the camera and microphone for a stand-in and lavfi's tone, and
// execs the real program (realFFmpeg, in the TestMain hook). The stand-in
// camera is lavfi's test picture, which arrives decoded as a raw camera's
// does; on Windows, also a recording in each compressed format a camera
// hands dshow (testdata), which only a decoder turns into frames.
//
// The recordings are a second of a test picture at 1280x720 and 30 fps
// (MJPEG: a third of one, every frame its own) in NUT, which keeps each
// frame's time and lets ffmpeg read it over and over (a raw H.264 stream
// cannot be looped). They were made once with a full ffmpeg (6.1, Ubuntu
// 24.04's) from
// -f lavfi -i testsrc2=size=320x180:rate=30,scale=1280:720:flags=bicubic:
//
//	camera-mjpeg.nut  -frames:v 10 -c:v mjpeg -pix_fmt yuvj422p -q:v 10 -f nut
//	camera-h264.nut   -frames:v 30 -c:v libx264 -preset veryfast -profile:v high -pix_fmt yuv420p -g 30 -bf 0 -crf 28 -f nut
//	camera-hevc.nut   -frames:v 30 -c:v libx265 -preset veryfast -pix_fmt yuv420p -x265-params keyint=30:bframes=0 -crf 30 -f nut

// realFFmpegEnv names the ffmpeg to test; unset, the test is skipped.
// realFFmpegCameraEnv, when set, names the recording that stands in for
// the camera.
const (
	realFFmpegEnv       = "CAPTURE_REAL_FFMPEG"
	realFFmpegCameraEnv = realFFmpegEnv + "_CAMERA"
)

// compressedCameras are the recordings in testdata by the format they are
// in, one per compressed format a camera hands dshow (formats.go).
var compressedCameras = map[string]string{"mjpeg": "camera-mjpeg.nut", "h264": "camera-h264.nut", "hevc": "camera-hevc.nut"}

// realFFmpegOS is the system whose input and encoder the real-ffmpeg test
// exercises: the one it runs on, when that is a system the connector has a
// bundled ffmpeg for (macOS, Windows); darwin otherwise.
func realFFmpegOS() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return "darwin"
}

// standInInputs rewrites the camera input group of args (from -f
// avfoundation or -f dshow to and including the device string after -i)
// into a stand-in camera and lavfi's tone: lavfi's test picture of the
// same size and rate, or with camera set, that recording, read in real
// time and over and over, as a camera sends. Everything after the group,
// the connector's encoding chain, is untouched.
func standInInputs(args []string, camera string) []string {
	start := -1
	for i, a := range args {
		if a == "-f" && i+1 < len(args) && (args[i+1] == "avfoundation" || args[i+1] == "dshow") {
			start = i
			break
		}
	}
	if start < 0 {
		return args
	}
	size, rate := "1280x720", "30"
	end := len(args)
	for i := start; i+1 < len(args); i++ {
		switch args[i] {
		case "-video_size":
			size = args[i+1]
		case "-framerate":
			rate = args[i+1]
		case "-i":
			end = i + 2
		}
		if end != len(args) {
			break
		}
	}
	out := append([]string(nil), args[:start]...)
	if camera != "" {
		out = append(out, "-re", "-stream_loop", "-1", "-f", "nut", "-i", camera,
			"-re", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000")
	} else {
		out = append(out, "-f", "lavfi", "-i", "testsrc2=size="+size+":rate="+rate,
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000")
	}
	return append(out, args[end:]...)
}

func TestStandInInputs(t *testing.T) {
	cam := Device{Kind: Video, ID: "0", Name: "Cam"}
	mic := Device{Kind: Audio, ID: "1", Name: "Mic"}
	args := Args("darwin", Options{VideoSize: "640x480", FPS: 15}, Input{}, cam, &mic, EncoderVideoToolbox, "rtsp://127.0.0.1:1/x")
	got := strings.Join(standInInputs(args, ""), " ")
	want := "-hide_banner -loglevel warning -nostats -f lavfi -i testsrc2=size=640x480:rate=15 -f lavfi -i sine=frequency=440:sample_rate=48000 -c:v h264_videotoolbox"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("got  %s\nwant prefix %s", got, want)
	}
	if !strings.HasSuffix(got, "-f rtsp -rtsp_transport tcp -pkt_size 1200 rtsp://127.0.0.1:1/x") {
		t.Fatalf("tail changed: %s", got)
	}
	if strings.Contains(got, "avfoundation") || strings.Contains(got, "thread_queue_size") {
		t.Fatalf("input group not removed: %s", got)
	}
	win := Args("windows", Options{VideoSize: "1920x1080", FPS: 25}, Input{Rate: "25", Format: []string{"-vcodec", "mjpeg"}},
		Device{Kind: Video, ID: "Insta360 Link"}, &Device{Kind: Audio, ID: "Microphone (Yeti)"}, EncoderMediaFound, "rtsp://127.0.0.1:1/x")
	got = strings.Join(standInInputs(win, ""), " ")
	want = "-hide_banner -loglevel warning -nostats -f lavfi -i testsrc2=size=1920x1080:rate=25 -f lavfi -i sine=frequency=440:sample_rate=48000 -c:v h264_mf"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("windows: got  %s\nwant prefix %s", got, want)
	}
	if strings.Contains(got, "dshow") || strings.Contains(got, "rtbufsize") || strings.Contains(got, "video=") || strings.Contains(got, "-vcodec") {
		t.Fatalf("windows input group not removed: %s", got)
	}
	// A recording stands in for a camera that sends its picture compressed.
	hevc := filepath.Join("testdata", compressedCameras["hevc"])
	got = strings.Join(standInInputs(win, hevc), " ")
	want = "-hide_banner -loglevel warning -nostats -re -stream_loop -1 -f nut -i " + hevc +
		" -re -f lavfi -i sine=frequency=440:sample_rate=48000 -c:v h264_mf"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("recorded camera: got  %s\nwant prefix %s", got, want)
	}
	plain := []string{"-f", "v4l2", "-i", "/dev/video0"}
	if strings.Join(standInInputs(plain, ""), " ") != strings.Join(plain, " ") {
		t.Fatal("linux arguments changed")
	}
	for format, name := range compressedCameras {
		if st, err := os.Stat(filepath.Join("testdata", name)); err != nil || st.Size() == 0 {
			t.Errorf("recorded %s camera %s: %v", format, name, err)
		}
	}
}

func TestRealFFmpegPublishes(t *testing.T) {
	ffmpeg := os.Getenv(realFFmpegEnv)
	if ffmpeg == "" {
		t.Skipf("%s not set", realFFmpegEnv)
	}
	if _, err := os.Stat(ffmpeg); err != nil {
		t.Fatal(err)
	}
	// The system's own input and encoder (avfoundation and VideoToolbox on
	// a Mac, dshow and Media Foundation on Windows): what the bundled ffmpeg
	// for that system holds, and what the connector asks of it there.
	goos := realFFmpegOS()
	encoder := HardwareEncoder(goos)
	// Preflight: the encoders on a test picture and tone, to the null
	// output, so a build missing a component fails here with ffmpeg's own
	// words rather than as a timeout below.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	preArgs := []string{"-hide_banner", "-loglevel", "error", "-nostats",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "1"}
	preArgs = append(preArgs, encoderArgs(encoder, "3.1")...)
	preArgs = append(preArgs, "-pix_fmt", "yuv420p", "-color_range", "tv", "-c:a", "libopus", "-ac", "1", "-ar", "48000", "-f", "null", "-")
	pre := exec.CommandContext(ctx, ffmpeg, preArgs...)
	if out, err := pre.CombinedOutput(); err != nil {
		t.Fatalf("%s cannot encode the test source with %s: %v\n%s", ffmpeg, encoder, err, out)
	}

	t.Setenv("CAPTURE_FAKE_FFMPEG", "")
	t.Setenv(realFFmpegEnv+"_WRAP", ffmpeg)
	t.Run("raw", func(t *testing.T) { publishesThroughRealFFmpeg(t, goos, encoder, "") })
	if goos != "windows" {
		return
	}
	// dshow hands over what a camera sends, compressed into one of these
	// for most: each must reach the encoder as frames.
	for _, format := range []string{"mjpeg", "h264", "hevc"} {
		camera, err := filepath.Abs(filepath.Join("testdata", compressedCameras[format]))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(format, func(t *testing.T) { publishesThroughRealFFmpeg(t, goos, encoder, camera) })
	}
}

// publishesThroughRealFFmpeg starts a Source on the real ffmpeg (through
// the wrapper, with camera standing in when set) and reads its stream back.
func publishesThroughRealFFmpeg(t *testing.T, goos, encoder, camera string) {
	t.Setenv(realFFmpegCameraEnv, camera)
	srv, src := testSourceOn(t, goos, Options{}, os.Args[0])
	src.Start()
	waitFor(t, "the real ffmpeg to publish", src.Publishing)
	c, n := play(t, srv)
	defer c.Close()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && n.n.Load() < 60 {
		time.Sleep(50 * time.Millisecond)
	}
	if n.n.Load() < 60 {
		t.Fatalf("read %d packets from the real ffmpeg in 30 s", n.n.Load())
	}
	if !src.Publishing() || !srv.Stats().Publishing {
		t.Fatal("not publishing")
	}
	if src.Encoder() != encoder {
		t.Fatalf("encoder %s: %s failed to start and the fallback took over", src.Encoder(), encoder)
	}
	// A real H.264 stream carries a parameter set or an IDR before long;
	// the payload must not be the fake's marker.
	if v := n.lastVideo.Load(); v == nil || len(*v) == 0 {
		t.Fatal("no video payload")
	}
	src.Stop()
	if src.Running() {
		t.Fatal("running after Stop")
	}
	waitFor(t, "publication end", func() bool { return !src.Publishing() })
	t.Logf("real ffmpeg published %d packets through the connector's own argument shape", n.n.Load())
}

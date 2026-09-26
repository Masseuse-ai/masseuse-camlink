package capture

import (
	"context"
	"os"
	"os/exec"
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
// swaps the camera and microphone for lavfi's test picture and tone, and
// execs the real program (realFFmpeg, in the TestMain hook).

// realFFmpegEnv names the ffmpeg to test; unset, the test is skipped.
const realFFmpegEnv = "CAPTURE_REAL_FFMPEG"

// realFFmpegOS is the system whose input and encoder the real-ffmpeg test
// exercises: the one it runs on, when that is a system the connector has a
// bundled ffmpeg for (macOS, Windows); darwin otherwise.
func realFFmpegOS() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return "darwin"
}

// lavfiInputs rewrites the camera input group of args (from -f avfoundation
// or -f dshow to and including the device string after -i) into two lavfi
// inputs of the same size and rate; everything after it, the connector's
// encoding chain, is untouched.
func lavfiInputs(args []string) []string {
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
	out = append(out, "-f", "lavfi", "-i", "testsrc2=size="+size+":rate="+rate,
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000")
	return append(out, args[end:]...)
}

func TestLavfiInputs(t *testing.T) {
	cam := Device{Kind: Video, ID: "0", Name: "Cam"}
	mic := Device{Kind: Audio, ID: "1", Name: "Mic"}
	args := Args("darwin", Options{VideoSize: "640x480", FPS: 15}, cam, &mic, EncoderVideoToolbox, "rtsp://127.0.0.1:1/x")
	got := strings.Join(lavfiInputs(args), " ")
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
	win := Args("windows", Options{VideoSize: "1920x1080", FPS: 25}, Device{Kind: Video, ID: "Insta360 Link"},
		&Device{Kind: Audio, ID: "Microphone (Yeti)"}, EncoderMediaFound, "rtsp://127.0.0.1:1/x")
	got = strings.Join(lavfiInputs(win), " ")
	want = "-hide_banner -loglevel warning -nostats -f lavfi -i testsrc2=size=1920x1080:rate=25 -f lavfi -i sine=frequency=440:sample_rate=48000 -c:v h264_mf"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("windows: got  %s\nwant prefix %s", got, want)
	}
	if strings.Contains(got, "dshow") || strings.Contains(got, "rtbufsize") || strings.Contains(got, "video=") {
		t.Fatalf("windows input group not removed: %s", got)
	}
	plain := []string{"-f", "v4l2", "-i", "/dev/video0"}
	if strings.Join(lavfiInputs(plain), " ") != strings.Join(plain, " ") {
		t.Fatal("linux arguments changed")
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
	preArgs = append(preArgs, "-pix_fmt", "yuv420p", "-c:a", "libopus", "-ac", "1", "-ar", "48000", "-f", "null", "-")
	pre := exec.CommandContext(ctx, ffmpeg, preArgs...)
	if out, err := pre.CombinedOutput(); err != nil {
		t.Fatalf("%s cannot encode the test source with %s: %v\n%s", ffmpeg, encoder, err, out)
	}

	t.Setenv("CAPTURE_FAKE_FFMPEG", "")
	t.Setenv(realFFmpegEnv+"_WRAP", ffmpeg)
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

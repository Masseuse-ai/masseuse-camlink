package capture

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakePreview is the fake ffmpeg asked for the preview (previewArgs): an
// mpjpeg stream of small stand-in pictures on standard output, PreviewFPS a
// second, until a q on standard input. With CAPTURE_FAKE_PREVIEW_PICTURES=n
// it ends after n, as an ffmpeg whose camera was unplugged does; with
// CAPTURE_FAKE_NO_MJPEG=1 it is a build without the JPEG encoder.
func fakePreview() int {
	if os.Getenv("CAPTURE_FAKE_NO_MJPEG") == "1" {
		fmt.Fprintln(os.Stderr, "[vost#0:0 @ 0x1] Unknown encoder 'mjpeg'")
		return 8
	}
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
	limit, _ := strconv.Atoi(os.Getenv("CAPTURE_FAKE_PREVIEW_PICTURES"))
	w := bufio.NewWriter(os.Stdout)
	fmt.Fprint(w, "--ffmpeg\r\n")
	tick := time.NewTicker(time.Second / PreviewFPS)
	defer tick.Stop()
	for n := 1; ; n++ {
		select {
		case <-quit:
			return 0
		case <-tick.C:
		}
		pic := []byte{0xFF, 0xD8, 'p', byte(n), 0xFF, 0xD9}
		fmt.Fprintf(w, "Content-type: image/jpeg\r\nContent-length: %d\r\n\r\n", len(pic))
		_, _ = w.Write(pic)
		fmt.Fprint(w, "\r\n--ffmpeg\r\n")
		if err := w.Flush(); err != nil {
			return 1
		}
		if limit > 0 && n >= limit {
			fmt.Fprintln(os.Stderr, "[in#0/avfoundation @ 0x1] Error during demuxing: Input/output error")
			return 1
		}
	}
}

func TestPreviewArgs(t *testing.T) {
	join := func(a []string) string { return strings.Join(a, " ") }
	const tail = " -an -vf fps=8,scale=w='min(640,iw)':h='min(640,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2 -c:v mjpeg -q:v 7 -f mpjpeg pipe:1"
	// On a Mac: the camera by name in its mode, as a session's capture
	// opens it, and the microphone not at all.
	cam := Device{Kind: Video, ID: "1", Name: "FaceTime HD Camera", Input: "FaceTime HD Camera"}
	opts := Options{VideoSize: "1552x1552", FPS: 30}
	in := Input{Rate: "30.00", Format: nil}
	got := join(previewArgs("darwin", opts, in, cam))
	want := "-hide_banner -loglevel warning -nostats -f avfoundation -framerate 30.00 -video_size 1552x1552 -thread_queue_size 512 -i FaceTime HD Camera" + tail
	if got != want {
		t.Fatalf("darwin:\n got %s\nwant %s", got, want)
	}
	mic := Device{Kind: Audio, ID: "0", Name: "MacBook Pro Microphone", Input: "MacBook Pro Microphone"}
	session := join(Args("darwin", opts, in, cam, &mic, EncoderVideoToolbox, "rtsp://127.0.0.1:1/x"))
	if !strings.Contains(session, "-f avfoundation -framerate 30.00 -video_size 1552x1552 -thread_queue_size 512 -i FaceTime HD Camera:MacBook Pro Microphone ") {
		t.Fatalf("the session opens the camera otherwise: %s", session)
	}
	// On Windows: a 4K camera sending MJPEG, decoded at a quarter of its
	// size before it is scaled.
	link := Device{Kind: Video, ID: "Insta360 Link", Name: "Insta360 Link"}
	got = join(previewArgs("windows", Options{VideoSize: "3840x2160", FPS: 30}, Input{Format: []string{"-vcodec", "mjpeg"}}, link))
	want = "-hide_banner -loglevel warning -nostats -lowres 2 -f dshow -rtbufsize 100M -framerate 30 -video_size 3840x2160 -vcodec mjpeg -thread_queue_size 512 -i video=Insta360 Link" + tail
	if got != want {
		t.Fatalf("windows:\n got %s\nwant %s", got, want)
	}
	// A raw mode is decoded as it is.
	got = join(previewArgs("windows", Options{VideoSize: "1280x720", FPS: 30}, Input{Format: []string{"-pixel_format", "yuyv422"}}, link))
	if strings.Contains(got, "-lowres") || !strings.Contains(got, "-pixel_format yuyv422 -thread_queue_size 512 -i video=Insta360 Link -an ") {
		t.Fatalf("windows raw: %s", got)
	}
	got = join(previewArgs("linux", Options{VideoSize: "1280x720", FPS: 30}, Input{}, Device{Kind: Video, ID: "/dev/video0", Name: "C920"}))
	want = "-hide_banner -loglevel warning -nostats -f v4l2 -framerate 30 -video_size 1280x720 -thread_queue_size 512 -i /dev/video0" + tail
	if got != want {
		t.Fatalf("linux:\n got %s\nwant %s", got, want)
	}
}

func TestPreviewLowres(t *testing.T) {
	mjpeg := Input{Format: []string{"-vcodec", "mjpeg"}}
	for _, tc := range []struct {
		size string
		in   Input
		want int
	}{
		{"1280x720", mjpeg, 1},
		{"1920x1080", mjpeg, 1},
		{"3840x2160", mjpeg, 2},
		{"2160x3840", mjpeg, 2},
		{"7680x4320", mjpeg, 3},
		{"15360x8640", mjpeg, 3}, // the decoder halves three times at most
		{"640x480", mjpeg, 0},
		{"3840x2160", Input{Format: []string{"-vcodec", "h264"}}, 0},
		{"3840x2160", Input{Format: []string{"-pixel_format", "nv12"}}, 0},
		{"3840x2160", Input{}, 0},
		{"wide", mjpeg, 0},
	} {
		if got := previewLowres(tc.size, tc.in); got != tc.want {
			t.Errorf("%s %v: %d, want %d", tc.size, tc.in.Format, got, tc.want)
		}
	}
}

func TestReadMPJPEG(t *testing.T) {
	pic := func(n byte) []byte { return []byte{0xFF, 0xD8, n, '\r', '\n', '\r', '\n', 0xFF, 0xD9} }
	part := func(b []byte) string {
		return fmt.Sprintf("Content-type: image/jpeg\r\nContent-length: %d\r\n\r\n%s\r\n--ffmpeg\r\n", len(b), b)
	}
	stream := "--ffmpeg\r\n" + part(pic(1)) + part(pic(2)) + part(pic(3))
	read := func(s string) ([][]byte, error) {
		var got [][]byte
		err := readMPJPEG(strings.NewReader(s), func(b []byte) { got = append(got, b) })
		return got, err
	}
	// Each picture by its length, whatever its bytes hold (here line ends),
	// in a slice of its own.
	got, err := read(stream)
	if err != nil || len(got) != 3 {
		t.Fatalf("%d pictures, %v", len(got), err)
	}
	for i, b := range got {
		if !bytes.Equal(b, pic(byte(i+1))) {
			t.Fatalf("picture %d: %x", i, b)
		}
	}
	// Cut short by ffmpeg stopping: the pictures before, and no error.
	got, err = read(stream + "Content-type: image/jpeg\r\nContent-length: 100\r\n\r\n\xFF\xD8abc")
	if err != nil || len(got) != 3 {
		t.Fatalf("cut short: %d pictures, %v", len(got), err)
	}
	// A part that is not a JPEG is left out.
	got, err = read("--ffmpeg\r\n" + part([]byte("not a picture")) + part(pic(4)))
	if err != nil || len(got) != 1 || got[0][2] != 4 {
		t.Fatalf("not a JPEG: %d pictures, %v", len(got), err)
	}
	// Lengths that are not ffmpeg's end the stream.
	for _, bad := range []string{"x", "0", "-5", strconv.Itoa(maxPreviewJPEG + 1)} {
		if _, err := read("--ffmpeg\r\nContent-type: image/jpeg\r\nContent-length: " + bad + "\r\n\r\n"); err == nil {
			t.Errorf("length %q: no error", bad)
		}
	}
}

// previewSource is a darwin Source on the fake ffmpeg, its graces short.
func previewSource(t *testing.T, goos string) *Source {
	t.Helper()
	t.Setenv("CAPTURE_FAKE_FFMPEG", "1")
	_, src := testSourceOn(t, goos, Options{}, os.Args[0])
	src.announceGrace = 2 * time.Second
	src.stopGrace = 500 * time.Millisecond
	return src
}

// TestSourcePreviewHandsOverPictures: the pictures reach frame as they
// come, the session's capture stays off, and ending ctx stops ffmpeg with
// its q, well within the stop grace (long here: a race-instrumented fake
// takes a second to exit).
func TestSourcePreviewHandsOverPictures(t *testing.T) {
	src := previewSource(t, "darwin")
	src.stopGrace = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var n atomic.Int64
	var first atomic.Value
	done := make(chan error, 1)
	go func() {
		done <- src.Preview(ctx, func(b []byte) {
			if n.Add(1) == 1 {
				first.Store(b)
			}
		})
	}()
	waitFor(t, "pictures", func() bool { return n.Load() >= 3 })
	if b, _ := first.Load().([]byte); len(b) != 6 || b[0] != 0xFF || b[1] != 0xD8 || b[3] != 1 {
		t.Fatalf("first picture %x", b)
	}
	if src.Running() || src.Publishing() {
		t.Fatal("the preview turned the session's capture on")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Preview: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Preview went on after its context ended")
	}
	if d := time.Since(start); d >= src.stopGrace/2 {
		t.Fatalf("stopped in %s: the q was not heard", d)
	}
}

// TestSourcePreviewSaysWhyItEnded: a camera that sends nothing (on a Mac,
// the one the system refuses), one that cannot be opened, one unplugged
// mid-preview and an ffmpeg without the JPEG encoder each end the preview
// with their own reason for the person.
func TestSourcePreviewSaysWhyItEnded(t *testing.T) {
	for _, tc := range []struct {
		name, env, value string
		goos             string
		reason           func(src *Source) string
		pictures         bool
	}{
		{"stalled on a Mac", "CAPTURE_FAKE_STALL", "1", "darwin", func(src *Source) string { return stallTrouble("darwin", src.announceGrace) }, false},
		{"stalled on Windows", "CAPTURE_FAKE_STALL", "1", "windows", func(src *Source) string { return stallTrouble("windows", src.announceGrace) }, false},
		{"not opened", "CAPTURE_FAKE_BAD_INPUT", "0", "darwin", func(*Source) string {
			return "the camera could not be opened; another program may have it, or it was unplugged"
		}, false},
		{"unplugged", "CAPTURE_FAKE_PREVIEW_PICTURES", "3", "darwin", func(*Source) string { return "the camera stopped sending its picture" }, true},
		{"no JPEG encoder", "CAPTURE_FAKE_NO_MJPEG", "1", "darwin", func(*Source) string { return "this ffmpeg cannot make the preview: it has no JPEG output" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := previewSource(t, tc.goos)
			t.Setenv(tc.env, tc.value)
			src.announceGrace = 300 * time.Millisecond
			var n atomic.Int64
			start := time.Now()
			err := src.Preview(context.Background(), func([]byte) { n.Add(1) })
			var pe *PreviewError
			if !errors.As(err, &pe) || pe.Reason != tc.reason(src) {
				t.Fatalf("Preview: %v", err)
			}
			if got := n.Load() > 0; got != tc.pictures {
				t.Fatalf("%d pictures", n.Load())
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("took %s", d)
			}
		})
	}
}

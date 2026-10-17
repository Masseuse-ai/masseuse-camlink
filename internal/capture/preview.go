package capture

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The window's preview: while no session has the camera, the desktop
// window's Ready screen shows the person its picture, so they can place it
// before the session starts. It is an ffmpeg of its own (previewArgs): the
// camera alone, opened in the session's mode, so the picture is the one a
// session would be sent, as small JPEGs on ffmpeg's standard output.
// Preview hands each to its caller and keeps none; nothing of it reaches
// the stream's server, and so the tunnel, or the disk.

// maxPreviewJPEG bounds one picture: a part that announces more is not a
// preview picture (one PreviewSize wide at previewArgs' quality is tens of
// kilobytes).
const maxPreviewJPEG = 4 << 20

// A PreviewError is why the preview ended on its own. Reason is for the
// person, a sentence without its stop, as Trouble is for a session; Err is
// what ffmpeg said, when it said anything.
type PreviewError struct {
	Reason string
	Err    error
}

func (e *PreviewError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *PreviewError) Unwrap() error { return e.Err }

// Preview shows the camera to frame until ctx ends: ffmpeg is started on
// the camera in use, in its mode, and each picture is handed to frame as
// JPEG bytes as soon as it is read, from one goroutine and in order. frame
// may keep the slice and must return promptly: a slow frame holds ffmpeg
// up. Once ctx ends ffmpeg is asked to quit, and killed after the stop
// grace, and Preview returns ctx's error. It returns a *PreviewError when
// no picture comes for the announce grace (the camera refused, or gone
// quiet) or ffmpeg ends on its own. The caller keeps the preview and a
// session's capture (Start) apart: one camera, one ffmpeg at a time.
func (s *Source) Preview(ctx context.Context, frame func(jpeg []byte)) error {
	s.mu.Lock()
	goos, ffmpeg, opts, in, cam := s.goos, s.ffmpeg, s.opts, s.input, s.cam
	grace, stopGrace := s.announceGrace, s.stopGrace
	s.mu.Unlock()
	args := previewArgs(goos, opts, in, cam)
	cmd := exec.Command(ffmpeg, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	s.log.Debug("capture: starting the preview", "args", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return &PreviewError{Reason: "ffmpeg could not be started", Err: err}
	}
	tail := &tailBuffer{n: 12}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			tail.add(sc.Text())
			s.log.Debug("ffmpeg (preview)", "line", sc.Text())
		}
	}()
	// last is when the last picture came (Unix nanoseconds), or ffmpeg
	// started until one has; pictures counts them.
	var last, pictures atomic.Int64
	last.Store(time.Now().UnixNano())
	readDone := make(chan error, 1)
	go func() {
		readDone <- readMPJPEG(stdout, func(b []byte) {
			last.Store(time.Now().UnixNano())
			pictures.Add(1)
			frame(b)
		})
	}()
	watch := time.NewTicker(max(grace/10, 10*time.Millisecond))
	defer watch.Stop()
	var (
		ended   error // ctx's error or the stall, when either ended the preview
		readErr error
		read    bool // the stream ended: ffmpeg is gone or going
	)
loop:
	for {
		select {
		case <-ctx.Done():
			ended = ctx.Err()
			break loop
		case readErr = <-readDone:
			read = true
			break loop
		case <-watch.C:
			if time.Since(time.Unix(0, last.Load())) >= grace {
				s.log.Warn("capture: the preview's camera delivered no picture", "camera", cam.Name, "after", grace.String())
				ended = &PreviewError{Reason: stallTrouble(goos, grace)}
				break loop
			}
		}
	}
	// ffmpeg ends here, whatever ended the preview: asked to quit ("q" on
	// standard input, on every system), then killed once the stop grace has
	// passed. A stream that could not be read leaves ffmpeg running: it is
	// killed outright.
	switch {
	case !read:
		_, _ = io.WriteString(stdin, "q\n")
		_ = stdin.Close()
		select {
		case readErr = <-readDone:
		case <-time.After(stopGrace):
			_ = cmd.Process.Kill()
			readErr = <-readDone
		}
	case readErr != nil:
		_ = cmd.Process.Kill()
	}
	_ = stdin.Close()
	<-stderrDone
	waitErr := cmd.Wait()
	if ended != nil {
		return ended
	}
	if readErr != nil {
		return &PreviewError{Reason: "the preview's pictures could not be read", Err: readErr}
	}
	return previewExit(fmt.Errorf("ffmpeg: %v: %s", waitErr, tail.String()), pictures.Load() > 0)
}

// previewExit is why an ffmpeg that ended on its own ended the preview,
// from what it wrote (classifyExit); sent says pictures had come before.
func previewExit(err error, sent bool) *PreviewError {
	msg := strings.ToLower(err.Error())
	switch kind := classifyExit(err); {
	case strings.Contains(msg, "mjpeg") && (kind == exitOption || strings.Contains(msg, "not known")),
		strings.Contains(msg, "mpjpeg"), strings.Contains(msg, "protocol not found"):
		return &PreviewError{Reason: "this ffmpeg cannot make the preview: it has no JPEG output", Err: err}
	case kind == exitInput:
		return &PreviewError{Reason: "the camera could not be opened; another program may have it, or it was unplugged", Err: err}
	case kind == exitDecoder:
		return &PreviewError{Reason: "the camera sends its picture as " + undecodable(err) + ", which this ffmpeg cannot decode", Err: err}
	case sent:
		return &PreviewError{Reason: "the camera stopped sending its picture", Err: err}
	default:
		return &PreviewError{Reason: "ffmpeg stopped as soon as it started", Err: err}
	}
}

// readMPJPEG reads ffmpeg's mpjpeg stream (a boundary line, then for each
// picture its headers, a blank line, Content-length bytes and the next
// boundary) and hands each picture to frame, in a slice of its own. It
// returns nil at the stream's end, a picture cut short by it included (an
// ffmpeg stopped mid-write), and an error for a length that is not a
// number or over maxPreviewJPEG. A part that is not a JPEG is left out.
func readMPJPEG(r io.Reader, frame func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	length := -1
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			if v, ok := headerValue(line, "content-length"); ok {
				n, err := strconv.Atoi(v)
				if err != nil || n <= 0 || n > maxPreviewJPEG {
					return fmt.Errorf("a preview picture of length %q", v)
				}
				length = n
			}
			continue
		}
		if length < 0 {
			// The line that ends a picture's bytes.
			continue
		}
		b := make([]byte, length)
		length = -1
		if _, err := io.ReadFull(br, b); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xD8 {
			frame(b)
		}
	}
}

// headerValue is the value of the header line when it names name, case
// aside.
func headerValue(line []byte, name string) (string, bool) {
	k, v, ok := bytes.Cut(line, []byte(":"))
	if !ok || !strings.EqualFold(string(bytes.TrimSpace(k)), name) {
		return "", false
	}
	return string(bytes.TrimSpace(v)), true
}

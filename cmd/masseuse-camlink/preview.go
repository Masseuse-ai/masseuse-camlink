package main

import (
	"context"
	"errors"
	"time"

	"github.com/Masseuse-ai/masseuse-camlink/internal/capture"
)

// The window's preview of the camera behind the person: while no session
// has it, the desktop window's Ready screen asks for it every few seconds
// (the ipc preview command) and shows its pictures, so the person can place
// the camera before the session starts. It is the camera the session would
// use, in its mode (capture.Source.Preview), video alone; its pictures go
// to the window over standard output (the preview_frame events) and
// nowhere else. A session's first stream takes the camera from it
// (dialLocal), and the window's switch, a change of camera or the program
// ending stop it; so does the window no longer asking (previewLease).

// previewLease is how long a preview runs after the window's last ask: the
// window asks every few seconds while it shows the preview, so one hidden,
// closed or gone lets the camera go within this.
const previewLease = 10 * time.Second

// previewRetry is the first wait before a preview that ended on its own
// (no picture, the camera taken by another program) is tried again while
// the window still asks; it doubles up to previewRetryMax.
const (
	previewRetry    = 5 * time.Second
	previewRetryMax = 30 * time.Second
)

// previewState is the preview's standing as the window is told it.
type previewState string

const (
	// previewStarting: ffmpeg is opening the camera; no picture yet.
	previewStarting previewState = "starting"
	// previewLive: pictures are coming.
	previewLive previewState = "live"
	// previewOff: no preview runs.
	previewOff previewState = "off"
	// previewUnavailable: the preview cannot run, or ended; the note says
	// why, for the person.
	previewUnavailable previewState = "unavailable"
)

// previewRun is one preview while it runs: cancel ends it, done is closed
// once its ffmpeg is gone, lease ends it when the window stops asking.
type previewRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	lease  *time.Timer
}

// stop ends the run and waits until its ffmpeg is gone.
func (p *previewRun) stop() {
	p.cancel()
	<-p.done
}

// preview is the window's ask: on starts the preview, or renews its lease,
// unless a session has the camera, the window's switch has it off, or the
// camera is not one of the computer's own; off stops it. What comes of it
// is said as Preview events.
func (c *camControl) preview(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !on {
		c.stopPreviewLocked()
		return
	}
	if note := c.previewRefusalLocked(); note != "" {
		c.stopPreviewLocked()
		c.reporter().Preview(previewUnavailable, note, "", "")
		return
	}
	lease := c.lease
	if lease == 0 {
		lease = previewLease
	}
	if c.prev != nil {
		c.prev.lease.Reset(lease)
		return
	}
	retry := c.retry
	if retry == 0 {
		retry = previewRetry
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &previewRun{cancel: cancel, done: make(chan struct{})}
	run.lease = time.AfterFunc(lease, func() { c.previewLapsed(run) })
	c.prev = run
	go c.runPreview(ctx, run, c.offer, retry)
}

// previewRefusalLocked is why the preview cannot run now, for the person,
// or "" when it can; the caller holds c.mu.
func (c *camControl) previewRefusalLocked() string {
	switch {
	case c.on:
		return "a session has the camera"
	case c.cameraOff:
		return "the camera is switched off"
	case c.offer == nil:
		return "no camera is set up"
	case !c.offer.ready:
		return c.offer.note
	case c.offer.preview == nil:
		return "a camera on your network shows its picture in its own app"
	}
	return ""
}

// stopPreviewLocked ends the preview, if one runs, and waits until its
// ffmpeg is gone; the caller holds c.mu, which the preview's goroutine
// never takes.
func (c *camControl) stopPreviewLocked() {
	p := c.prev
	if p == nil {
		return
	}
	c.prev = nil
	p.lease.Stop()
	p.stop()
}

// previewLapsed is a preview's lease running out: the window stopped
// asking (hidden, closed, or showing something else).
func (c *camControl) previewLapsed(run *previewRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prev != run {
		return
	}
	c.log.Info("camera: the window stopped asking for the preview; turning it off")
	c.stopPreviewLocked()
}

// runPreview runs one preview until it is stopped: the camera's pictures
// to the window and, when ffmpeg ends on its own, why, and another try
// after wait (doubling up to previewRetryMax) while the window still asks.
// It never takes c.mu.
func (c *camControl) runPreview(ctx context.Context, run *previewRun, o *offer, wait time.Duration) {
	defer close(run.done)
	ui := c.reporter()
	camera := o.save.Camera
	for {
		ui.Preview(previewStarting, "", camera, o.size)
		c.log.Info("camera: preview on", "camera", camera)
		live := false
		err := o.preview(ctx, func(b []byte) {
			if !live {
				live = true
				ui.Preview(previewLive, "", camera, o.size)
			}
			ui.PreviewFrame(b)
		})
		if ctx.Err() != nil {
			c.log.Info("camera: preview off", "camera", camera)
			ui.Preview(previewOff, "", camera, o.size)
			return
		}
		note := err.Error()
		var pe *capture.PreviewError
		if errors.As(err, &pe) {
			note = pe.Reason
		}
		c.log.Warn("camera: the preview ended; trying again while the window asks", "camera", camera, "err", err, "in", wait.String())
		ui.Preview(previewUnavailable, note, camera, o.size)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, previewRetryMax)
	}
}

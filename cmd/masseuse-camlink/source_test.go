package main

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
	"github.com/FemLed/masseuse-camlink/internal/serve"
)

// fakeOffer is a ready offer whose start and stop are counted.
type fakeOffer struct {
	offer
	starts, stops atomic.Int32
}

func newFakeOffer() *fakeOffer {
	f := &fakeOffer{}
	f.offer = offer{kind: "capture", label: "Test Camera", ready: true, publishing: func() bool { return false }}
	f.offer.start = func() { f.starts.Add(1) }
	f.offer.stop = func() { f.stops.Add(1) }
	return f
}

// open is the enclave opening a stream to the connector's own camera.
func (c *camControl) open(t *testing.T) {
	t.Helper()
	conn, err := c.dialLocal()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestCameraStaysOnForTheNextTunnel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink, err := serve.New(serve.Config{StateDir: t.TempDir(), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	f := newFakeOffer()
	out := &console{}
	cam := &camControl{sink: sink, offer: &f.offer, log: logger, out: out, grace: 300 * time.Millisecond}

	// A stream turns the camera on; the tunnel then ends with the session
	// still live: the camera stays on for the grace.
	cam.open(t)
	cam.off(true)
	time.Sleep(150 * time.Millisecond)
	if f.starts.Load() != 1 || f.stops.Load() != 0 || out.count("Camera off.") != 0 {
		t.Fatalf("camera did not stay on: starts %d stops %d\n%s", f.starts.Load(), f.stops.Load(), out.String())
	}
	// The next tunnel opens a stream in time: the camera stays on, the
	// grace is forgotten, nothing restarted.
	cam.open(t)
	time.Sleep(400 * time.Millisecond)
	if f.starts.Load() != 1 || f.stops.Load() != 0 {
		t.Fatalf("camera went off although a stream came within the grace: starts %d stops %d", f.starts.Load(), f.stops.Load())
	}
	// The tunnel ends again and nothing comes: off once the grace passes.
	cam.off(true)
	cam.off(true) // a second ending does not restart the grace
	if f.stops.Load() != 0 {
		t.Fatal("camera went off at once")
	}
	waitUntil(t, "the camera to go off", func() bool { return f.stops.Load() == 1 })
	if out.count("Camera off.") != 1 || out.count("Camera on: Test Camera.") != 1 {
		t.Fatalf("console:\n%s", out.String())
	}
	// On again, then the session ends: off at once, and a later grace
	// ending changes nothing.
	cam.open(t)
	cam.off(false)
	if f.starts.Load() != 2 || f.stops.Load() != 2 {
		t.Fatalf("session end: starts %d stops %d", f.starts.Load(), f.stops.Load())
	}
	cam.off(true)
	cam.off(false)
	time.Sleep(400 * time.Millisecond)
	if f.stops.Load() != 2 || out.count("Camera off.") != 2 {
		t.Fatalf("off while off: stops %d\n%s", f.stops.Load(), out.String())
	}
}

func TestADroppedTunnelKeepsTheCameraAndClearTurnsItOff(t *testing.T) {
	h := newHarness(t)
	w := newWire(t, h.srv.Listener.Addr().String())
	f := newFakeOffer()
	h.mgr.cam.offer = &f.offer
	// Longer than the 2 s the connector waits before re-dialing after a
	// drop, shorter than the 15 s of the real grace so the test is quick.
	h.mgr.cam.grace = 4 * time.Second

	ticket, hash := h.ticket(t)
	h.expect(t, hash)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s3", Origin: w.origin, Ticket: ticket, TicketHash: hash})
	waitUntil(t, "attach", func() bool { return h.gw.Status().Connected })
	h.mgr.cam.open(t) // the enclave reads the camera through this tunnel

	// The service leases again: a new ticket for the same session. The
	// tunnel and the camera are untouched.
	ticket2, hash2 := h.ticket(t)
	h.expect(t, hash2)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s3", Origin: w.origin, Ticket: ticket2, TicketHash: hash2})
	time.Sleep(300 * time.Millisecond)
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dialed %d times for a new ticket", n)
	}
	if f.starts.Load() != 1 || f.stops.Load() != 0 {
		t.Fatalf("across the new ticket: starts %d stops %d", f.starts.Load(), f.stops.Load())
	}

	// The network drops the tunnel: the connector re-dials (with the new
	// ticket) and the camera stays on through the change.
	w.cut()
	waitUntil(t, "second attach", func() bool { return h.gw.Status().Connected && h.dials.Load() == 2 })
	if f.starts.Load() != 1 || f.stops.Load() != 0 {
		t.Fatalf("across the re-dial: starts %d stops %d", f.starts.Load(), f.stops.Load())
	}
	h.mgr.cam.open(t) // the relay is back at the stream through the new tunnel
	time.Sleep(4500 * time.Millisecond)
	if f.starts.Load() != 1 || f.stops.Load() != 0 {
		t.Fatalf("after the grace, with the new tunnel reading: starts %d stops %d", f.starts.Load(), f.stops.Load())
	}

	// The session ends: the camera goes off at once, not after the grace.
	h.mgr.OnClear("s3", "ended")
	waitUntil(t, "the camera to go off", func() bool { return f.stops.Load() == 1 })
	waitUntil(t, "detach", func() bool { return !h.gw.Status().Connected })
	if h.out.count("Camera off.") != 1 || h.out.count("Camera link closed.") != 1 {
		t.Fatalf("console:\n%s", h.out.String())
	}
}

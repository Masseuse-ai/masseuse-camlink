package share

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

var tcp = gortsplib.ProtocolTCP

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func videoDesc() *description.Session {
	return &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
	}}
}

// read plays url the way OBS's Media Source does: plain RTSP over TCP.
func read(url string) (*gortsplib.Client, *atomic.Int64, error) {
	var n atomic.Int64
	c := &gortsplib.Client{Scheme: "rtsp", ReadTimeout: 5 * time.Second, Protocol: &tcp}
	u, err := base.ParseURL(url)
	if err != nil {
		return nil, nil, err
	}
	c.Host = u.Host
	if err := c.Start(); err != nil {
		return nil, nil, err
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		c.Close()
		return nil, nil, err
	}
	c.OnPacketRTPAny(func(*description.Media, format.Format, *rtp.Packet) { n.Add(1) })
	if _, err := c.Play(nil); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, &n, nil
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTheSecretIsFreshAndTheAddressCarriesIt(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSecret()
	if a == b || len(a) != 24 || strings.ContainsAny(a, "+/=") {
		t.Fatalf("secrets %q %q", a, b)
	}
	if _, err := New(Config{Port: -1, Logger: quiet()}); err == nil {
		t.Fatal("a server with no secret")
	}
	s, err := New(Config{Port: -1, Secret: a, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.HasPrefix(s.URL(), "rtsp://127.0.0.1:") || !strings.HasSuffix(s.URL(), "/phone-"+a) {
		t.Fatalf("url %s", s.URL())
	}
}

func TestTheArrivingPictureIsServedToReadersAndToNothingElse(t *testing.T) {
	var changes atomic.Int64
	secret, _ := NewSecret()
	s, err := New(Config{Port: -1, Secret: secret, Logger: quiet(), OnChange: func(bool) { changes.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Nothing arriving: the secret path is not found, and the wrong path
	// never is.
	if _, _, err := read(s.URL()); err == nil {
		t.Fatal("read with nothing arriving")
	}
	desc := videoDesc()
	rec, err := s.Publish(desc)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Arriving() {
		t.Fatal("not arriving")
	}
	if _, err := s.Publish(videoDesc()); err != ErrBusy {
		t.Fatalf("second publish: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var seq uint16
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			seq++
			pkt := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 96}, Payload: []byte{0x65, 1}}
			_ = rec.WritePacketRTPWithNTP(desc.Medias[0], pkt, time.Time{})
		}
	}()
	c, n, err := read(s.URL())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer c.Close()
	waitFor(t, "packets", func() bool { return n.Load() >= 10 })
	waitFor(t, "reader counted", func() bool { return s.Readers() == 1 })
	wrong := strings.TrimSuffix(s.URL(), secret) + "guess"
	if _, _, err := read(wrong); err == nil {
		t.Fatal("a wrong secret was served")
	}
	// Nobody publishes into the share server itself.
	pub := &gortsplib.Client{Scheme: "rtsp", Protocol: &tcp, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
	if err := pub.StartRecording(s.URL(), videoDesc()); err == nil {
		pub.Close()
		t.Fatal("a publisher was taken")
	}
	// The reception ending disconnects the reader and the path is gone
	// until the next picture.
	rec.Close()
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader survived the picture stopping")
	}
	waitFor(t, "reader gone", func() bool { return s.Readers() == 0 && !s.Arriving() })
	waitFor(t, "changes said", func() bool { return changes.Load() == 2 })
	if _, _, err := read(s.URL()); err == nil {
		t.Fatal("read after the picture stopped")
	}
}

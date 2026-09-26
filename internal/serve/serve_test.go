package serve

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// sampleDesc is a video+audio description like the one ffmpeg announces.
func sampleDesc() *description.Session {
	return &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 97, ChannelCount: 1}}},
	}}
}

// pump writes a packet per media every 20 ms until ctx ends.
func pump(ctx context.Context, t *testing.T, pub *Publication, desc *description.Session) {
	t.Helper()
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
		for _, m := range desc.Medias {
			pt := m.Formats[0].PayloadType()
			pkt := &rtp.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, PayloadType: pt, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: uint32(pt)},
				Payload: []byte{0x65, 1, 2, 3},
			}
			if err := pub.WritePacketRTP(m, pkt); err != nil {
				return
			}
		}
	}
}

// reader plays the stream over conns from dial and counts packets by kind,
// keeping the video packets' headers to check continuity.
type reader struct {
	c            *gortsplib.Client
	video, audio atomic.Int64
	leaf         string

	mu      sync.Mutex
	headers []rtp.Header
}

func (r *reader) videoHeaders() []rtp.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rtp.Header(nil), r.headers...)
}

func play(t *testing.T, dial func() (net.Conn, error)) (*reader, error) {
	t.Helper()
	r := &reader{}
	r.c = &gortsplib.Client{
		Scheme:      "rtsps",
		Host:        Target,
		ReadTimeout: 12 * time.Second,
		Protocol:    &tcp,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return dial() },
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, // the enclave pins the fingerprint instead of a chain
			VerifyConnection: func(cs tls.ConnectionState) error {
				r.leaf = Fingerprint(cs.PeerCertificates[0])
				return nil
			},
		},
	}
	if err := r.c.Start(); err != nil {
		return nil, err
	}
	u, _ := base.ParseURL(Link)
	desc, _, err := r.c.Describe(u)
	if err != nil {
		r.c.Close()
		return nil, err
	}
	if err := r.c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		r.c.Close()
		return nil, err
	}
	r.c.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		if medi.Type == description.MediaTypeVideo {
			r.video.Add(1)
			r.mu.Lock()
			r.headers = append(r.headers, pkt.Header)
			r.mu.Unlock()
		} else {
			r.audio.Add(1)
		}
	})
	if _, err := r.c.Play(nil); err != nil {
		r.c.Close()
		return nil, err
	}
	return r, nil
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

func TestCertificatePersists(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(a.Leaf) != Fingerprint(b.Leaf) {
		t.Fatal("certificate regenerated on second load")
	}
	info, err := os.Stat(filepath.Join(dir, CertFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("certificate file mode %o", info.Mode().Perm())
	}
	if len(Fingerprint(a.Leaf)) != 64 {
		t.Fatalf("fingerprint %q", Fingerprint(a.Leaf))
	}
	// A corrupt file is replaced rather than fatal.
	if err := os.WriteFile(filepath.Join(dir, CertFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadOrCreateCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(c.Leaf) == Fingerprint(a.Leaf) {
		t.Fatal("corrupt file not replaced")
	}
}

func TestReaderGetsPacketsAndPinsCertificate(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	desc := sampleDesc()
	pub, err := s.Publish(desc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(sampleDesc()); err != ErrBusy {
		t.Fatalf("second publish: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pump(ctx, t, pub, desc)

	r, err := play(t, s.Dial)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	defer r.c.Close()
	if r.leaf != s.Fingerprint() {
		t.Fatalf("reader saw %s, server has %s", r.leaf, s.Fingerprint())
	}
	waitFor(t, "packets", func() bool { return r.video.Load() >= 10 && r.audio.Load() >= 10 })
	waitFor(t, "reader count", func() bool { return s.Readers() == 1 })
	st := s.Stats()
	if !st.Publishing || st.VideoPackets < 10 || st.AudioPackets < 10 || st.VideoBytes == 0 || st.Readers != 1 {
		t.Fatalf("stats %+v", st)
	}

	// Ending the publication disconnects the reader; the server is then
	// ready for the next source.
	pub.Close()
	select {
	case <-func() chan struct{} {
		ch := make(chan struct{})
		go func() { _ = r.c.Wait(); close(ch) }()
		return ch
	}():
	case <-time.After(5 * time.Second):
		t.Fatal("reader survived the publication ending")
	}
	waitFor(t, "reader gone", func() bool { return s.Readers() == 0 })
	if s.Stats().Publishing {
		t.Fatal("still publishing")
	}
}

func TestDescribeWaitsForSource(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet(), DescribeWait: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The source comes up 300 ms after the enclave asks: the DESCRIBE waits.
	desc := sampleDesc()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(300 * time.Millisecond)
		pub, err := s.Publish(desc)
		if err != nil {
			return
		}
		pump(ctx, t, pub, desc)
	}()
	started := time.Now()
	r, err := play(t, s.Dial)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	defer r.c.Close()
	if time.Since(started) < 250*time.Millisecond {
		t.Fatal("describe answered before the source existed")
	}
	waitFor(t, "packets", func() bool { return r.video.Load() >= 5 })
}

func TestDescribeWithoutSourceIs404(t *testing.T) {
	// The default wait outlasts the enclave's relay, which gives the answer
	// 10 s: the connector is never what gives up first.
	if s, err := New(Config{StateDir: t.TempDir(), Logger: quiet()}); err != nil {
		t.Fatal(err)
	} else {
		s.Close()
		if s.cfg.DescribeWait != DefaultDescribeWait || DefaultDescribeWait <= 10*time.Second {
			t.Fatalf("default DescribeWait %s", s.cfg.DescribeWait)
		}
	}
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet(), DescribeWait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := play(t, s.Dial); err == nil {
		t.Fatal("played with no source")
	}
	// Wrong path is refused immediately.
	c := &gortsplib.Client{Scheme: "rtsps", Host: Target,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return s.Dial() },
		TLSConfig:   &tls.Config{InsecureSkipVerify: true}}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	u, _ := base.ParseURL("rtsps://" + Target + "/other")
	started := time.Now()
	if _, _, err := c.Describe(u); err == nil || time.Since(started) > 150*time.Millisecond {
		t.Fatalf("other path: %v after %s", err, time.Since(started))
	}
}

func TestDialAfterClose(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close() // idempotent
	if _, err := s.Dial(); err != ErrClosed {
		t.Fatalf("dial after close: %v", err)
	}
}

// tcp is the transport the enclave uses.
var tcp = gortsplib.ProtocolTCP

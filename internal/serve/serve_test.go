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
	pumpTimed(ctx, t, pub, desc, nil)
}

// pumpTimed is pump with each packet timed by `ntpOf` of its sequence
// number on the source's clock (WritePacketRTPWithNTP); nil times them at
// their writing, as pump does. The timestamps advance 3000 ticks (a 30 fps
// frame) per packet.
func pumpTimed(ctx context.Context, t *testing.T, pub *Publication, desc *description.Session,
	ntpOf func(seq uint16) time.Time) {
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
			var ntp time.Time
			if ntpOf != nil {
				ntp = ntpOf(seq)
			}
			if err := pub.WritePacketRTPWithNTP(m, pkt, ntp); err != nil {
				return
			}
		}
	}
}

// timedPacket is one video packet as a reader saw it: its RTP timestamp and
// the absolute time the stream's sender reports gave it, once they had.
type timedPacket struct {
	ts  uint32
	ntp time.Time
	ok  bool
}

// reader plays the stream over conns from dial and counts packets by kind,
// keeping the video packets' headers to check continuity and the time the
// sender reports gave each.
type reader struct {
	c            *gortsplib.Client
	video, audio atomic.Int64
	leaf         string

	mu      sync.Mutex
	headers []rtp.Header
	timed   []timedPacket
}

func (r *reader) videoHeaders() []rtp.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rtp.Header(nil), r.headers...)
}

func (r *reader) timedPackets() []timedPacket {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]timedPacket(nil), r.timed...)
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
			ntp, ok := r.c.PacketNTP(medi, pkt)
			r.mu.Lock()
			r.headers = append(r.headers, pkt.Header)
			r.timed = append(r.timed, timedPacket{ts: pkt.Timestamp, ntp: ntp, ok: ok})
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

// frameAt is the time the pump's packet seq belongs to when the source's
// clock started at base: one 30 fps frame per packet, as its timestamps say.
func frameAt(base time.Time, seq uint16) time.Time {
	return base.Add(time.Duration(seq) * time.Second / 30)
}

func TestReadersGetTheSourcesTime(t *testing.T) {
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
	// The source's clock runs 1.5 s behind this computer's: within
	// MaxSourceClockSkew, so the time it gives each frame is what the
	// readers' sender reports must carry, not the moment of forwarding.
	base := time.Now().Add(-1500 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pumpTimed(ctx, t, pub, desc, func(seq uint16) time.Time { return frameAt(base, seq) })

	r, err := play(t, s.Dial)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	defer r.c.Close()
	waitFor(t, "timed packets", func() bool {
		n := 0
		for _, p := range r.timedPackets() {
			if p.ok {
				n++
			}
		}
		return n >= 10
	})
	timed := 0
	for _, p := range r.timedPackets() {
		if !p.ok {
			continue // before the first sender report: no time yet
		}
		timed++
		want := frameAt(base, uint16(p.ts/3000))
		if d := p.ntp.Sub(want); d > time.Millisecond || d < -time.Millisecond {
			t.Fatalf("packet ts %d timed %s, source said %s (off by %s)", p.ts, p.ntp, want, d)
		}
	}
	if timed < 10 {
		t.Fatalf("only %d packets timed", timed)
	}
}

func TestASourceClockFarOffIsSetAside(t *testing.T) {
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
	// A camera whose clock was never set: an hour off. Its time would date
	// the stream wrongly, so the packets are timed by this computer's clock.
	base := time.Now().Add(-time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pumpTimed(ctx, t, pub, desc, func(seq uint16) time.Time { return frameAt(base, seq) })

	r, err := play(t, s.Dial)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	defer r.c.Close()
	waitFor(t, "timed packets", func() bool {
		for _, p := range r.timedPackets() {
			if p.ok {
				return true
			}
		}
		return false
	})
	for _, p := range r.timedPackets() {
		if !p.ok {
			continue
		}
		if off := time.Since(p.ntp); off > 5*time.Second || off < -5*time.Second {
			t.Fatalf("packet ts %d timed %s, %s from now: the source's clock was not set aside", p.ts, p.ntp, off)
		}
	}
}

// sourceClock plays a source's sender reports for a TimedWriter: it knows
// the time of a packet (frameAt on `base`) once `reported` is set.
type sourceClock struct {
	base     time.Time
	reported atomic.Bool
}

func (c *sourceClock) timeOf(_ *description.Media, pkt *rtp.Packet) (time.Time, bool) {
	if !c.reported.Load() {
		return time.Time{}, false
	}
	return frameAt(c.base, uint16(pkt.Timestamp/3000)), true
}

// writeTimed writes one packet per media through w, as pumpTimed makes
// them, with sequence number seq.
func writeTimed(t *testing.T, w *TimedWriter, desc *description.Session, seq uint16) {
	t.Helper()
	for _, m := range desc.Medias {
		pt := m.Formats[0].PayloadType()
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, Marker: true, PayloadType: pt, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: uint32(pt)},
			Payload: []byte{0x65, 1, 2, 3},
		}
		if err := w.Write(m, pkt); err != nil {
			t.Fatalf("write %d: %v", seq, err)
		}
	}
}

func TestTimedWriterHoldsTheFirstPacketsForTheSourcesReport(t *testing.T) {
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
	r, err := play(t, s.Dial)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	defer r.c.Close()

	// The source reports after its fourth packet, as a gortsplib publisher
	// reports right after its first: the packets before are held, so that
	// the reader's first sender report is anchored on the source's time.
	clock := &sourceClock{base: time.Now().Add(-1500 * time.Millisecond)}
	w := NewTimedWriter(pub, clock.timeOf)
	for seq := uint16(1); seq <= 4; seq++ {
		writeTimed(t, w, desc, seq)
	}
	if w.Held() != 8 || s.Stats().VideoPackets != 0 || r.video.Load() != 0 {
		t.Fatalf("held %d, %d video packets written, reader saw %d", w.Held(), s.Stats().VideoPackets, r.video.Load())
	}
	clock.reported.Store(true)
	writeTimed(t, w, desc, 5)
	if w.Held() != 0 {
		t.Fatalf("still holding %d", w.Held())
	}
	// The reader's report follows its first packet, and the flushed packets
	// may all be on the wire before it: packets keep coming, a frame apart,
	// until the reader has timed a few - it must, once its report is out.
	seq := uint16(5)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		timed := 0
		for _, p := range r.timedPackets() {
			if p.ok {
				timed++
			}
		}
		if timed >= 4 {
			break
		}
		seq++
		writeTimed(t, w, desc, seq)
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "packets", func() bool { return r.video.Load() >= int64(seq) && r.audio.Load() >= int64(seq) })
	// The ones it has timed carry the source's time, not the forwarding
	// time 1.5 s later.
	timed := 0
	for i, p := range r.timedPackets() {
		if p.ts != uint32(i+1)*3000 {
			t.Fatalf("packet %d has timestamp %d: order lost", i, p.ts)
		}
		if !p.ok {
			continue
		}
		timed++
		want := frameAt(clock.base, uint16(i+1))
		if d := p.ntp.Sub(want); d > time.Millisecond || d < -time.Millisecond {
			t.Fatalf("packet %d timed %s, source said %s (off by %s)", i, p.ntp, want, d)
		}
	}
	if timed == 0 {
		t.Fatal("no packet timed")
	}
}

func TestTimedWriterGivesUpOnASourceThatDoesNotReport(t *testing.T) {
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
	r, err := play(t, s.Dial)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	defer r.c.Close()

	// A source that never reports: after the hold its packets flow, timed
	// at their forwarding, none of them lost.
	w := NewTimedWriter(pub, func(*description.Media, *rtp.Packet) (time.Time, bool) { return time.Time{}, false })
	w.hold = 100 * time.Millisecond
	writeTimed(t, w, desc, 1)
	if w.Held() != 2 {
		t.Fatalf("held %d", w.Held())
	}
	time.Sleep(120 * time.Millisecond)
	for seq := uint16(2); seq <= 6; seq++ {
		writeTimed(t, w, desc, seq)
	}
	if w.Held() != 0 {
		t.Fatalf("still holding %d", w.Held())
	}
	waitFor(t, "packets", func() bool { return r.video.Load() >= 6 && r.audio.Load() >= 6 })
	for i, p := range r.timedPackets() {
		if p.ts != uint32(i+1)*3000 {
			t.Fatalf("packet %d has timestamp %d: order lost", i, p.ts)
		}
		if p.ok {
			if off := time.Since(p.ntp); off > 2*time.Second || off < -2*time.Second {
				t.Fatalf("packet %d timed %s from now", i, off)
			}
		}
	}
	// Many packets before the hold runs out settle it too.
	w2 := NewTimedWriter(pub, func(*description.Media, *rtp.Packet) (time.Time, bool) { return time.Time{}, false })
	w2.hold = time.Hour
	for seq := uint16(7); seq < 7+HoldPackets; seq++ {
		writeTimed(t, w2, desc, seq)
	}
	if w2.Held() != 0 {
		t.Fatalf("still holding %d after %d packets", w2.Held(), HoldPackets)
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

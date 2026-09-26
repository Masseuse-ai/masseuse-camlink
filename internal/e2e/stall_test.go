package e2e

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/gateway"
	"github.com/FemLed/masseuse-camlink/internal/identity"
	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/FemLed/masseuse-camlink/internal/tunnel"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// pausingProxy sits between the connector and the gateway. While paused it
// stops reading what the connector sends, the way a stalled uplink does:
// the connector's bytes pile up in its socket and nothing reaches the
// gateway, while the gateway's own bytes still get through.
type pausingProxy struct {
	ln       net.Listener
	upstream string

	mu     sync.Mutex
	resume chan struct{} // non-nil while paused; closed to resume
}

func startPausingProxy(t *testing.T, upstream string) *pausingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pausingProxy{ln: ln, upstream: upstream}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return p
}

func (p *pausingProxy) serve(down net.Conn) {
	defer down.Close()
	up, err := net.Dial("tcp", p.upstream)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { // gateway to connector: never held
		_, _ = io.Copy(down, up)
		done <- struct{}{}
	}()
	go func() { // connector to gateway: held while paused
		buf := make([]byte, 32<<10)
		for {
			p.wait()
			n, err := down.Read(buf)
			if n > 0 {
				if _, werr := up.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
}

func (p *pausingProxy) wait() {
	p.mu.Lock()
	ch := p.resume
	p.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

func (p *pausingProxy) pause() {
	p.mu.Lock()
	if p.resume == nil {
		p.resume = make(chan struct{})
	}
	p.mu.Unlock()
}

func (p *pausingProxy) unpause() {
	p.mu.Lock()
	if p.resume != nil {
		close(p.resume)
		p.resume = nil
	}
	p.mu.Unlock()
}

// livePublisher is a source at a realistic rate: 30 frames a second of 8
// packets of 1200 bytes (about 2.3 Mb/s), one in thirty a keyframe, plus
// audio every 20 ms.
func livePublisher(ctx context.Context, srv *serve.Server) error {
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 97, ChannelCount: 1}}},
	}}
	pub, err := srv.Publish(desc)
	if err != nil {
		return err
	}
	defer pub.Close()
	video := time.NewTicker(33 * time.Millisecond)
	defer video.Stop()
	audio := time.NewTicker(20 * time.Millisecond)
	defer audio.Stop()
	var vseq, aseq uint16
	var frame uint32
	body := make([]byte, 1200)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-video.C:
			ts := frame * 3000
			for i := 0; i < 8; i++ {
				vseq++
				payload := append([]byte(nil), body...)
				switch {
				case i == 0 && frame%30 == 0:
					payload[0] = 0x65 // IDR
				case i == 0:
					payload[0] = 0x41 // non-IDR slice
				default:
					payload[0], payload[1] = 0x7c, 0x01 // FU-A continuation
				}
				pkt := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: i == 7, PayloadType: 96, SequenceNumber: vseq, Timestamp: ts, SSRC: 1}, Payload: payload}
				if err := pub.WritePacketRTP(desc.Medias[0], pkt); err != nil {
					return err
				}
			}
			frame++
		case <-audio.C:
			aseq++
			pkt := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, PayloadType: 97, SequenceNumber: aseq, Timestamp: uint32(aseq) * 960, SSRC: 2}, Payload: make([]byte, 160)}
			if err := pub.WritePacketRTP(desc.Medias[1], pkt); err != nil {
				return err
			}
		}
	}
}

// gapReader plays the stream through the relay and notes, for every jump
// in the video timestamps (frames missing from the stream), the NAL type
// the video resumed with, and when each kind of packet last arrived.
type gapReader struct {
	c            *gortsplib.Client
	video, audio atomic.Int64

	mu          sync.Mutex
	lastVideoAt time.Time
	lastAudioAt time.Time
	lastTS      uint32
	haveTS      bool
	resumedWith []byte
}

func readGaps(relayAddr string) (*gapReader, error) {
	r := &gapReader{}
	r.c = &gortsplib.Client{
		Scheme: "rtsps", Host: relayAddr, ReadTimeout: 15 * time.Second, Protocol: &tcp,
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // MediaMTX pins the fingerprint instead of a chain
	}
	if err := r.c.Start(); err != nil {
		return nil, err
	}
	u, _ := base.ParseURL("rtsps://" + relayAddr + "/" + serve.Path)
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
		now := time.Now()
		r.mu.Lock()
		defer r.mu.Unlock()
		if medi.Type == description.MediaTypeVideo {
			r.video.Add(1)
			// Frames are 3000 ticks apart; a larger step means frames were
			// dropped, and the first packet after it must begin a keyframe.
			if r.haveTS && pkt.Timestamp != r.lastTS && pkt.Timestamp-r.lastTS > 4500 && len(pkt.Payload) > 0 {
				r.resumedWith = append(r.resumedWith, pkt.Payload[0]&0x1F)
			}
			r.lastTS, r.haveTS = pkt.Timestamp, true
			r.lastVideoAt = now
		} else {
			r.audio.Add(1)
			r.lastAudioAt = now
		}
	})
	if _, err := r.c.Play(nil); err != nil {
		r.c.Close()
		return nil, err
	}
	return r, nil
}

func (r *gapReader) snapshot() (video, audio int64, lastVideo, lastAudio time.Time, resumed []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.video.Load(), r.audio.Load(), r.lastVideoAt, r.lastAudioAt, append([]byte(nil), r.resumedWith...)
}

func TestTunnelSurvivesAnUplinkStallByDroppingVideo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The gateway pings every 20 s: none falls in the stall, so what is
	// tested is the connector's side of it (its pongs are covered by the
	// same mechanism; see the residual risk in the README).
	gw := gateway.New(gateway.Config{Logger: logger, PingInterval: 20 * time.Second})
	wsSrv := httptest.NewTLSServer(gw.WSHandler())
	t.Cleanup(wsSrv.Close)
	relay, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = gw.ServeRelay(ctx, relay) }()

	proxy := startPausingProxy(t, wsSrv.Listener.Addr().String())
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spki := sha256.Sum256(wsSrv.Certificate().RawSubjectPublicKeyInfo)
	roots := x509.NewCertPool()
	roots.AddCert(wsSrv.Certificate())
	srv, err := serve.New(serve.Config{StateDir: t.TempDir(), Logger: logger, DescribeWait: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	dialer := &tunnel.Dialer{
		Identity: id, Attester: &fakeAttester{spki: spki}, RootCAs: roots, Logger: logger,
		Local:         map[string]func() (net.Conn, error){serve.Target: srv.Dial},
		ProbeInterval: 250 * time.Millisecond,
	}
	r := &rig{gw: gw, wsSrv: wsSrv, relay: relay, control: gw.ControlHandler(), connector: id, dialer: dialer}

	// The connector reaches the gateway through the proxy.
	ticket, ticketHash := mintTicket(t)
	r.expect(t, ticketHash)
	tun, err := dialer.Dial(ctx, "https://"+proxy.ln.Addr().String(), ticket, ticketHash)
	if err != nil {
		t.Fatalf("dial through the proxy: %v", err)
	}
	defer tun.Close()
	go func() { _ = tun.Serve(ctx) }()
	waitStatus(t, r, func(s gateway.Status) bool { return s.Connected })
	srv.SetBacklog(tun.Backlog)

	if code, _ := r.ctl(t, "POST", "/target", map[string]any{"host": "127.0.0.1", "port": serve.Port}); code != 200 {
		t.Fatalf("target: %d", code)
	}
	pctx, pcancel := context.WithCancel(context.Background())
	defer pcancel()
	pubErr := make(chan error, 1)
	go func() { pubErr <- livePublisher(pctx, srv) }()
	reader, err := readGaps(relay.Addr().String())
	if err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	defer reader.c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (reader.video.Load() < 100 || reader.audio.Load() < 20) {
		time.Sleep(10 * time.Millisecond)
	}
	if reader.video.Load() < 100 || reader.audio.Load() < 20 {
		t.Fatalf("before the stall: video %d audio %d", reader.video.Load(), reader.audio.Load())
	}
	if b := tun.Backlog(); b > 500*time.Millisecond {
		t.Fatalf("backlog before the stall %s", b)
	}

	// The uplink stalls for five seconds.
	proxy.pause()
	stalledAt := time.Now()
	time.Sleep(5 * time.Second)
	backlog := tun.Backlog()
	stalled := srv.Stats()
	proxy.unpause()
	resumedAt := time.Now()

	if backlog < 3*time.Second {
		t.Errorf("backlog during the stall %s, want seconds", backlog)
	}
	if !stalled.Congested || stalled.VideoFramesDropped == 0 || stalled.Episodes == 0 {
		t.Errorf("gate during the stall: %+v", stalled)
	}
	select {
	case <-tun.Done():
		t.Fatalf("tunnel ended during the stall: %v", tun.Err())
	default:
	}

	// Afterwards: the tunnel is up, audio and video flow again, and video
	// resumed with a keyframe.
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, _, lastVideo, lastAudio, _ := reader.snapshot()
		if lastVideo.After(resumedAt.Add(time.Second)) && lastAudio.After(resumedAt.Add(time.Second)) && !srv.Stats().Congested {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-tun.Done():
		t.Fatalf("tunnel ended after the stall: %v", tun.Err())
	default:
	}
	_, _, lastVideo, lastAudio, resumed := reader.snapshot()
	if !lastVideo.After(resumedAt.Add(time.Second)) || !lastAudio.After(resumedAt.Add(time.Second)) {
		t.Fatalf("after the stall: last video %s, last audio %s after resume", lastVideo.Sub(resumedAt), lastAudio.Sub(resumedAt))
	}
	after := srv.Stats()
	if after.Congested {
		t.Errorf("still gating %s after the stall", time.Since(resumedAt))
	}
	if after.Episodes != 1 {
		t.Errorf("congestion episodes %d, want 1", after.Episodes)
	}
	if len(resumed) == 0 {
		t.Fatalf("reader saw no frames missing across a %s stall", resumedAt.Sub(stalledAt))
	}
	for _, nal := range resumed {
		if nal != 5 && nal != 7 && nal != 8 {
			t.Errorf("video resumed with NAL type %d after dropped frames, not a keyframe", nal)
		}
	}
	if b := tun.Backlog(); b > time.Second {
		t.Errorf("backlog after the stall %s", b)
	}
	pcancel()
	if err := <-pubErr; err != nil {
		t.Fatalf("publisher: %v", err)
	}
}

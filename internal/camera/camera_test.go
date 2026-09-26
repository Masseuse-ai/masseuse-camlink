package camera

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"
)

var tcp = gortsplib.ProtocolTCP

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// cameraClockBehind is how far the fake camera's clock (its sender reports)
// sits behind this computer's: within serve.MaxSourceClockSkew, so the
// stream's readers must be told the camera's time, not the forwarding time.
const cameraClockBehind = 600 * time.Millisecond

// fakeCamera is an RTSPS camera with basic authentication, an H.264 track
// sent as oversized packets (as cameras on TCP do) and an Opus track.
type fakeCamera struct {
	srv         *gortsplib.Server
	stream      *gortsplib.ServerStream
	desc        *description.Session
	fingerprint string
	host        string
	user, pass  string
	described   atomic.Int32
	stop        chan struct{}
	wg          sync.WaitGroup
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "camera"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func startCamera(t *testing.T, cert tls.Certificate) *fakeCamera {
	t.Helper()
	c := &fakeCamera{user: "viewer", pass: "s3cret", stop: make(chan struct{}), fingerprint: serve.Fingerprint(cert.Leaf)}
	c.desc = &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 97, ChannelCount: 2}}},
	}}
	c.srv = &gortsplib.Server{
		Handler: c, RTSPAddress: "127.0.0.1:0",
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	if err := c.srv.Start(); err != nil {
		t.Fatal(err)
	}
	c.host = c.srv.NetListener().Addr().String()
	c.stream = &gortsplib.ServerStream{Server: c.srv, Desc: c.desc}
	if err := c.stream.Initialize(); err != nil {
		t.Fatal(err)
	}
	c.wg.Add(1)
	go c.pump()
	t.Cleanup(func() {
		close(c.stop)
		c.wg.Wait()
		c.stream.Close()
		c.srv.Close()
	})
	return c
}

// pump sends one 2800-byte H.264 access unit as two ~1400-byte FU-A
// fragments (over the proxy's 1200-byte payload limit, as cameras on TCP
// often send) and one Opus packet every 20 ms. Video timestamps follow the
// wall clock and each unit is timed on the camera's clock, which sits
// cameraClockBehind this computer's.
func (c *fakeCamera) pump() {
	defer c.wg.Done()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var seq uint16
	nalu := make([]byte, 2800)
	nalu[0] = 0x65 // IDR slice
	start := time.Now()
	for {
		select {
		case <-c.stop:
			return
		case <-tick.C:
		}
		seq++
		now := time.Now()
		ts := uint32(now.Sub(start).Seconds() * 90000)
		half := len(nalu) / 2
		for i, chunk := range [][]byte{nalu[1:half], nalu[half:]} {
			fu := []byte{0x7c, 0x05} // FU-A indicator, type 5 (IDR)
			if i == 0 {
				fu[1] |= 0x80 // start
			} else {
				fu[1] |= 0x40 // end
			}
			pkt := &rtp.Packet{
				Header:  rtp.Header{Version: 2, Marker: i == 1, PayloadType: 96, SequenceNumber: seq*2 + uint16(i), Timestamp: ts, SSRC: 11},
				Payload: append(fu, chunk...),
			}
			_ = c.stream.WritePacketRTPWithNTP(c.desc.Medias[0], pkt, now.Add(-cameraClockBehind))
		}
		_ = c.stream.WritePacketRTP(c.desc.Medias[1], &rtp.Packet{
			Header:  rtp.Header{Version: 2, Marker: true, PayloadType: 97, SequenceNumber: seq, Timestamp: uint32(seq) * 960, SSRC: 22},
			Payload: []byte{1, 2, 3, 4},
		})
	}
}

func (c *fakeCamera) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !ctx.Conn.VerifyCredentials(ctx.Request, c.user, c.pass) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	c.described.Add(1)
	if ctx.Path != "/live" {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, c.stream, nil
}

func (c *fakeCamera) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !ctx.Conn.VerifyCredentials(ctx.Request, c.user, c.pass) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	return &base.Response{StatusCode: base.StatusOK}, c.stream, nil
}

func (c *fakeCamera) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (c *fakeCamera) url(user, pass string) string {
	if user == "" {
		return "rtsps://" + c.host + "/live"
	}
	return fmt.Sprintf("rtsps://%s:%s@%s/live", user, pass, c.host)
}

// reader plays the connector's stream in-process and records packet sizes
// and how far behind this computer's clock the stream's sender reports
// timed the latest video packet (lag, nanoseconds, once timed).
type reader struct {
	c            *gortsplib.Client
	video, audio atomic.Int64
	maxSize      atomic.Int64
	timed        atomic.Bool
	lag          atomic.Int64
}

func play(t *testing.T, srv *serve.Server) *reader {
	t.Helper()
	r := &reader{}
	r.c = &gortsplib.Client{
		Scheme: "rtsps", Host: serve.Target, Protocol: &tcp, ReadTimeout: 10 * time.Second,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return srv.Dial() },
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	}
	if err := r.c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.c.Close)
	u, _ := base.ParseURL(serve.Link)
	desc, _, err := r.c.Describe(u)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if err := r.c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatal(err)
	}
	r.c.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		if medi.Type == description.MediaTypeVideo {
			r.video.Add(1)
			if n := int64(pkt.MarshalSize()); n > r.maxSize.Load() {
				r.maxSize.Store(n)
			}
			if ntp, ok := r.c.PacketNTP(medi, pkt); ok {
				r.lag.Store(int64(time.Since(ntp)))
				r.timed.Store(true)
			}
		} else {
			r.audio.Add(1)
		}
	})
	if _, err := r.c.Play(nil); err != nil {
		t.Fatal(err)
	}
	return r
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newSink(t *testing.T) *serve.Server {
	t.Helper()
	srv, err := serve.New(serve.Config{StateDir: t.TempDir(), Logger: quiet(), DescribeWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestProxyTrustsOnFirstUseAndRepacketizes(t *testing.T) {
	cam := startCamera(t, selfSigned(t))
	sink := newSink(t)
	state := t.TempDir()
	var trusted atomic.Int32
	src, err := New(context.Background(), sink, Options{
		URL: cam.url("viewer", "s3cret"), StateDir: state, Logger: quiet(),
		OnTrust: func(host, fp string) {
			if host != cam.host || fp != cam.fingerprint {
				t.Errorf("trusted %s %s", host, fp)
			}
			trusted.Add(1)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.Label() != "Camera at 127.0.0.1" || src.Fingerprint() != "" {
		t.Fatalf("label %q fingerprint %q", src.Label(), src.Fingerprint())
	}
	desc, err := src.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(desc.Medias) != 2 || trusted.Load() != 1 || src.Fingerprint() != cam.fingerprint {
		t.Fatalf("probe: medias %d trusted %d fingerprint %q", len(desc.Medias), trusted.Load(), src.Fingerprint())
	}
	store, _ := os.ReadFile(filepath.Join(state, StoreFile))
	var saved map[string]string
	if err := json.Unmarshal(store, &saved); err != nil || saved[cam.host] != cam.fingerprint {
		t.Fatalf("store %s: %v", store, err)
	}

	src.Start()
	defer src.Stop()
	r := play(t, sink)
	waitFor(t, "packets", func() bool { return r.video.Load() >= 20 && r.audio.Load() >= 10 })
	if !src.Publishing() {
		t.Fatal("not publishing")
	}
	// The camera's ~1500-byte fragments arrive re-packetized under the limit.
	if max := r.maxSize.Load(); max > payloadMax+12 {
		t.Fatalf("video packet of %d bytes forwarded", max)
	}
	// The re-packetized units keep the camera's own time (its sender
	// reports say its clock is 600 ms behind); the readers are told that,
	// not the moment of forwarding.
	waitFor(t, "timed packets", r.timed.Load)
	if lag := time.Duration(r.lag.Load()); lag < cameraClockBehind-100*time.Millisecond || lag > cameraClockBehind+700*time.Millisecond {
		t.Fatalf("reader timed the latest frame %s ago; the camera's clock is %s behind", lag, cameraClockBehind)
	}
	if st := sink.Stats(); st.VideoPackets < 20 || st.AudioPackets < 10 {
		t.Fatalf("stats %+v", st)
	}
	if trusted.Load() != 1 {
		t.Fatalf("trusted %d times", trusted.Load())
	}

	src.Stop()
	waitFor(t, "stop", func() bool { return !src.Publishing() && !sink.Stats().Publishing })
	if src.Running() {
		t.Fatal("running after stop")
	}
}

func TestProxyRefusesChangedCertificate(t *testing.T) {
	first := startCamera(t, selfSigned(t))
	sink := newSink(t)
	state := t.TempDir()
	src, err := New(context.Background(), sink, Options{URL: first.url("viewer", "s3cret"), StateDir: state, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Something else answers at the same address with another certificate:
	// the saved fingerprint refuses it.
	other := startCamera(t, selfSigned(t))
	store := map[string]string{other.host: first.fingerprint}
	b, _ := json.Marshal(store)
	if err := os.WriteFile(filepath.Join(state, StoreFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	src2, err := New(context.Background(), sink, Options{URL: other.url("viewer", "s3cret"), StateDir: state, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = src2.Probe(context.Background())
	var fe *FingerprintError
	if !errors.As(err, &fe) || fe.Pinned != first.fingerprint || fe.Seen != other.fingerprint {
		t.Fatalf("probe: %v", err)
	}
	if other.described.Load() != 0 {
		t.Fatal("request reached the camera despite the certificate")
	}

	// An explicit pin wins over the store and is checked the same way.
	src3, err := New(context.Background(), sink, Options{
		URL: other.url("viewer", "s3cret"), StateDir: state, Logger: quiet(),
		Fingerprint: strings.ToUpper(other.fingerprint[:16]) + ":" + other.fingerprint[16:],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src3.Probe(context.Background()); err != nil {
		t.Fatalf("pinned probe: %v", err)
	}
	if _, err := New(context.Background(), sink, Options{URL: other.url("viewer", "s3cret"), Fingerprint: "abc"}); err == nil {
		t.Fatal("bad fingerprint accepted")
	}
}

func TestProxyCredentials(t *testing.T) {
	cam := startCamera(t, selfSigned(t))
	sink := newSink(t)
	src, err := New(context.Background(), sink, Options{URL: cam.url("viewer", "wrong"), StateDir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("wrong password: %v", err)
	}
	if err := src.LastError(); err != nil {
		t.Fatalf("last error before start: %v", err)
	}
}

func TestProxyRejectsBadLinks(t *testing.T) {
	sink := newSink(t)
	for _, u := range []string{
		"rtsp://192.168.1.20/live",       // not encrypted
		"rtsps://93.184.216.34:322/live", // public address
		"rtsps://",
		"https://192.168.1.20/",
	} {
		if _, err := New(context.Background(), sink, Options{URL: u}); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	src, err := New(context.Background(), sink, Options{URL: "rtsps://user:pw@192.168.1.20/live"})
	if err != nil {
		t.Fatal(err)
	}
	if src.Host() != "192.168.1.20:322" || src.Label() != "Camera at 192.168.1.20" {
		t.Fatalf("host %q label %q", src.Host(), src.Label())
	}
	if normalizeFingerprint("SHA256:AB:cd") != "" || normalizeFingerprint(strings.Repeat("ab", 32)) != strings.Repeat("ab", 32) {
		t.Fatal("normalizeFingerprint")
	}
}

package e2e

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/gateway"
	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/FemLed/masseuse-camlink/internal/share"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// ownListener adds the gateway's own-endpoint listener to a rig: every
// connection to it is a stream to the connector's own endpoint, whatever
// the target.
func ownListener(t *testing.T, r *rig) net.Listener {
	t.Helper()
	own, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.gw.ServeOwn(ctx, own) }()
	return own
}

// readPath plays one of the connector's paths through addr as the enclave's
// RTSP server does (RTSPS over TCP), counting video packets.
func readPath(addr, path string) (*gortsplib.Client, *atomic.Int64, error) {
	var n atomic.Int64
	c := &gortsplib.Client{
		Scheme: "rtsps", Host: addr, ReadTimeout: 12 * time.Second, Protocol: &tcp,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if err := c.Start(); err != nil {
		return nil, nil, err
	}
	u, _ := base.ParseURL("rtsps://" + addr + "/" + path)
	desc, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		c.Close()
		return nil, nil, err
	}
	c.OnPacketRTPAny(func(medi *description.Media, _ format.Format, _ *rtp.Packet) {
		if medi.Type == description.MediaTypeVideo {
			n.Add(1)
		}
	})
	if _, err := c.Play(nil); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, &n, nil
}

// enclavePublisher publishes the phone's picture into the connector's phone
// path through addr the way the enclave's ffmpeg does (RTSPS over TCP, a
// RECORD), a video packet every 20 ms until ctx ends.
func enclavePublisher(ctx context.Context, addr string) error {
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
	}}
	c := &gortsplib.Client{
		Scheme: "rtsps", Host: addr, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, Protocol: &tcp,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if err := c.StartRecording("rtsps://"+addr+"/"+serve.PhonePath, desc); err != nil {
		return err
	}
	defer c.Close()
	var seq uint16
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		seq++
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, Marker: true, PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 7},
			Payload: []byte{0x65, 1, 2, 3},
		}
		if err := c.WritePacketRTPWithNTP(desc.Medias[0], pkt, time.Now()); err != nil {
			return err
		}
	}
}

// localReader reads the phone's picture off the share server the way OBS's
// Media Source does: plain RTSP on loopback.
func localReader(url string) (*gortsplib.Client, *atomic.Int64, error) {
	var n atomic.Int64
	u, err := base.ParseURL(url)
	if err != nil {
		return nil, nil, err
	}
	c := &gortsplib.Client{Scheme: "rtsp", Host: u.Host, ReadTimeout: 5 * time.Second, Protocol: &tcp}
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

func TestThePhonesPictureReachesTheComputerAndTheFaceComesBack(t *testing.T) {
	r := setup(t)
	own := ownListener(t, r)
	srv, err := serve.New(serve.Config{StateDir: t.TempDir(), Logger: r.dialer.Logger, DescribeWait: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	r.dialer.Local = map[string]func() (net.Conn, error){serve.Target: srv.Dial}
	_, stop := attach(t, r)
	defer stop()

	// The relay listener is the network camera's: a camera on the
	// connector's network is the body view, and the target lock is its.
	cam := startCamera(t)
	host, portStr, _ := net.SplitHostPort(cam.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	if code, _ := r.ctl(t, "POST", "/target", map[string]any{"host": host, "port": port}); code != 200 {
		t.Fatalf("target: %d", code)
	}
	if _, got, err := mediamtx(t, r.relay.Addr().String(), []byte("body")); err != nil || string(got) != "body" {
		t.Fatalf("network camera through the relay: %v %q", err, got)
	}

	// Not asked for on the computer: the enclave's publish is refused at
	// the connector, and nothing is served locally.
	ctx0, cancel0 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel0()
	if err := enclavePublisher(ctx0, own.Addr().String()); err == nil {
		t.Fatal("the phone's picture was taken without -share-phone")
	}

	// Asked for: the connector serves it on loopback for OBS.
	secret, _ := share.NewSecret()
	shared, err := share.New(share.Config{Port: -1, Secret: secret, Logger: r.dialer.Logger})
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	srv.SetReceiver(shared)
	pctx, pcancel := context.WithCancel(context.Background())
	pubErr := make(chan error, 1)
	go func() { pubErr <- enclavePublisher(pctx, own.Addr().String()) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !shared.Arriving() {
		time.Sleep(10 * time.Millisecond)
	}
	if !shared.Arriving() {
		t.Fatal("the phone's picture did not arrive through the own listener")
	}
	obs, got, err := localReader(shared.URL())
	if err != nil {
		t.Fatalf("local reader: %v", err)
	}
	defer obs.Close()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && got.Load() < 20 {
		time.Sleep(10 * time.Millisecond)
	}
	if got.Load() < 20 {
		t.Fatalf("packets to the local reader: %d", got.Load())
	}

	// Meanwhile the connector's front-facing camera comes back through the
	// same own listener, beside the network camera the relay is locked to.
	faceDesc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
	}}
	fctx, fcancel := context.WithCancel(context.Background())
	defer fcancel()
	var faceStarted atomic.Bool
	srv.Face().SetOnDemand(func() {
		go func() {
			pub, err := srv.Face().Publish(faceDesc)
			if err != nil {
				t.Error(err)
				return
			}
			faceStarted.Store(true)
			defer pub.Close()
			var seq uint16
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-fctx.Done():
					return
				case <-tick.C:
				}
				seq++
				pkt := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 96}, Payload: []byte{0x65, 1}}
				_ = pub.WritePacketRTP(faceDesc.Medias[0], pkt)
			}
		}()
	})
	face, faceN, err := readPath(own.Addr().String(), serve.FacePath)
	if err != nil {
		t.Fatalf("face through the own listener: %v", err)
	}
	defer face.Close()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && faceN.Load() < 20 {
		time.Sleep(10 * time.Millisecond)
	}
	if faceN.Load() < 20 || !faceStarted.Load() {
		t.Fatalf("face packets %d, started %v", faceN.Load(), faceStarted.Load())
	}
	st := r.gw.Status()
	if !st.Target || st.OwnStreams < 2 {
		t.Fatalf("status %+v: the relay's target and two own streams expected", st)
	}
	// The network camera is still reachable through the relay: the own
	// streams did not take the lock.
	if _, got, err := mediamtx(t, r.relay.Addr().String(), []byte("still")); err != nil || string(got) != "still" {
		t.Fatalf("network camera after the own streams: %v %q", err, got)
	}

	// The enclave stops publishing: the local reader is disconnected, the
	// path gone until the next picture.
	pcancel()
	if err := <-pubErr; err != nil {
		t.Fatalf("publisher: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = obs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the local reader survived the picture stopping")
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && shared.Arriving() {
		time.Sleep(10 * time.Millisecond)
	}
	if shared.Arriving() {
		t.Fatal("still arriving")
	}
}

func TestTheOwnListenerIsRefusedWithoutAConnector(t *testing.T) {
	r := setup(t)
	own := ownListener(t, r)
	conn, err := net.Dial("tcp", own.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("the own listener stayed open with no connector")
	}
	if st := r.gw.Status(); st.OwnStreams != 0 {
		t.Fatalf("status %+v", st)
	}
	_ = gateway.OwnTarget
}

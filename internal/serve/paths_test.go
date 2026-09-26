package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// playPath is play for any served path.
func playPath(t *testing.T, dial func() (net.Conn, error), path string) (*reader, error) {
	t.Helper()
	r := &reader{}
	r.c = &gortsplib.Client{
		Scheme: "rtsps", Host: Target, ReadTimeout: 12 * time.Second, Protocol: &tcp,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return dial() },
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	}
	if err := r.c.Start(); err != nil {
		return nil, err
	}
	u, _ := base.ParseURL("rtsps://" + Target + "/" + path)
	desc, _, err := r.c.Describe(u)
	if err != nil {
		r.c.Close()
		return nil, err
	}
	if err := r.c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		r.c.Close()
		return nil, err
	}
	r.c.OnPacketRTPAny(func(medi *description.Media, _ format.Format, _ *rtp.Packet) {
		if medi.Type == description.MediaTypeVideo {
			r.video.Add(1)
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

// fakeReceiver stands in for internal/share: it keeps what arrives.
type fakeReceiver struct {
	mu       sync.Mutex
	packets  int
	timed    int
	closed   int
	refuse   error
	received *description.Session
}

type fakeReception struct{ r *fakeReceiver }

func (r *fakeReceiver) Publish(desc *description.Session) (Reception, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refuse != nil {
		return nil, r.refuse
	}
	r.received = desc
	return &fakeReception{r: r}, nil
}

func (p *fakeReception) WritePacketRTPWithNTP(_ *description.Media, _ *rtp.Packet, ntp time.Time) error {
	p.r.mu.Lock()
	defer p.r.mu.Unlock()
	p.r.packets++
	if !ntp.IsZero() {
		p.r.timed++
	}
	return nil
}

func (p *fakeReception) Close() {
	p.r.mu.Lock()
	defer p.r.mu.Unlock()
	p.r.closed++
}

func (r *fakeReceiver) counts() (packets, timed, closed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.packets, r.timed, r.closed
}

// record publishes desc into path through dial the way the enclave's ffmpeg
// does over the tunnel, writing a video packet every 20 ms until ctx ends.
func record(ctx context.Context, dial func() (net.Conn, error), path string, desc *description.Session) error {
	c := &gortsplib.Client{
		Scheme: "rtsps", Host: Target, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, Protocol: &tcp,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return dial() },
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	}
	if err := c.StartRecording("rtsps://"+Target+"/"+path, desc); err != nil {
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
			Header:  rtp.Header{Version: 2, Marker: true, PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 96},
			Payload: []byte{0x65, 1, 2, 3},
		}
		if err := c.WritePacketRTPWithNTP(desc.Medias[0], pkt, time.Now()); err != nil {
			return err
		}
	}
}

func TestTheFaceStreamIsServedBesideTheCamera(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet(), DescribeWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Camera() != s.Stream(Path) || s.Face() != s.Stream(FacePath) || s.Stream("other") != nil {
		t.Fatal("streams by path")
	}
	// The face stream starts its source on demand: a DESCRIBE with nothing
	// publishing calls the hook, whose source then publishes.
	faceDesc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var started sync.WaitGroup
	started.Add(1)
	s.Face().SetOnDemand(func() {
		go func() {
			time.Sleep(200 * time.Millisecond)
			pub, err := s.Face().Publish(faceDesc)
			if err != nil {
				t.Error(err)
				return
			}
			started.Done()
			pump(ctx, t, pub, faceDesc)
		}()
	})
	face, err := playPath(t, s.Dial, FacePath)
	if err != nil {
		t.Fatalf("play face: %v", err)
	}
	defer face.c.Close()
	started.Wait()
	waitFor(t, "face packets", func() bool { return face.video.Load() >= 10 })
	if face.audio.Load() != 0 {
		t.Fatalf("the face stream carries no audio, got %d", face.audio.Load())
	}
	// The camera stream is its own: no source yet, so a DESCRIBE with no
	// wait finds nothing, and its readers are counted apart.
	if s.Face().Readers() != 1 || s.Readers() != 0 {
		t.Fatalf("readers face %d camera %d", s.Face().Readers(), s.Readers())
	}
	if !s.Face().Stats().Publishing || s.Stats().Publishing {
		t.Fatal("stats are per stream")
	}
	// The camera comes up beside it, read on its own path.
	camDesc := sampleDesc()
	camPub, err := s.Publish(camDesc)
	if err != nil {
		t.Fatal(err)
	}
	go pump(ctx, t, camPub, camDesc)
	cam, err := play(t, s.Dial)
	if err != nil {
		t.Fatalf("play camera: %v", err)
	}
	defer cam.c.Close()
	waitFor(t, "camera packets", func() bool { return cam.video.Load() >= 10 && cam.audio.Load() >= 10 })
	if s.Readers() != 1 || s.Face().Readers() != 1 {
		t.Fatalf("readers camera %d face %d", s.Readers(), s.Face().Readers())
	}
	// An unknown path is not found.
	if _, err := playPath(t, s.Dial, "elsewhere"); err == nil {
		t.Fatal("an unknown path was served")
	}
}

func TestThePhonesPictureIsTakenOnlyWhenAsked(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet(), DescribeWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
	}}
	// No receiver: the publish is refused, and nothing else takes a
	// publisher either.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := record(ctx, s.Dial, PhonePath, desc); err == nil {
		t.Fatal("published with no receiver")
	}
	if err := record(ctx, s.Dial, Path, desc); err == nil {
		t.Fatal("published into the camera path")
	}
	// A receiver takes it: every packet, timed, and the reception closes
	// with the publisher's session.
	recv := &fakeReceiver{}
	s.SetReceiver(recv)
	rctx, rcancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- record(rctx, s.Dial, PhonePath, desc) }()
	waitFor(t, "packets received", func() bool { n, _, _ := recv.counts(); return n >= 10 })
	rcancel()
	if err := <-errc; err != nil {
		t.Fatalf("record: %v", err)
	}
	waitFor(t, "reception closed", func() bool { _, _, c := recv.counts(); return c == 1 })
	if _, timed, _ := recv.counts(); timed == 0 {
		t.Fatal("no packet carried the publisher's time")
	}
	if recv.received == nil || len(recv.received.Medias) != 1 {
		t.Fatalf("received %+v", recv.received)
	}
	// A receiver that is busy refuses, and the enclave is told so.
	recv.refuse = errors.New("busy")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := record(ctx2, s.Dial, PhonePath, desc); err == nil {
		t.Fatal("a refused publish went through")
	}
	// Reading the phone path through the tunnel is not a thing.
	if _, err := playPath(t, s.Dial, PhonePath); err == nil {
		t.Fatal("the phone path was read through the tunnel")
	}
}

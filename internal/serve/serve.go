// Package serve is the connector's own camera stream: an RTSPS server that
// exists only behind the tunnel (docs/PROTOCOL.md, section 6).
//
// The enclave names it with one fixed link, rtsps://127.0.0.1:7443/camera,
// the way it names any camera on the connector's network; the gateway then
// opens tunnel streams to 127.0.0.1:7443, and the connector answers those
// from inside the process (internal/tunnel, Dialer.Local) rather than
// dialing anything. No port is opened on the computer, so no other program
// on it can read the stream. The enclave completes TLS with this server
// through the tunnel and pins the certificate it sees, exactly as it does
// with a camera.
//
// What the stream carries is whatever one source is publishing into it: the
// computer's own camera and microphone (internal/capture) or a camera on the
// network the connector pulls (internal/camera). A DESCRIBE that arrives
// before the source is up waits for it, since sources start on demand.
//
// The stream is one continuous RTP stream to its readers whatever happens
// behind it. A publisher may hand over to a replacement (the encoder
// restarting at another bit rate) without the readers noticing: sequence
// numbers, timestamps and SSRC carry on. And when the tunnel falls behind
// (a stalled uplink), whole video frames are dropped here, at the source,
// rather than letting bytes pile up in the connection: the reader keeps
// getting audio and, once caught up, video again from a keyframe.
package serve

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"
)

const (
	// Port is the loopback port the enclave names. Nothing listens on it:
	// an OPEN for Target is answered inside the process.
	Port = 7443
	// Target is the OPEN payload the gateway sends for this stream.
	Target = "127.0.0.1:7443"
	// Path is the stream's RTSP path.
	Path = "camera"
	// Link is what the phone hands the enclave (PUT /ingest/source).
	Link = "rtsps://" + Target + "/" + Path
	// CertFile is the certificate and key, in the state directory.
	CertFile = "camera-cert.pem"
	// DefaultHandoverGrace is how long a publication waits for the
	// replacement publisher after ArmHandover before it closes.
	DefaultHandoverGrace = 8 * time.Second
	// DefaultDescribeWait is how long a DESCRIBE waits for the source to
	// start publishing (Config.DescribeWait). It outlasts the enclave's
	// relay, which waits 10 s for the answer, so that the wait is never
	// what gives up first: whenever the source comes up within the
	// reader's patience the reader gets its answer. The enclave gives the
	// path 15 s to be ready in all.
	DefaultDescribeWait = 12 * time.Second
	// MaxSourceClockSkew bounds how far a source's own clock may sit from
	// this computer's before its time is set aside for this computer's
	// (WritePacketRTPWithNTP): a network camera whose clock was never set
	// would otherwise date the stream by years, and the enclave lines the
	// stream up with the phone's by that time.
	MaxSourceClockSkew = 5 * time.Second
)

var (
	// ErrBusy is returned by Publish while another source is publishing and
	// no handover is armed.
	ErrBusy = errors.New("serve: a source is already publishing")
	// ErrIncompatible is returned by Publish when the replacement publisher's
	// medias do not match the stream's.
	ErrIncompatible = errors.New("serve: the replacement source's medias differ from the stream's")
	// ErrUnknownMedia is returned by WritePacketRTP for a media that is not
	// the publishing generation's.
	ErrUnknownMedia = errors.New("serve: media is not the publisher's")
)

// Config configures a Server.
type Config struct {
	// StateDir holds the certificate (CertFile).
	StateDir string
	// DescribeWait bounds how long a DESCRIBE waits for a source to start
	// publishing; 0 means DefaultDescribeWait.
	DescribeWait time.Duration
	// GateBacklog is the tunnel backlog at which video frames are dropped;
	// 0 means DefaultGateBacklog.
	GateBacklog time.Duration
	Logger      *slog.Logger
}

// Server is the stream's RTSPS server.
type Server struct {
	cfg  Config
	log  *slog.Logger
	leaf *x509.Certificate
	ln   *pipeListener
	srv  *gortsplib.Server

	// overflows counts reader write queues overflowing (OnStreamWriteError);
	// overflowAt is when it last happened, unix nanoseconds.
	overflows  atomic.Uint64
	overflowAt atomic.Int64
	episodes   atomic.Uint64

	mu      sync.Mutex
	stream  *gortsplib.ServerStream
	pub     *Publication
	ready   chan struct{} // closed while a source is publishing
	readers map[*gortsplib.ServerSession]struct{}
	backlog func() time.Duration
	closed  bool
}

// New loads (or creates) the certificate and starts the server.
func New(cfg Config) (*Server, error) {
	if cfg.DescribeWait == 0 {
		cfg.DescribeWait = DefaultDescribeWait
	}
	if cfg.GateBacklog == 0 {
		cfg.GateBacklog = DefaultGateBacklog
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cert, err := LoadOrCreateCertificate(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, log: cfg.Logger, leaf: cert.Leaf, ln: newPipeListener(),
		ready:   make(chan struct{}),
		readers: map[*gortsplib.ServerSession]struct{}{},
	}
	s.srv = &gortsplib.Server{
		Handler:     s,
		RTSPAddress: Target,
		TLSConfig:   &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Listen:      func(string, string) (net.Listener, error) { return s.ln, nil },
	}
	if err := s.srv.Start(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close stops the server, ending every reader and the current publication.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	pub := s.pub
	s.mu.Unlock()
	if pub != nil {
		pub.Close()
	}
	_ = s.ln.Close()
	s.srv.Close()
}

// Certificate is the leaf the enclave sees through the tunnel.
func (s *Server) Certificate() *x509.Certificate { return s.leaf }

// Fingerprint is the certificate's SHA-256, lowercase hex.
func (s *Server) Fingerprint() string { return Fingerprint(s.leaf) }

// Dial answers an OPEN for Target: it returns the connection the tunnel
// relays to the enclave; the server accepts the other end.
func (s *Server) Dial() (net.Conn, error) {
	return s.ln.dial()
}

// Readers is how many enclave sessions are playing the stream.
func (s *Server) Readers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.readers)
}

// SetBacklog tells the stream how far behind the tunnel carrying it is
// (internal/tunnel, Tunnel.Backlog); nil means no tunnel, nothing known.
// Video is dropped while the backlog is at least Config.GateBacklog.
func (s *Server) SetBacklog(fn func() time.Duration) {
	s.mu.Lock()
	s.backlog = fn
	s.mu.Unlock()
}

// congested reports whether the tunnel is behind: by the backlog its probe
// measures, or because a reader's write queue overflowed just now.
func (s *Server) congested(now time.Time) (bool, time.Duration) {
	s.mu.Lock()
	fn := s.backlog
	s.mu.Unlock()
	var backlog time.Duration
	if fn != nil {
		backlog = fn()
	}
	if backlog >= s.cfg.GateBacklog {
		return true, backlog
	}
	if at := s.overflowAt.Load(); at != 0 && now.Sub(time.Unix(0, at)) < overflowWindow {
		return true, backlog
	}
	return false, backlog
}

// Publish makes desc the stream. Packets go in through the Publication;
// its medias must be the ones in desc. One source publishes at a time,
// except that while the current publication has a handover armed
// (Publication.ArmHandover) a compatible desc joins it as the next
// generation and the same Publication is returned.
func (s *Server) Publish(desc *description.Session) (*Publication, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.pub != nil {
		if s.pub.handoverArmed() {
			if err := s.pub.join(desc); err != nil {
				return nil, err
			}
			return s.pub, nil
		}
		return nil, ErrBusy
	}
	st := &gortsplib.ServerStream{Server: s.srv, Desc: desc}
	if err := st.Initialize(); err != nil {
		return nil, err
	}
	p := &Publication{s: s, st: st, since: time.Now(), gens: 1, current: map[*description.Media]*track{}}
	for _, m := range desc.Medias {
		tr := newTrack(m)
		p.tracks = append(p.tracks, tr)
		p.current[m] = tr
	}
	s.stream = st
	s.pub = p
	close(s.ready)
	s.log.Info("stream: source publishing", "medias", mediaKinds(desc))
	return p, nil
}

// Stats describes the current publication.
type Stats struct {
	Publishing bool
	Since      time.Time
	Readers    int
	// Generations is how many publishers have fed this publication.
	Generations int
	// Packets and bytes written since the publication started, by kind.
	VideoPackets, VideoBytes uint64
	AudioPackets, AudioBytes uint64
	// VideoFramesDropped is how many video frames the gate dropped since
	// the publication started; Congested says whether it is dropping now.
	VideoFramesDropped uint64
	Congested          bool
	// Episodes counts dropping episodes; Overflows counts reader write
	// queues overflowing. Both since the server started.
	Episodes, Overflows uint64
}

// Stats reports the current publication and its readers.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	pub, readers := s.pub, len(s.readers)
	s.mu.Unlock()
	st := Stats{Readers: readers, Episodes: s.episodes.Load(), Overflows: s.overflows.Load()}
	if pub != nil {
		st.Publishing = true
		st.Since = pub.since
		st.VideoPackets, st.VideoBytes = pub.videoPackets.Load(), pub.videoBytes.Load()
		st.AudioPackets, st.AudioBytes = pub.audioPackets.Load(), pub.audioBytes.Load()
		st.VideoFramesDropped = pub.droppedFrames.Load()
		st.Congested = pub.congested.Load()
		pub.mu.Lock()
		st.Generations = pub.gens
		pub.mu.Unlock()
	}
	return st
}

// Publication is one source's hold on the stream, across every generation
// of publisher that feeds it.
type Publication struct {
	s      *Server
	st     *gortsplib.ServerStream
	since  time.Time
	tracks []*track

	videoPackets, videoBytes atomic.Uint64
	audioPackets, audioBytes atomic.Uint64
	droppedFrames            atomic.Uint64
	congested                atomic.Bool
	clockOff                 atomic.Bool
	closeOnce                sync.Once

	mu       sync.Mutex
	current  map[*description.Media]*track // the publishing generation's medias
	gens     int
	handover time.Time   // zero: none armed; else the replacement is due by then
	grace    *time.Timer // closes the publication if the replacement never comes
	released bool        // the current generation's publisher has left
	owed     int         // releases still to come from publishers already replaced
	wmu      sync.Mutex  // serialises writes across a generation change
}

// WritePacketRTP sends a packet to every reader, with the stream's
// continuity applied and, for video, subject to the gate. The packet is
// timed at this moment: WritePacketRTPWithNTP with a zero time.
func (p *Publication) WritePacketRTP(medi *description.Media, pkt *rtp.Packet) error {
	return p.WritePacketRTPWithNTP(medi, pkt, time.Time{})
}

// WritePacketRTPWithNTP is WritePacketRTP with the moment the packet's
// frame belongs to on the source's own clock: what its RTCP sender report
// said (ffmpeg's for the computer's camera, the camera's for a network
// camera). The readers' sender reports carry it on, and the enclave lines
// this stream up with the phone's - which reports the same way - by that
// time. A zero ntp, or one more than MaxSourceClockSkew from this
// computer's clock, is replaced by this moment.
func (p *Publication) WritePacketRTPWithNTP(medi *description.Media, pkt *rtp.Packet, ntp time.Time) error {
	p.mu.Lock()
	tr := p.current[medi]
	p.mu.Unlock()
	if tr == nil {
		return ErrUnknownMedia
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	now := time.Now()
	if ntp.IsZero() {
		ntp = now
	} else if skew := ntp.Sub(now); skew > MaxSourceClockSkew || skew < -MaxSourceClockSkew {
		if p.clockOff.CompareAndSwap(false, true) {
			p.s.log.Warn("stream: the source's clock is off; timing the stream by this computer's",
				"off", skew.Round(time.Second).String())
		}
		ntp = now
	}
	if tr.video {
		congested, backlog := p.s.congested(now)
		ok, ended := tr.admit(pkt, congested, backlog, now)
		if ended != nil {
			p.congested.Store(false)
			p.s.log.Warn("stream: congestion over; video frames were dropped",
				"frames", ended.frames, "packets", ended.packets, "over", ended.over.Round(100*time.Millisecond).String(),
				"peak_backlog", ended.peak.Round(100*time.Millisecond).String())
		}
		if !ok {
			if tr.droppedPackets == 1 {
				// The first packet dropped opens an episode.
				p.s.episodes.Add(1)
				p.congested.Store(true)
			}
			p.droppedFrames.Store(sumDropped(p.tracks))
			return nil
		}
	}
	out := tr.stamp(pkt, now)
	n := uint64(out.MarshalSize())
	if tr.video {
		p.videoPackets.Add(1)
		p.videoBytes.Add(n)
	} else if tr.media.Type == description.MediaTypeAudio {
		p.audioPackets.Add(1)
		p.audioBytes.Add(n)
	}
	return p.st.WritePacketRTPWithNTP(tr.media, out, ntp)
}

func sumDropped(tracks []*track) uint64 {
	var n uint64
	for _, t := range tracks {
		n += t.droppedFrames
	}
	return n
}

// ArmHandover announces that the publisher is about to be replaced: when
// it leaves (Release), the publication stays for up to grace (0 means
// DefaultHandoverGrace) waiting for a compatible publisher to Publish, which
// joins it without the readers noticing. If none comes, it closes.
func (p *Publication) ArmHandover(grace time.Duration) {
	if grace == 0 {
		grace = DefaultHandoverGrace
	}
	p.mu.Lock()
	p.handover = time.Now().Add(grace)
	p.mu.Unlock()
}

// Release is the publisher leaving: the publication closes unless a
// handover is armed, in which case it waits for the replacement. A release
// from a publisher whose replacement has already joined is ignored.
func (p *Publication) Release() {
	now := time.Now()
	p.mu.Lock()
	if p.owed > 0 {
		p.owed--
		p.mu.Unlock()
		return
	}
	if p.handover.IsZero() || !now.Before(p.handover) {
		p.mu.Unlock()
		p.Close()
		return
	}
	p.released = true
	wait := p.handover.Sub(now)
	gen := p.gens
	if p.grace != nil {
		p.grace.Stop()
	}
	p.grace = time.AfterFunc(wait, func() {
		p.mu.Lock()
		stale := !p.handover.IsZero() && p.gens == gen
		p.mu.Unlock()
		if stale {
			p.s.log.Warn("stream: the replacement source did not come; ending the stream")
			p.Close()
		}
	})
	p.mu.Unlock()
	p.s.log.Info("stream: source left; waiting for its replacement", "grace", wait.Round(time.Second).String())
}

func (p *Publication) handoverArmed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.handover.IsZero() && time.Now().Before(p.handover)
}

// join makes desc's medias the publishing generation. Caller holds s.mu.
func (p *Publication) join(desc *description.Session) error {
	if err := compatible(p.tracks, desc); err != nil {
		return err
	}
	p.mu.Lock()
	if p.grace != nil {
		p.grace.Stop()
		p.grace = nil
	}
	p.handover = time.Time{}
	if !p.released {
		p.owed++ // the previous publisher's release is still to come
	}
	p.released = false
	p.gens++
	gens := p.gens
	p.current = map[*description.Media]*track{}
	for i, m := range desc.Medias {
		p.current[m] = p.tracks[i]
	}
	p.mu.Unlock()
	p.wmu.Lock()
	for _, tr := range p.tracks {
		tr.fresh = true
		tr.inHave = false
	}
	p.wmu.Unlock()
	p.s.log.Info("stream: source replaced; readers keep the same stream", "generation", gens)
	return nil
}

// compatible checks that desc has the stream's medias: same count, types,
// payload types, codecs and clock rates, so packets can carry on.
func compatible(tracks []*track, desc *description.Session) error {
	if len(desc.Medias) != len(tracks) {
		return fmt.Errorf("%w: %d medias, stream has %d", ErrIncompatible, len(desc.Medias), len(tracks))
	}
	for i, m := range desc.Medias {
		want := tracks[i].media
		if m.Type != want.Type || len(m.Formats) == 0 || len(want.Formats) == 0 {
			return fmt.Errorf("%w: media %d", ErrIncompatible, i)
		}
		a, b := m.Formats[0], want.Formats[0]
		if a.PayloadType() != b.PayloadType() || a.Codec() != b.Codec() || a.ClockRate() != b.ClockRate() {
			return fmt.Errorf("%w: media %d is %s/%d, stream has %s/%d", ErrIncompatible, i, a.Codec(), a.PayloadType(), b.Codec(), b.PayloadType())
		}
	}
	return nil
}

// Close ends the publication: readers are disconnected and the next
// DESCRIBE waits for the next source.
func (p *Publication) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		if p.grace != nil {
			p.grace.Stop()
			p.grace = nil
		}
		p.handover = time.Time{}
		p.mu.Unlock()
		p.s.mu.Lock()
		if p.s.stream == p.st {
			p.s.stream, p.s.pub = nil, nil
			p.s.ready = make(chan struct{})
		}
		p.s.mu.Unlock()
		p.st.Close()
		p.s.log.Info("stream: source stopped", "video_packets", p.videoPackets.Load(), "audio_packets", p.audioPackets.Load(),
			"video_frames_dropped", p.droppedFrames.Load())
	})
}

// --- RTSP handler -----------------------------------------------------------

// current returns the stream, waiting up to DescribeWait for a source when
// none is publishing yet.
func (s *Server) current(wait bool) *gortsplib.ServerStream {
	s.mu.Lock()
	st, ready := s.stream, s.ready
	s.mu.Unlock()
	if st != nil || !wait {
		return st
	}
	select {
	case <-ready:
	case <-time.After(s.cfg.DescribeWait):
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

// OnDescribe answers the enclave's DESCRIBE, waiting for the source if it is
// still starting.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if ctx.Path != "/"+Path {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	st := s.current(true)
	if st == nil {
		s.log.Warn("stream: described with no source publishing")
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnSetup lets a reader set up the stream.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if ctx.Path != "/"+Path {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	st := s.current(false)
	if st == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnPlay counts the reader.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	s.mu.Lock()
	s.readers[ctx.Session] = struct{}{}
	n := len(s.readers)
	s.mu.Unlock()
	s.log.Info("stream: enclave reading", "readers", n)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnAnnounce refuses publishers: sources publish from inside the process.
func (s *Server) OnAnnounce(*gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusMethodNotAllowed}, nil
}

// OnSessionClose uncounts a reader.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	_, was := s.readers[ctx.Session]
	delete(s.readers, ctx.Session)
	n := len(s.readers)
	s.mu.Unlock()
	if was {
		s.log.Info("stream: enclave stopped reading", "readers", n)
	}
}

// OnStreamWriteError is a reader's write queue overflowing: the tunnel is
// not taking packets as fast as they come. It is counted, not logged per
// packet (gortsplib would print every one), and the gate treats it as
// congestion for a moment (overflowWindow) even when no probe says so.
func (s *Server) OnStreamWriteError(*gortsplib.ServerHandlerOnStreamWriteErrorCtx) {
	s.overflows.Add(1)
	s.overflowAt.Store(time.Now().UnixNano())
}

func mediaKinds(desc *description.Session) []string {
	kinds := make([]string, 0, len(desc.Medias))
	for _, m := range desc.Medias {
		kinds = append(kinds, string(m.Type))
	}
	return kinds
}

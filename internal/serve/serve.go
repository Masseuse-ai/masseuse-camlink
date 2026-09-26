// Package serve is the connector's own streams: an RTSPS server that exists
// only behind the tunnel (docs/PROTOCOL.md, section 6).
//
// The enclave names it with fixed links, rtsps://127.0.0.1:7443/camera and
// rtsps://127.0.0.1:7443/face, the way it names any camera on the
// connector's network; the gateway then opens tunnel streams to
// 127.0.0.1:7443, and the connector answers those from inside the process
// (internal/tunnel, Dialer.Local) rather than dialing anything. No port is
// opened on the computer, so no other program on it can read the streams.
// The enclave completes TLS with this server through the tunnel and pins
// the certificate it sees, exactly as it does with a camera.
//
// Two streams are served, each a path: `camera`, the computer's own camera
// and microphone (internal/capture) or a camera on the network the
// connector pulls (internal/camera), the session's body view; and `face`,
// a second camera of the computer's chosen as the person's front-facing
// picture (a virtual camera such as OBS's, or any other), served only when
// one is configured. A DESCRIBE that arrives before a source is up waits
// for it, since sources start on demand.
//
// One path is taken rather than served: `phone`. When the person asks for
// it on this computer (Receiver set), the enclave publishes the phone's own
// picture into it through the tunnel, and the receiver (internal/share)
// hands it to a program on this computer. Nothing is published into any
// other path; without a receiver, nothing is published at all.
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
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

const (
	// Port is the loopback port the enclave names. Nothing listens on it:
	// an OPEN for Target is answered inside the process.
	Port = 7443
	// Target is the OPEN payload the gateway sends for this stream.
	Target = "127.0.0.1:7443"
	// Path is the body camera stream's RTSP path.
	Path = "camera"
	// Link is what the phone hands the enclave (PUT /ingest/source).
	Link = "rtsps://" + Target + "/" + Path
	// FacePath is the front-facing camera stream's RTSP path.
	FacePath = "face"
	// FaceLink is what the phone hands the enclave for it (PUT
	// /ingest/face-source).
	FaceLink = "rtsps://" + Target + "/" + FacePath
	// PhonePath is the path the enclave publishes the phone's picture into,
	// when a Receiver is set.
	PhonePath = "phone"
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
	// ErrNotAsked is what a publisher into the phone path gets while no
	// Receiver is set: the person has not asked for the phone's picture on
	// this computer.
	ErrNotAsked = errors.New("serve: the phone's picture was not asked for on this computer")
)

// A PacketWriter takes a source's packets, each with the moment its frame
// belongs to on the source's clock: a Publication, or a Reception.
type PacketWriter interface {
	WritePacketRTPWithNTP(medi *description.Media, pkt *rtp.Packet, ntp time.Time) error
}

// A Receiver takes the phone's picture as the enclave publishes it into the
// phone path (internal/share): Publish is called with the enclave's
// description when it announces, and the Reception returned gets every
// packet and is closed when the enclave's session ends.
type Receiver interface {
	Publish(desc *description.Session) (Reception, error)
}

// A Reception is one publication into a Receiver.
type Reception interface {
	PacketWriter
	Close()
}

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

// Server is the streams' RTSPS server.
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
	streams map[string]*Stream // by path: Path, FacePath
	backlog func() time.Duration
	closed  bool
	// receiver takes the phone's picture published into PhonePath; nil
	// refuses the publish. receptions are the publishing sessions' takes.
	receiver   Receiver
	receptions map[*gortsplib.ServerSession]Reception
}

// A Stream is one served path: what one source publishes into it, and who
// is reading it. The camera stream is the Server's own methods' (Publish,
// Stats, Readers); the face stream is Face().
type Stream struct {
	s    *Server
	path string

	mu      sync.Mutex
	stream  *gortsplib.ServerStream
	pub     *Publication
	ready   chan struct{} // closed while a source is publishing
	readers map[*gortsplib.ServerSession]struct{}
	// onDemand, when set, is called at a DESCRIBE that finds no source
	// publishing: the source's chance to start (SetOnDemand).
	onDemand func()
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
		streams:    map[string]*Stream{},
		receptions: map[*gortsplib.ServerSession]Reception{},
	}
	for _, path := range []string{Path, FacePath} {
		s.streams[path] = &Stream{s: s, path: path, ready: make(chan struct{}), readers: map[*gortsplib.ServerSession]struct{}{}}
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

// Close stops the server, ending every reader, every publication and any
// reception.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	streams := make([]*Stream, 0, len(s.streams))
	for _, st := range s.streams {
		streams = append(streams, st)
	}
	receptions := make([]Reception, 0, len(s.receptions))
	for _, r := range s.receptions {
		receptions = append(receptions, r)
	}
	s.receptions = map[*gortsplib.ServerSession]Reception{}
	s.mu.Unlock()
	for _, st := range streams {
		st.mu.Lock()
		pub := st.pub
		st.mu.Unlock()
		if pub != nil {
			pub.Close()
		}
	}
	for _, r := range receptions {
		r.Close()
	}
	_ = s.ln.Close()
	s.srv.Close()
}

// Camera is the body camera stream (Path); the Server's own Publish, Stats
// and Readers are its.
func (s *Server) Camera() *Stream { return s.streams[Path] }

// Face is the front-facing camera stream (FacePath).
func (s *Server) Face() *Stream { return s.streams[FacePath] }

// Stream returns the served stream at path (Path or FacePath), or nil.
func (s *Server) Stream(path string) *Stream { return s.streams[path] }

// SetReceiver names what takes the phone's picture the enclave publishes
// into PhonePath; nil (the default) refuses the publish with ErrNotAsked.
func (s *Server) SetReceiver(r Receiver) {
	s.mu.Lock()
	s.receiver = r
	s.mu.Unlock()
}

// Path is the stream's RTSP path.
func (st *Stream) Path() string { return st.path }

// SetOnDemand names what to do when a DESCRIBE finds no source publishing
// into this stream: start one. It is called at most once per such
// DESCRIBE, before the wait for the source; nil (the default) does nothing.
func (st *Stream) SetOnDemand(start func()) {
	st.mu.Lock()
	st.onDemand = start
	st.mu.Unlock()
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

// Readers is how many enclave sessions are playing the camera stream.
func (s *Server) Readers() int { return s.Camera().Readers() }

// Readers is how many enclave sessions are playing the stream.
func (st *Stream) Readers() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.readers)
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

// Publish makes desc the camera stream (Stream.Publish on Camera()).
func (s *Server) Publish(desc *description.Session) (*Publication, error) {
	return s.Camera().Publish(desc)
}

// Publish makes desc the stream. Packets go in through the Publication;
// its medias must be the ones in desc. One source publishes at a time,
// except that while the current publication has a handover armed
// (Publication.ArmHandover) a compatible desc joins it as the next
// generation and the same Publication is returned.
func (st *Stream) Publish(desc *description.Session) (*Publication, error) {
	s := st.s
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pub != nil {
		if st.pub.handoverArmed() {
			if err := st.pub.join(desc); err != nil {
				return nil, err
			}
			return st.pub, nil
		}
		return nil, ErrBusy
	}
	ss := &gortsplib.ServerStream{Server: s.srv, Desc: desc}
	if err := ss.Initialize(); err != nil {
		return nil, err
	}
	p := &Publication{s: s, str: st, st: ss, since: time.Now(), gens: 1, current: map[*description.Media]*track{}}
	for _, m := range desc.Medias {
		tr := newTrack(m)
		p.tracks = append(p.tracks, tr)
		p.current[m] = tr
	}
	st.stream = ss
	st.pub = p
	close(st.ready)
	s.log.Info("stream: source publishing", "path", st.path, "medias", mediaKinds(desc))
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

// Stats reports the camera stream's publication and its readers.
func (s *Server) Stats() Stats { return s.Camera().Stats() }

// Stats reports the current publication and its readers.
func (str *Stream) Stats() Stats {
	s := str.s
	str.mu.Lock()
	pub, readers := str.pub, len(str.readers)
	str.mu.Unlock()
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
	str    *Stream
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
		p.str.mu.Lock()
		if p.str.stream == p.st {
			p.str.stream, p.str.pub = nil, nil
			p.str.ready = make(chan struct{})
		}
		p.str.mu.Unlock()
		p.st.Close()
		p.s.log.Info("stream: source stopped", "path", p.str.path, "video_packets", p.videoPackets.Load(), "audio_packets", p.audioPackets.Load(),
			"video_frames_dropped", p.droppedFrames.Load())
	})
}

// --- RTSP handler -----------------------------------------------------------

// current returns the stream, waiting up to DescribeWait for a source when
// none is publishing yet; with wait, a DESCRIBE that finds none first gives
// the source its chance to start (SetOnDemand).
func (st *Stream) current(wait bool) *gortsplib.ServerStream {
	st.mu.Lock()
	ss, ready, start := st.stream, st.ready, st.onDemand
	st.mu.Unlock()
	if ss != nil || !wait {
		return ss
	}
	if start != nil {
		start()
	}
	select {
	case <-ready:
	case <-time.After(st.s.cfg.DescribeWait):
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.stream
}

// streamFor is the served stream a request's path names, or nil.
func (s *Server) streamFor(path string) *Stream {
	if len(path) == 0 || path[0] != '/' {
		return nil
	}
	return s.streams[path[1:]]
}

// OnDescribe answers the enclave's DESCRIBE, waiting for the source if it is
// still starting.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	str := s.streamFor(ctx.Path)
	if str == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	ss := str.current(true)
	if ss == nil {
		s.log.Warn("stream: described with no source publishing", "path", str.path)
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, ss, nil
}

// OnSetup lets a reader set up a served stream, or the enclave set up the
// phone publication it announced.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if ctx.Session.State() == gortsplib.ServerSessionStatePreRecord {
		s.mu.Lock()
		_, publishing := s.receptions[ctx.Session]
		s.mu.Unlock()
		if !publishing || ctx.Path != "/"+PhonePath {
			return &base.Response{StatusCode: base.StatusMethodNotAllowed}, nil, nil
		}
		return &base.Response{StatusCode: base.StatusOK}, nil, nil
	}
	str := s.streamFor(ctx.Path)
	if str == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	ss := str.current(false)
	if ss == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, ss, nil
}

// OnPlay counts the reader.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	str := s.streamFor(ctx.Path)
	if str == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}
	str.mu.Lock()
	str.readers[ctx.Session] = struct{}{}
	n := len(str.readers)
	str.mu.Unlock()
	s.log.Info("stream: enclave reading", "path", str.path, "readers", n)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnAnnounce takes the enclave's publication of the phone's picture into
// the receiver, when one is set: the person asked for the picture on this
// computer. Any other path, and the phone path without a receiver, is
// refused: the served streams' sources publish from inside the process.
func (s *Server) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	if ctx.Path != "/"+PhonePath {
		return &base.Response{StatusCode: base.StatusMethodNotAllowed}, nil
	}
	s.mu.Lock()
	r := s.receiver
	s.mu.Unlock()
	if r == nil {
		s.log.Warn("stream: the enclave offered the phone's picture; not asked for on this computer")
		return &base.Response{StatusCode: base.StatusForbidden}, nil
	}
	rec, err := r.Publish(ctx.Description)
	if err != nil {
		s.log.Warn("stream: the phone's picture was refused", "err", err)
		return &base.Response{StatusCode: base.StatusServiceUnavailable}, nil
	}
	s.mu.Lock()
	s.receptions[ctx.Session] = rec
	s.mu.Unlock()
	s.log.Info("stream: the phone's picture arriving", "medias", mediaKinds(ctx.Description))
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnRecord forwards the phone's packets into the reception, each timed by
// the enclave's sender reports (the phone's own frame times, kept by the
// enclave's relay), through a TimedWriter as the capture's intake does.
func (s *Server) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	s.mu.Lock()
	rec := s.receptions[ctx.Session]
	s.mu.Unlock()
	if rec == nil {
		return &base.Response{StatusCode: base.StatusMethodNotValidInThisState}, nil
	}
	w := NewTimedWriter(rec, ctx.Session.PacketNTP)
	var dropped atomic.Uint64
	ctx.Session.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		if err := w.Write(medi, pkt); err != nil {
			if n := dropped.Add(1); n == 1 || n%1000 == 0 {
				s.log.Warn("stream: a packet of the phone's picture was not forwarded", "err", err, "dropped", n)
			}
		}
	})
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnSessionClose uncounts a reader, or ends the reception a publishing
// session fed.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	for _, str := range s.streams {
		str.mu.Lock()
		_, was := str.readers[ctx.Session]
		delete(str.readers, ctx.Session)
		n := len(str.readers)
		str.mu.Unlock()
		if was {
			s.log.Info("stream: enclave stopped reading", "path", str.path, "readers", n)
		}
	}
	s.mu.Lock()
	rec, publishing := s.receptions[ctx.Session]
	delete(s.receptions, ctx.Session)
	s.mu.Unlock()
	if publishing {
		rec.Close()
		s.log.Info("stream: the phone's picture stopped")
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

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
package serve

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
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
)

// ErrBusy is returned by Publish while another source is publishing.
var ErrBusy = errors.New("serve: a source is already publishing")

// Config configures a Server.
type Config struct {
	// StateDir holds the certificate (CertFile).
	StateDir string
	// DescribeWait bounds how long a DESCRIBE waits for a source to start
	// publishing; 0 means 8 s. The enclave gives the path 15 s to be ready.
	DescribeWait time.Duration
	Logger       *slog.Logger
}

// Server is the stream's RTSPS server.
type Server struct {
	cfg  Config
	log  *slog.Logger
	leaf *x509.Certificate
	ln   *pipeListener
	srv  *gortsplib.Server

	mu      sync.Mutex
	stream  *gortsplib.ServerStream
	pub     *Publication
	ready   chan struct{} // closed while a source is publishing
	readers map[*gortsplib.ServerSession]struct{}
	closed  bool
}

// New loads (or creates) the certificate and starts the server.
func New(cfg Config) (*Server, error) {
	if cfg.DescribeWait == 0 {
		cfg.DescribeWait = 8 * time.Second
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

// Publish makes desc the stream. Packets go in through the Publication;
// its medias must be the ones in desc. One source publishes at a time.
func (s *Server) Publish(desc *description.Session) (*Publication, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.stream != nil {
		return nil, ErrBusy
	}
	st := &gortsplib.ServerStream{Server: s.srv, Desc: desc}
	if err := st.Initialize(); err != nil {
		return nil, err
	}
	s.stream = st
	s.pub = &Publication{s: s, st: st, since: time.Now()}
	close(s.ready)
	s.log.Info("stream: source publishing", "medias", mediaKinds(desc))
	return s.pub, nil
}

// Stats describes the current publication.
type Stats struct {
	Publishing bool
	Since      time.Time
	Readers    int
	// Packets and bytes written since the publication started, by kind.
	VideoPackets, VideoBytes uint64
	AudioPackets, AudioBytes uint64
}

// Stats reports the current publication and its readers.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	pub, readers := s.pub, len(s.readers)
	s.mu.Unlock()
	st := Stats{Readers: readers}
	if pub != nil {
		st.Publishing = true
		st.Since = pub.since
		st.VideoPackets, st.VideoBytes = pub.videoPackets.Load(), pub.videoBytes.Load()
		st.AudioPackets, st.AudioBytes = pub.audioPackets.Load(), pub.audioBytes.Load()
	}
	return st
}

// Publication is one source's hold on the stream.
type Publication struct {
	s     *Server
	st    *gortsplib.ServerStream
	since time.Time

	videoPackets, videoBytes atomic.Uint64
	audioPackets, audioBytes atomic.Uint64
	closeOnce                sync.Once
}

// WritePacketRTP sends a packet to every reader.
func (p *Publication) WritePacketRTP(medi *description.Media, pkt *rtp.Packet) error {
	n := uint64(pkt.MarshalSize())
	if medi.Type == description.MediaTypeVideo {
		p.videoPackets.Add(1)
		p.videoBytes.Add(n)
	} else if medi.Type == description.MediaTypeAudio {
		p.audioPackets.Add(1)
		p.audioBytes.Add(n)
	}
	return p.st.WritePacketRTP(medi, pkt)
}

// Close ends the publication: readers are disconnected and the next
// DESCRIBE waits for the next source.
func (p *Publication) Close() {
	p.closeOnce.Do(func() {
		p.s.mu.Lock()
		if p.s.stream == p.st {
			p.s.stream, p.s.pub = nil, nil
			p.s.ready = make(chan struct{})
		}
		p.s.mu.Unlock()
		p.st.Close()
		p.s.log.Info("stream: source stopped", "video_packets", p.videoPackets.Load(), "audio_packets", p.audioPackets.Load())
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

func mediaKinds(desc *description.Session) []string {
	kinds := make([]string, 0, len(desc.Medias))
	for _, m := range desc.Medias {
		kinds = append(kinds, string(m.Type))
	}
	return kinds
}

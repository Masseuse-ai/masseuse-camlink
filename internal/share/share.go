// Package share hands the phone's picture to a program on this computer.
//
// When the person asks for it (`-share-phone`), the enclave publishes the
// phone's own camera picture into the connector's phone path through the
// tunnel (internal/serve, Receiver), and this package serves it again on a
// loopback RTSP address another program may open: OBS Studio's Media
// Source, typically, so that filters can be applied to the picture and the
// result sent back as the front-facing camera (`-face-camera`). It is the
// one socket this program opens for others; the path on it is a secret,
// kept with the camera choice so that an OBS scene set up once keeps
// working, and the server refuses everything but reading. The picture is
// the person's own, arriving on their own computer at their own asking.
//
// Readers get the packets as the enclave sent them, timed by the phone's
// own frame times where the enclave's relay kept them: the picture is
// neither decoded nor changed here.
package share

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"
)

const (
	// DefaultPort is the loopback port the picture is served on.
	DefaultPort = 7446
	// pathPrefix begins the secret path: what a person sees in the address.
	pathPrefix = "phone-"
)

// ErrBusy is returned by Publish while a picture is already arriving.
var ErrBusy = errors.New("share: the phone's picture is already arriving")

// NewSecret makes a fresh path secret: 18 random bytes, base64url.
func NewSecret() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("share: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Config configures a Server.
type Config struct {
	// Port is the loopback port to listen on; 0 means DefaultPort, and a
	// negative port one the system chooses (tests).
	Port int
	// Secret is the path's secret (NewSecret); required.
	Secret string
	// OnChange, when set, is called with true when a picture starts
	// arriving and false when it stops.
	OnChange func(arriving bool)
	Logger   *slog.Logger
}

// Server is the loopback RTSP server the picture is read from.
type Server struct {
	srv  *gortsplib.Server
	path string
	url  string
	log  *slog.Logger
	on   func(bool)

	mu      sync.Mutex
	stream  *gortsplib.ServerStream
	rec     *reception
	readers map[*gortsplib.ServerSession]struct{}
	closed  bool
}

// New starts the server on 127.0.0.1:port. It implements serve.Receiver.
func New(cfg Config) (*Server, error) {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.Port < 0 {
		cfg.Port = 0
	}
	if cfg.Secret == "" {
		return nil, errors.New("share: a path secret is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		path: "/" + pathPrefix + cfg.Secret, log: cfg.Logger, on: cfg.OnChange,
		readers: map[*gortsplib.ServerSession]struct{}{},
	}
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(cfg.Port))
	s.srv = &gortsplib.Server{Handler: s, RTSPAddress: addr}
	if err := s.srv.Start(); err != nil {
		return nil, fmt.Errorf("share: listen on %s: %w", addr, err)
	}
	s.url = "rtsp://" + s.srv.NetListener().Addr().String() + s.path
	return s, nil
}

// URL is the address a program on this computer opens.
func (s *Server) URL() string { return s.url }

// Port is the loopback port the server listens on.
func (s *Server) Port() int {
	if addr, ok := s.srv.NetListener().Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// Arriving reports whether the phone's picture is arriving now.
func (s *Server) Arriving() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream != nil
}

// Readers is how many programs are reading the picture.
func (s *Server) Readers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.readers)
}

// Close stops the server, ending any reception and every reader.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	rec := s.rec
	s.mu.Unlock()
	if rec != nil {
		rec.Close()
	}
	s.srv.Close()
}

// Publish takes the enclave's description of the phone's picture and
// returns the reception its packets go into (serve.Receiver). One picture
// arrives at a time.
func (s *Server) Publish(desc *description.Session) (serve.Reception, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, serve.ErrClosed
	}
	if s.stream != nil {
		return nil, ErrBusy
	}
	st := &gortsplib.ServerStream{Server: s.srv, Desc: desc}
	if err := st.Initialize(); err != nil {
		return nil, err
	}
	rec := &reception{s: s, st: st}
	s.stream, s.rec = st, rec
	s.log.Info("share: the phone's picture arriving", "url", s.url)
	if s.on != nil {
		go s.on(true)
	}
	return rec, nil
}

// reception is one publication of the phone's picture.
type reception struct {
	s    *Server
	st   *gortsplib.ServerStream
	once sync.Once
}

// WritePacketRTPWithNTP hands a packet to every reader, with the phone's
// time for it when the enclave's report gave one.
func (r *reception) WritePacketRTPWithNTP(medi *description.Media, pkt *rtp.Packet, ntp time.Time) error {
	if ntp.IsZero() {
		ntp = time.Now()
	}
	return r.st.WritePacketRTPWithNTP(medi, pkt, ntp)
}

// Close ends the reception: readers are disconnected and the next DESCRIBE
// finds nothing until the enclave publishes again.
func (r *reception) Close() {
	r.once.Do(func() {
		r.s.mu.Lock()
		if r.s.stream == r.st {
			r.s.stream, r.s.rec = nil, nil
		}
		r.s.mu.Unlock()
		r.st.Close()
		r.s.log.Info("share: the phone's picture stopped")
		if r.s.on != nil {
			go r.s.on(false)
		}
	})
}

// --- RTSP handler: readers only -------------------------------------------------

func (s *Server) current(path string) *gortsplib.ServerStream {
	if path != s.path {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

// OnDescribe answers a reader that names the secret path while a picture
// is arriving; anything else is not found.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	st := s.current(ctx.Path)
	if st == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnSetup lets a reader set the stream up; nobody publishes here.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if ctx.Session.State() == gortsplib.ServerSessionStatePreRecord {
		return &base.Response{StatusCode: base.StatusMethodNotAllowed}, nil, nil
	}
	st := s.current(ctx.Path)
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
	s.log.Info("share: a program is reading the phone's picture", "readers", n)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnAnnounce refuses publishers: the picture comes through the tunnel.
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
		s.log.Info("share: a program stopped reading the phone's picture", "readers", n)
	}
}

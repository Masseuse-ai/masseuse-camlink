// Package gateway is the enclave side of the tunnel (docs/PROTOCOL.md,
// sections 4 and 5): it accepts exactly one connector, authenticated by the
// ticket the service brokered and by an Ed25519 challenge against the
// connector key the service named, and relays loopback connections from the
// enclave's RTSP server to that connector as OPEN streams.
//
// Nothing here decrypts anything: the RTSP server terminates the camera's
// TLS itself. The gateway sees ciphertext and the target address, and it
// never logs the latter.
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/FemLed/masseuse-camlink/internal/frame"
	"github.com/FemLed/masseuse-camlink/internal/identity"
	"github.com/FemLed/masseuse-camlink/internal/mux"
)

// Subprotocol is the WebSocket subprotocol both ends require.
const Subprotocol = "camlink.v1"

// Config sizes a Server.
type Config struct {
	// WSAddr is where Caddy proxies /ingest/tunnel to (127.0.0.1:8090).
	WSAddr string
	// RelayAddr is what the RTSP server dials as the camera (127.0.0.1:7441).
	RelayAddr string
	// ControlAddr is the producer-facing control API (127.0.0.1:8091).
	ControlAddr string
	// PingInterval keeps the WebSocket alive through Caddy; 0 means 30 s.
	PingInterval time.Duration
	// RelayBuffer is the copy buffer per direction; 0 means 256 KiB.
	RelayBuffer int
	// OpenTimeout bounds how long a relay connection waits for the connector
	// to answer OPEN; 0 means 15 s.
	OpenTimeout time.Duration
	// HandshakeTimeout bounds the CHALLENGE/PROVE exchange; 0 means 10 s.
	HandshakeTimeout time.Duration
	Logger           *slog.Logger
}

type expectation struct {
	key           ed25519.PublicKey
	keyStr        string
	ticketHash    [32]byte
	ticketHashHex string
	expiresAt     time.Time
	attachedOnce  bool
}

type attached struct {
	sess   *mux.Session
	keyStr string
	since  time.Time
	cancel context.CancelFunc
}

// Server is one gateway instance.
type Server struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	expect   *expectation
	attached *attached
	target   string
	now      func() time.Time
}

// New builds a Server; call Run or serve the handlers yourself.
func New(cfg Config) *Server {
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.RelayBuffer == 0 {
		cfg.RelayBuffer = 256 << 10
	}
	if cfg.OpenTimeout == 0 {
		cfg.OpenTimeout = 15 * time.Second
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{cfg: cfg, log: cfg.Logger, now: time.Now}
}

// Run serves the three listeners until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	relayLn, err := net.Listen("tcp", s.cfg.RelayAddr)
	if err != nil {
		return fmt.Errorf("relay listener: %w", err)
	}
	defer relayLn.Close()
	wsSrv := &http.Server{Addr: s.cfg.WSAddr, Handler: s.WSHandler(), ReadHeaderTimeout: 10 * time.Second}
	ctlSrv := &http.Server{Addr: s.cfg.ControlAddr, Handler: s.ControlHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second}

	errc := make(chan error, 3)
	go func() { errc <- s.ServeRelay(ctx, relayLn) }()
	go func() { errc <- wsSrv.ListenAndServe() }()
	go func() { errc <- ctlSrv.ListenAndServe() }()
	s.log.Info("gateway listening", "ws", s.cfg.WSAddr, "relay", s.cfg.RelayAddr, "control", s.cfg.ControlAddr)

	select {
	case <-ctx.Done():
	case err = <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("gateway listener failed", "err", err)
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = wsSrv.Shutdown(shutdown)
	_ = ctlSrv.Shutdown(shutdown)
	s.dropConnector("gateway shutting down")
	return err
}

// --- tunnel (WebSocket) ------------------------------------------------------

// WSHandler accepts connector tunnels at any path (Caddy routes
// /ingest/tunnel here).
func (s *Server) WSHandler() http.Handler {
	return http.HandlerFunc(s.handleTunnel)
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	exp := s.expectationFor(bearer(r))
	if exp == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       []string{Subprotocol},
		InsecureSkipVerify: true, // not a browser client: no Origin to check
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("tunnel: websocket accept failed", "err", err)
		return
	}
	if c.Subprotocol() != Subprotocol {
		c.Close(websocket.StatusProtocolError, "subprotocol "+Subprotocol+" required")
		return
	}
	c.SetReadLimit(frame.HeaderSize + frame.MaxPayload)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)

	if err := s.handshake(conn, exp, hostOnly(r.Host)); err != nil {
		s.log.Warn("tunnel: handshake failed", "connector", prefix(exp.keyStr), "err", err)
		_ = frame.NewWriter(conn).Write(frame.Frame{Type: frame.TypeReset, Stream: 0, Payload: []byte(err.Error())})
		_ = conn.Close()
		return
	}
	sess := mux.New(conn, true)
	s.attach(exp, sess, cancel)
	s.log.Info("tunnel: connector attached", "connector", prefix(exp.keyStr))

	go s.pingLoop(ctx, c, sess)
	<-sess.Done()
	s.detach(sess)
	s.log.Info("tunnel: connector detached", "connector", prefix(exp.keyStr), "reason", errString(sess.Err()))
}

// expectationFor returns the current expectation if the ticket matches it
// and is still valid for a first attach.
func (s *Server) expectationFor(ticket string) *expectation {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(ticket, "="))
	if err != nil || len(raw) == 0 {
		return nil
	}
	h := sha256.Sum256(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	exp := s.expect
	if exp == nil {
		return nil
	}
	if subtle.ConstantTimeCompare(h[:], exp.ticketHash[:]) != 1 {
		return nil
	}
	if !exp.attachedOnce && s.now().After(exp.expiresAt) {
		return nil
	}
	return exp
}

func (s *Server) handshake(conn net.Conn, exp *expectation, host string) error {
	nonce := make([]byte, frame.ChallengeSize)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	w := frame.NewWriter(conn)
	r := frame.NewReader(conn)
	_ = conn.SetDeadline(s.now().Add(s.cfg.HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})
	if err := w.Write(frame.Frame{Type: frame.TypeChallenge, Stream: 0, Payload: nonce}); err != nil {
		return err
	}
	f, err := r.Read()
	if err != nil {
		return fmt.Errorf("waiting for PROVE: %w", err)
	}
	if f.Type != frame.TypeProve || f.Stream != 0 {
		return errors.New("expected PROVE on stream 0")
	}
	if len(f.Payload) != ed25519.SignatureSize || !ed25519.Verify(exp.key, identity.TunnelProof(host, exp.ticketHashHex, nonce), f.Payload) {
		return errors.New("bad proof")
	}
	return w.Write(frame.Frame{Type: frame.TypeReady, Stream: 0})
}

func (s *Server) attach(exp *expectation, sess *mux.Session, cancel context.CancelFunc) {
	s.mu.Lock()
	if s.expect != exp {
		// The expectation changed while we were shaking hands.
		s.mu.Unlock()
		sess.CloseWithReason("replaced")
		return
	}
	exp.attachedOnce = true
	old := s.attached
	s.attached = &attached{sess: sess, keyStr: exp.keyStr, since: s.now(), cancel: cancel}
	s.mu.Unlock()
	if old != nil {
		old.sess.CloseWithReason("replaced by a new connection")
	}
}

func (s *Server) detach(sess *mux.Session) {
	s.mu.Lock()
	if s.attached != nil && s.attached.sess == sess {
		s.attached = nil
	}
	s.mu.Unlock()
}

func (s *Server) dropConnector(reason string) {
	s.mu.Lock()
	att := s.attached
	s.attached = nil
	s.mu.Unlock()
	if att != nil {
		att.sess.CloseWithReason(reason)
	}
}

func (s *Server) pingLoop(ctx context.Context, c *websocket.Conn, sess *mux.Session) {
	t := time.NewTicker(s.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, s.cfg.PingInterval)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				s.log.Warn("tunnel: ping failed", "err", err)
				sess.Close()
				return
			}
		}
	}
}

// --- relay listener ----------------------------------------------------------

// ServeRelay accepts connections from the RTSP server and relays each one to
// the attached connector as a stream to the current target.
func (s *Server) ServeRelay(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.relay(ctx, c)
	}
}

func (s *Server) relay(ctx context.Context, c net.Conn) {
	s.mu.Lock()
	att, target := s.attached, s.target
	s.mu.Unlock()
	if att == nil || target == "" {
		s.log.Debug("relay: refused local connection", "connector", att != nil, "target", target != "")
		_ = c.Close()
		return
	}
	octx, cancel := context.WithTimeout(ctx, s.cfg.OpenTimeout)
	st, err := att.sess.Open(octx, target)
	cancel()
	if err != nil {
		s.log.Warn("relay: connector did not open the stream", "connector", prefix(att.keyStr), "err", err)
		_ = c.Close()
		return
	}
	s.log.Debug("relay: stream open", "stream", st.ID())
	mux.Relay(st, c, s.cfg.RelayBuffer)
	s.log.Debug("relay: stream closed", "stream", st.ID())
}

// --- control API -------------------------------------------------------------

type expectRequest struct {
	ConnectorKey string `json:"connectorKey"`
	TicketHash   string `json:"ticketHash"`
	ExpiresAt    int64  `json:"expiresAt"`
}

type targetRequest struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Status is GET /status.
type Status struct {
	Expecting          bool    `json:"expecting"`
	Connected          bool    `json:"connected"`
	SinceMs            *int64  `json:"sinceMs"`
	ConnectorKeyPrefix *string `json:"connectorKeyPrefix"`
	Target             bool    `json:"target"`
}

// ControlHandler is the loopback control API for the producer.
func (s *Server) ControlHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /expect", s.handleExpect)
	m.HandleFunc("POST /target", s.handleTarget)
	m.HandleFunc("POST /clear", s.handleClear)
	m.HandleFunc("GET /status", s.handleStatus)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	})
	return m
}

func (s *Server) handleExpect(w http.ResponseWriter, r *http.Request) {
	var req expectRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, err := identity.ParsePublicKey(req.ConnectorKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := hex.DecodeString(strings.ToLower(req.TicketHash))
	if err != nil || len(hash) != 32 {
		writeError(w, http.StatusBadRequest, "ticketHash must be 64 hex characters")
		return
	}
	expiresAt := time.Unix(req.ExpiresAt, 0)
	if req.ExpiresAt <= 0 || expiresAt.Before(s.now()) {
		writeError(w, http.StatusBadRequest, "expiresAt must be in the future")
		return
	}
	exp := &expectation{key: key, keyStr: req.ConnectorKey, ticketHashHex: strings.ToLower(req.TicketHash), expiresAt: expiresAt}
	copy(exp.ticketHash[:], hash)

	s.mu.Lock()
	prev := s.expect
	s.expect = exp
	att := s.attached
	// Whether this expectation names the connector the session already
	// had: the one attached now, or, with none attached, the one the
	// previous expectation named. The target is the producer's choice of
	// camera for this session, reached through that connector; it survives
	// a new ticket for the same connector whether or not the tunnel happens
	// to be up at this moment - the service re-expects on every new lease,
	// and a lease often follows the tunnel dropping (the connector's
	// uplink stalled, its process was replaced), so an expectation that
	// arrived between the drop and the re-dial used to forget the target
	// and leave the relay refused for the rest of the session.
	sameConnector := (att != nil && att.keyStr == exp.keyStr) ||
		(att == nil && prev != nil && prev.keyStr == exp.keyStr)
	switch {
	case att != nil && sameConnector:
		// Same connector, new ticket, tunnel up: keep the live tunnel and
		// the target, and treat the expectation as already attached so its
		// ticket outlives expiresAt.
		exp.attachedOnce = true
		att = nil
	case sameConnector:
		// Same connector, tunnel down: the target stays for its re-dial.
	default:
		// Another connector, or the first: a target reached through the
		// old one means nothing through the new.
		s.target = ""
		s.attached = nil
	}
	s.mu.Unlock()
	if att != nil {
		att.sess.CloseWithReason("session moved to another connector")
	}
	s.log.Info("control: expecting connector", "connector", prefix(exp.keyStr), "expiresIn", expiresAt.Sub(s.now()).Round(time.Second).String())
	writeJSON(w, http.StatusOK, map[string]string{"status": "expecting"})
}

func (s *Server) handleTarget(w http.ResponseWriter, r *http.Request) {
	var req targetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	host := strings.TrimSpace(req.Host)
	if host == "" || strings.ContainsAny(host, " /\\@") || req.Port < 1 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, "host and port (1-65535) are required")
		return
	}
	s.mu.Lock()
	if s.attached == nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "no connector attached")
		return
	}
	s.target = net.JoinHostPort(host, strconv.Itoa(req.Port))
	s.mu.Unlock()
	s.log.Info("control: target set")
	writeJSON(w, http.StatusOK, map[string]string{"status": "target set"})
}

func (s *Server) handleClear(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.expect = nil
	s.target = ""
	s.mu.Unlock()
	s.dropConnector("session cleared")
	s.log.Info("control: cleared")
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Status())
}

// Status reports the gateway's state without the target address.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Expecting: s.expect != nil, Target: s.target != ""}
	if s.attached != nil {
		st.Connected = true
		ms := s.attached.since.UnixMilli()
		st.SinceMs = &ms
		p := prefix(s.attached.keyStr)
		st.ConnectorKeyPrefix = &p
	}
	return st
}

// --- helpers -----------------------------------------------------------------

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func prefix(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Package camera pulls a camera on the connector's own network and
// republishes it through the connector's stream (internal/serve), so the
// enclave reads the same link, rtsps://127.0.0.1:7443/camera, whether the
// picture comes from the computer's own camera or from one in the room.
//
// The camera's certificate is pinned here, on the computer that shares the
// network with it: by a fingerprint given on the command line, or trusted on
// first use and saved (cameras.json in the state directory), so a camera
// replaced or impersonated later is refused. Credentials travel only on the
// local network, and only to the address they were given for. Media is
// forwarded as it is, except that H.264 and H.265 are re-packetized to fit
// the stream's packet limit; nothing is decoded.
package camera

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/FemLed/masseuse-camlink/internal/tunnel"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/pion/rtp"
)

// StoreFile holds trusted camera fingerprints, in the state directory.
const StoreFile = "cameras.json"

// payloadMax keeps re-packetized video under what the stream's RTSPS server
// forwards (1472 bytes less the SRTP overhead).
const payloadMax = 1200

var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Options configures a Source.
type Options struct {
	// URL is the camera's stream, rtsps://[user:password@]host[:port]/path.
	// The host must be on a private network.
	URL string
	// Fingerprint pins the camera's certificate (SHA-256, hex, case and
	// colons ignored). Empty trusts the certificate seen first and saves it.
	Fingerprint string
	// StateDir holds StoreFile.
	StateDir string
	// Resolver resolves the camera's name; nil means the system resolver.
	Resolver tunnel.Resolver
	// OnTrust is told when a certificate is trusted on first use and saved.
	OnTrust func(host, fingerprint string)
	Logger  *slog.Logger
}

// Source is the configured camera, idle until Start.
type Source struct {
	sink     *serve.Server
	u        *base.URL
	host     string
	addr     string
	pinned   string
	storeDir string
	resolver tunnel.Resolver
	onTrust  func(host, fingerprint string)
	log      *slog.Logger
	// ReadTimeout bounds each request to the camera; 0 means 10 s.
	ReadTimeout time.Duration

	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	publishing bool
	fpErr      *FingerprintError // set by verify, reported by the request that failed
	lastErr    error
}

// New checks the link and the pin, without connecting; the camera is
// contacted when a session first reads.
func New(ctx context.Context, sink *serve.Server, opts Options) (*Source, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	raw := strings.TrimSpace(opts.URL)
	pu, err := url.Parse(raw)
	if err != nil || pu.Scheme != "rtsps" || pu.Hostname() == "" {
		return nil, errors.New("camera: the link must start with rtsps:// and name the camera")
	}
	port := pu.Port()
	if port == "" {
		port = "322"
		pu.Host = net.JoinHostPort(pu.Hostname(), port)
	}
	addr, err := tunnel.ResolvePrivate(ctx, pu.Hostname(), port, opts.Resolver)
	if err != nil {
		return nil, fmt.Errorf("camera: %s: %v", pu.Hostname(), err)
	}
	u, err := base.ParseURL(pu.String())
	if err != nil {
		return nil, fmt.Errorf("camera: %w", err)
	}
	s := &Source{
		sink: sink, u: u, host: pu.Host, addr: addr, storeDir: opts.StateDir,
		resolver: opts.Resolver, onTrust: opts.OnTrust, log: opts.Logger,
	}
	if fp := normalizeFingerprint(opts.Fingerprint); opts.Fingerprint != "" {
		if fp == "" {
			return nil, errors.New("camera: the fingerprint must be the certificate's SHA-256 (64 hex digits)")
		}
		s.pinned = fp
	} else {
		store, err := loadStore(filepath.Join(opts.StateDir, StoreFile))
		if err != nil {
			return nil, err
		}
		s.pinned = store[s.host]
	}
	return s, nil
}

// Host is the camera's host:port, without credentials.
func (s *Source) Host() string { return s.host }

// Label names the camera the way the phone shows it.
func (s *Source) Label() string {
	host, _, _ := net.SplitHostPort(s.host)
	return "Camera at " + host
}

// Fingerprint is the pinned certificate fingerprint, "" until one is seen.
func (s *Source) Fingerprint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinned
}

// Running reports whether Start has been called and Stop has not.
func (s *Source) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

// Publishing reports whether the camera's stream is flowing.
func (s *Source) Publishing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishing
}

// LastError is why the last attempt to read the camera ended, nil if none.
func (s *Source) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Start begins pulling the camera and keeps at it (with backoff) until
// Stop. Calling it while running does nothing.
func (s *Source) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(ctx, s.done)
}

// Stop ends the pull.
func (s *Source) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (s *Source) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	backoff := time.Second
	for {
		started := time.Now()
		err := s.pull(ctx)
		if ctx.Err() != nil {
			s.log.Info("camera: stopped", "camera", s.host)
			return
		}
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		if time.Since(started) > 10*time.Second {
			backoff = time.Second
		}
		s.log.Warn("camera: stream ended; reconnecting", "camera", s.host, "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			s.log.Info("camera: stopped", "camera", s.host)
			return
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

// Probe connects, checks the certificate and describes the stream without
// starting it, reporting what the camera offers.
func (s *Source) Probe(ctx context.Context) (*description.Session, error) {
	c := s.client()
	if err := c.Start(); err != nil {
		return nil, err
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	desc, _, err := c.Describe(s.u)
	if err != nil {
		return nil, s.describeError(err)
	}
	return desc, nil
}

// pull reads the camera into the stream until it fails or ctx ends.
func (s *Source) pull(ctx context.Context) error {
	c := s.client()
	if err := c.Start(); err != nil {
		return err
	}
	defer c.Close()
	desc, _, err := c.Describe(s.u)
	if err != nil {
		return s.describeError(err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		return err
	}
	pub, err := s.sink.Publish(desc)
	if err != nil {
		return err
	}
	defer pub.Close()
	fwd := newForwarder(pub, desc, s.log, c.PacketNTP)
	c.OnPacketRTPAny(fwd.packet)
	if _, err := c.Play(nil); err != nil {
		return err
	}
	s.setPublishing(true)
	defer s.setPublishing(false)
	s.log.Info("camera: streaming", "camera", s.host, "medias", mediaKinds(desc))
	waitErr := make(chan error, 1)
	go func() { waitErr <- c.Wait() }()
	select {
	case err := <-waitErr:
		return err
	case <-ctx.Done():
		c.Close()
		<-waitErr
		return ctx.Err()
	}
}

func (s *Source) client() *gortsplib.Client {
	timeout := s.ReadTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	tcp := gortsplib.ProtocolTCP
	return &gortsplib.Client{
		Scheme: "rtsps", Host: s.host, Protocol: &tcp,
		ReadTimeout: timeout, WriteTimeout: timeout,
		// The camera's name resolved to a private address already; dial
		// that address, not whatever the name says now.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, s.addr)
		},
		TLSConfig: s.fingerprintVerifiedTLS(),
	}
}

// fingerprintVerifiedTLS is the TLS configuration for the camera. A home
// camera presents a self-signed certificate for a private address, which no
// chain of trust can vouch for, so chain verification is off and verify
// checks the certificate itself: its SHA-256 must be the pinned one (given
// with -camera-fingerprint, or trusted on first use and saved). It is the
// check the enclave applies to every camera it is handed. The connection is
// refused, not merely logged, when the fingerprint differs.
func (s *Source) fingerprintVerifiedTLS() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // no chain check; verify pins the certificate
		MinVersion:         tls.VersionTLS12,
		VerifyConnection:   s.verify,
	}
}

// verify pins the camera's certificate, trusting the first one seen when
// nothing is pinned yet.
func (s *Source) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("camera sent no certificate")
	}
	fp := serve.Fingerprint(cs.PeerCertificates[0])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned == "" {
		s.pinned = fp
		if err := s.save(fp); err != nil {
			s.log.Warn("camera: could not save the certificate fingerprint", "err", err)
		}
		s.log.Info("camera: certificate trusted on first use", "camera", s.host, "fingerprint", fp)
		if s.onTrust != nil {
			s.onTrust(s.host, fp)
		}
		return nil
	}
	if fp != s.pinned {
		s.fpErr = &FingerprintError{Host: s.host, Pinned: s.pinned, Seen: fp}
		return s.fpErr
	}
	return nil
}

// FingerprintError is a certificate other than the pinned one.
type FingerprintError struct{ Host, Pinned, Seen string }

func (e *FingerprintError) Error() string {
	return fmt.Sprintf("camera %s presented certificate %s, not the trusted %s; if the camera was replaced, pass -camera-fingerprint %s or remove it from %s",
		e.Host, e.Seen, e.Pinned, e.Seen, StoreFile)
}

// describeError names the likely cause of a failed first request: the
// certificate check (whose error the TLS layer does not pass through) or
// the credentials.
func (s *Source) describeError(err error) error {
	s.mu.Lock()
	fe := s.fpErr
	s.fpErr = nil
	s.mu.Unlock()
	if fe != nil {
		return fe
	}
	if strings.Contains(err.Error(), "401") {
		return fmt.Errorf("camera %s refused the user name or password", s.host)
	}
	return err
}

func (s *Source) save(fp string) error {
	if s.storeDir == "" {
		return nil
	}
	path := filepath.Join(s.storeDir, StoreFile)
	store, err := loadStore(path)
	if err != nil {
		return err
	}
	store[s.host] = fp
	b, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.storeDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func loadStore(path string) (map[string]string, error) {
	store := map[string]string{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("camera: %w", err)
	}
	if err := json.Unmarshal(b, &store); err != nil {
		return nil, fmt.Errorf("camera: %s: %w", path, err)
	}
	return store, nil
}

func (s *Source) setPublishing(v bool) {
	s.mu.Lock()
	s.publishing = v
	s.mu.Unlock()
}

func normalizeFingerprint(fp string) string {
	fp = strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(fp)))
	fp = strings.TrimPrefix(fp, "sha256")
	if !fingerprintRe.MatchString(fp) {
		return ""
	}
	return fp
}

func mediaKinds(desc *description.Session) []string {
	kinds := make([]string, 0, len(desc.Medias))
	for _, m := range desc.Medias {
		kinds = append(kinds, string(m.Type))
	}
	return kinds
}

// forwarder moves the camera's packets into the stream. H.264 and H.265 go
// through an RTP depacketizer and packetizer pair so that packets fit the
// stream's limit whatever size the camera chose; everything else passes as
// it is. Each packet is timed by the camera's own RTCP sender reports
// (`timeOf`, the RTSP client's PacketNTP; serve.TimedWriter), which the
// re-packetized units keep since they keep the timestamp; the stream sets
// a clock that is plainly wrong aside (serve.MaxSourceClockSkew).
type forwarder struct {
	w    *serve.TimedWriter
	log  *slog.Logger
	repk map[format.Format]*repacketizer
	mu   sync.Mutex
	warn int
}

func newForwarder(pub *serve.Publication, desc *description.Session, log *slog.Logger,
	timeOf func(*description.Media, *rtp.Packet) (time.Time, bool)) *forwarder {
	f := &forwarder{w: serve.NewTimedWriter(pub, timeOf), log: log, repk: map[format.Format]*repacketizer{}}
	for _, m := range desc.Medias {
		for _, fo := range m.Formats {
			if r := newRepacketizer(fo); r != nil {
				f.repk[fo] = r
			}
		}
	}
	return f
}

func (f *forwarder) packet(medi *description.Media, forma format.Format, pkt *rtp.Packet) {
	r := f.repk[forma]
	if r == nil {
		f.write(medi, pkt)
		return
	}
	for _, out := range r.process(pkt) {
		f.write(medi, out)
	}
}

func (f *forwarder) write(medi *description.Media, pkt *rtp.Packet) {
	if err := f.w.Write(medi, pkt); err != nil {
		f.mu.Lock()
		f.warn++
		n := f.warn
		f.mu.Unlock()
		if n == 1 || n%1000 == 0 {
			f.log.Warn("camera: packet not forwarded", "err", err, "dropped", n)
		}
	}
}

type repacketizer struct {
	decode func(*rtp.Packet) ([][]byte, error)
	encode func([][]byte) ([]*rtp.Packet, error)
}

func newRepacketizer(fo format.Format) *repacketizer {
	switch v := fo.(type) {
	case *format.H264:
		d, err := v.CreateDecoder()
		if err != nil {
			return nil
		}
		e := &rtph264.Encoder{PayloadType: v.PayloadTyp, PacketizationMode: v.PacketizationMode, PayloadMaxSize: payloadMax}
		if e.Init() != nil {
			return nil
		}
		return &repacketizer{decode: d.Decode, encode: e.Encode}
	case *format.H265:
		d, err := v.CreateDecoder()
		if err != nil {
			return nil
		}
		e := &rtph265.Encoder{PayloadType: v.PayloadTyp, MaxDONDiff: v.MaxDONDiff, PayloadMaxSize: payloadMax}
		if e.Init() != nil {
			return nil
		}
		return &repacketizer{decode: d.Decode, encode: e.Encode}
	}
	return nil
}

// process returns the packets to forward for pkt: none until an access unit
// is complete, then that unit packetized afresh with its timestamp.
func (r *repacketizer) process(pkt *rtp.Packet) []*rtp.Packet {
	au, err := r.decode(pkt)
	if err != nil {
		return nil // more packets needed, or a fragment without its start
	}
	out, err := r.encode(au)
	if err != nil {
		return nil
	}
	for _, p := range out {
		p.Timestamp = pkt.Timestamp
	}
	return out
}

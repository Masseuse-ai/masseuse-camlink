// Package tunnel is the connector side of the tunnel (docs/PROTOCOL.md,
// section 4): verify the enclave's attestation, dial its WebSocket with the
// brokered ticket over TLS pinned to the attested key, prove the connector
// identity, then accept OPEN streams and relay each one to the single
// private-network target the session names.
package tunnel

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/FemLed/masseuse-camlink/internal/attest"
	"github.com/FemLed/masseuse-camlink/internal/frame"
	"github.com/FemLed/masseuse-camlink/internal/identity"
	"github.com/FemLed/masseuse-camlink/internal/mux"
)

// Subprotocol is the WebSocket subprotocol both ends require.
const Subprotocol = "camlink.v1"

// Attester verifies an origin's attestation and returns the TLS key to pin.
type Attester interface {
	Verify(ctx context.Context, origin string) (*attest.Result, error)
}

// Resolver looks up hostnames; net.DefaultResolver in production.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Dialer opens tunnels.
type Dialer struct {
	Identity *identity.Identity
	Attester Attester
	// RootCAs verifies the enclave's certificate chain; nil means the system
	// roots. The attested SPKI is pinned in addition.
	RootCAs *x509.CertPool
	// Resolver resolves camera hostnames; nil means net.DefaultResolver.
	Resolver Resolver
	// DialTimeout bounds each camera dial; 0 means 10 s.
	DialTimeout time.Duration
	// HandshakeTimeout bounds the WebSocket and CHALLENGE/PROVE exchange; 0
	// means 20 s.
	HandshakeTimeout time.Duration
	// RelayBuffer is the copy buffer per direction; 0 means 256 KiB.
	RelayBuffer int
	Logger      *slog.Logger
}

// Tunnel is one attached tunnel.
type Tunnel struct {
	Origin string
	Result *attest.Result

	d      *Dialer
	sess   *mux.Session
	policy targetPolicy
	log    *slog.Logger
}

func (d *Dialer) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// Dial verifies origin, connects, proves the connector identity and returns
// the tunnel ready to Serve. ticketHash is the hex SHA-256 of the ticket as
// the service reported it.
func (d *Dialer) Dial(ctx context.Context, origin, ticket, ticketHash string) (*Tunnel, error) {
	if d.Identity == nil || d.Attester == nil {
		return nil, errors.New("tunnel: dialer needs an identity and an attester")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("tunnel: origin %q is not an https origin", origin)
	}
	hostname := u.Hostname()

	res, err := d.Attester.Verify(ctx, origin)
	if err != nil {
		return nil, fmt.Errorf("tunnel: %s: %w", hostname, err)
	}

	timeout := d.HandshakeTimeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := &http.Transport{
		TLSClientConfig:   attest.PinnedTLSConfig(d.RootCAs, hostname, res.SPKISHA256),
		DisableKeepAlives: true,
		Proxy:             nil, // never through a proxy: the pin is to this host
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+ticket)
	header.Set("User-Agent", "masseuse-camlink")
	c, resp, err := websocket.Dial(hctx, "wss://"+u.Host+"/ingest/tunnel", &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: transport},
		HTTPHeader:      header,
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("tunnel: %s answered %d", hostname, resp.StatusCode)
		}
		return nil, fmt.Errorf("tunnel: dial %s: %w", hostname, err)
	}
	if c.Subprotocol() != Subprotocol {
		c.Close(websocket.StatusProtocolError, "subprotocol")
		return nil, errors.New("tunnel: gateway did not select " + Subprotocol)
	}
	c.SetReadLimit(frame.HeaderSize + frame.MaxPayload)

	// The session context outlives Dial; the mux closes the conn when done.
	sctx, scancel := context.WithCancel(context.Background())
	conn := websocket.NetConn(sctx, c, websocket.MessageBinary)
	if err := d.prove(conn, hostname, ticketHash, timeout); err != nil {
		_ = conn.Close()
		scancel()
		return nil, fmt.Errorf("tunnel: %s: %w", hostname, err)
	}
	sess := mux.New(conn, false)
	go func() {
		<-sess.Done()
		scancel()
	}()
	return &Tunnel{Origin: origin, Result: res, d: d, sess: sess, log: d.logger().With("origin", hostname)}, nil
}

func (d *Dialer) prove(conn net.Conn, host, ticketHash string, timeout time.Duration) error {
	w := frame.NewWriter(conn)
	r := frame.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})
	f, err := r.Read()
	if err != nil {
		return fmt.Errorf("waiting for CHALLENGE: %w", err)
	}
	if f.Type == frame.TypeReset {
		return fmt.Errorf("gateway refused: %s", f.Payload)
	}
	if f.Type != frame.TypeChallenge || f.Stream != 0 || len(f.Payload) != frame.ChallengeSize {
		return errors.New("expected a 32-byte CHALLENGE on stream 0")
	}
	sig := d.Identity.Sign(identity.TunnelProof(host, strings.ToLower(ticketHash), f.Payload))
	if err := w.Write(frame.Frame{Type: frame.TypeProve, Stream: 0, Payload: sig}); err != nil {
		return err
	}
	f, err = r.Read()
	if err != nil {
		return fmt.Errorf("waiting for READY: %w", err)
	}
	if f.Type == frame.TypeReset {
		return fmt.Errorf("gateway refused: %s", f.Payload)
	}
	if f.Type != frame.TypeReady || f.Stream != 0 {
		return errors.New("expected READY on stream 0")
	}
	return nil
}

// Done is closed when the tunnel has ended.
func (t *Tunnel) Done() <-chan struct{} { return t.sess.Done() }

// Err reports why the tunnel ended.
func (t *Tunnel) Err() error { return t.sess.Err() }

// Close ends the tunnel.
func (t *Tunnel) Close() { t.sess.Close() }

// Serve answers OPEN requests until the tunnel ends or ctx is cancelled.
func (t *Tunnel) Serve(ctx context.Context) error {
	for {
		p, err := t.sess.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.sess.Close()
				return ctx.Err()
			}
			return err
		}
		go t.handle(ctx, p)
	}
}

func (t *Tunnel) handle(ctx context.Context, p *mux.Pending) {
	addr, err := t.policy.allow(ctx, p.Target, t.d.Resolver)
	if err != nil {
		t.log.Warn("refused stream", "target", p.Target, "reason", err.Error())
		_ = p.Refuse(err.Error())
		return
	}
	timeout := t.d.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
	cancel()
	if err != nil {
		// The local log may name the camera; the reason sent to the enclave
		// does not.
		t.log.Warn("camera unreachable", "target", p.Target, "err", err)
		_ = p.Refuse("camera unreachable")
		return
	}
	st, err := p.Accept()
	if err != nil {
		_ = conn.Close()
		return
	}
	t.log.Info("relaying", "target", p.Target, "stream", st.ID())
	buf := t.d.RelayBuffer
	if buf == 0 {
		buf = 256 << 10
	}
	mux.Relay(st, conn, buf)
	t.log.Info("stream closed", "stream", st.ID())
}

// targetPolicy admits exactly one host:port per tunnel, and only when it
// is on a private network: RFC 1918, loopback or link-local literals, or a
// name every address of which is such an address. The connector dials the
// address it checked, not the name, so a later DNS answer cannot redirect
// it.
type targetPolicy struct {
	mu     sync.Mutex
	locked string
}

func (p *targetPolicy) allow(ctx context.Context, target string, resolver Resolver) (string, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return "", errors.New("target must be host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("target port out of range")
	}
	p.mu.Lock()
	if p.locked != "" && p.locked != target {
		p.mu.Unlock()
		return "", errors.New("not the session target")
	}
	p.mu.Unlock()

	var ip net.IP
	if lit := net.ParseIP(host); lit != nil {
		if !isPrivate(lit) {
			return "", errors.New("target is not a private-network address")
		}
		ip = lit
	} else {
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, err := resolver.LookupIPAddr(rctx, host)
		cancel()
		if err != nil || len(addrs) == 0 {
			return "", errors.New("target name does not resolve")
		}
		for _, a := range addrs {
			if !isPrivate(a.IP) {
				return "", errors.New("target name resolves outside the private network")
			}
		}
		ip = addrs[0].IP
	}
	p.mu.Lock()
	p.locked = target
	p.mu.Unlock()
	return net.JoinHostPort(ip.String(), portStr), nil
}

func isPrivate(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

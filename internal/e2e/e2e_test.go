// Package e2e drives a gateway and a connector in one process: a fake TLS
// camera on loopback, the gateway's relay listener standing in for the
// camera as MediaMTX sees it, and a fake MediaMTX client that dials the
// relay and checks that the certificate it sees is the camera's own.
package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/attest"
	"github.com/FemLed/masseuse-camlink/internal/gateway"
	"github.com/FemLed/masseuse-camlink/internal/identity"
	"github.com/FemLed/masseuse-camlink/internal/tunnel"
)

// fakeAttester stands in for Confidential Space: it "attests" the test
// server by returning its certificate's SPKI hash as the pin.
type fakeAttester struct {
	spki  [32]byte
	calls int
}

func (f *fakeAttester) Verify(_ context.Context, origin string) (*attest.Result, error) {
	f.calls++
	return &attest.Result{Origin: origin, SPKISHA256: f.spki, ImageDigest: "sha256:test"}, nil
}

// camera is a TLS echo server with a self-signed certificate, like a home
// camera speaking RTSPS.
type camera struct {
	ln          net.Listener
	fingerprint string
	accepted    atomic.Int32
}

func startCamera(t *testing.T) *camera {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "UVC G4 Pro"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	c := &camera{ln: ln, fingerprint: hex.EncodeToString(sum[:])}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.accepted.Add(1)
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}

type rig struct {
	gw        *gateway.Server
	wsSrv     *httptest.Server
	relay     net.Listener
	control   http.Handler
	connector *identity.Identity
	attester  *fakeAttester
	dialer    *tunnel.Dialer
}

func setup(t *testing.T) *rig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(gateway.Config{Logger: logger, PingInterval: 200 * time.Millisecond})
	wsSrv := httptest.NewTLSServer(gw.WSHandler())
	t.Cleanup(wsSrv.Close)
	relay, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = gw.ServeRelay(ctx, relay) }()

	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spki := sha256.Sum256(wsSrv.Certificate().RawSubjectPublicKeyInfo)
	att := &fakeAttester{spki: spki}
	roots := x509.NewCertPool()
	roots.AddCert(wsSrv.Certificate())
	return &rig{
		gw: gw, wsSrv: wsSrv, relay: relay, control: gw.ControlHandler(), connector: id, attester: att,
		dialer: &tunnel.Dialer{Identity: id, Attester: att, RootCAs: roots, Logger: logger},
	}
}

func (r *rig) ctl(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	r.control.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func mintTicket(t *testing.T) (ticket, hash string) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), hex.EncodeToString(h[:])
}

func (r *rig) expect(t *testing.T, ticketHash string) {
	t.Helper()
	code, _ := r.ctl(t, "POST", "/expect", map[string]any{
		"connectorKey": r.connector.PublicKeyString(),
		"ticketHash":   ticketHash,
		"expiresAt":    time.Now().Add(2 * time.Minute).Unix(),
	})
	if code != 200 {
		t.Fatalf("expect: %d", code)
	}
}

func waitStatus(t *testing.T, r *rig, want func(gateway.Status) bool) gateway.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := r.gw.Status()
		if want(st) {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status never matched: %+v", r.gw.Status())
	return gateway.Status{}
}

// mediamtx dials the relay the way the enclave's RTSP server would and
// returns the leaf certificate it saw plus an echo round trip.
func mediamtx(t *testing.T, relayAddr string, payload []byte) (*x509.Certificate, []byte, error) {
	t.Helper()
	var leaf *x509.Certificate
	conn, err := tls.Dial("tcp", relayAddr, &tls.Config{
		InsecureSkipVerify: true, // MediaMTX pins the fingerprint instead of a chain
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			leaf = cs.PeerCertificates[0]
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		return leaf, nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	return leaf, got, err
}

func TestCameraThroughGatewayAndConnector(t *testing.T) {
	r := setup(t)
	cam := startCamera(t)

	// 1. The service brokers a ticket and tells the enclave to expect us.
	ticket, ticketHash := mintTicket(t)
	r.expect(t, ticketHash)
	if code, _ := r.ctl(t, "POST", "/target", map[string]any{"host": "127.0.0.1", "port": 1}); code != 409 {
		t.Fatalf("target before attach: %d, want 409", code)
	}

	// 2. The connector verifies the enclave and dials.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun, err := r.dialer.Dial(ctx, r.wsSrv.URL, ticket, ticketHash)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tun.Close()
	if r.attester.calls != 1 {
		t.Fatalf("attestation verified %d times", r.attester.calls)
	}
	go func() { _ = tun.Serve(ctx) }()
	waitStatus(t, r, func(s gateway.Status) bool { return s.Connected })
	st := r.gw.Status()
	if st.ConnectorKeyPrefix == nil || *st.ConnectorKeyPrefix != r.connector.PublicKeyString()[:8] || st.SinceMs == nil || st.Target {
		t.Fatalf("status %+v", st)
	}

	// 3. The producer sets the camera as the target.
	host, portStr, _ := net.SplitHostPort(cam.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	if code, _ := r.ctl(t, "POST", "/target", map[string]any{"host": host, "port": port}); code != 200 {
		t.Fatalf("target: %d", code)
	}

	// 4. MediaMTX dials the relay: the certificate it sees is the camera's,
	// byte for byte, and bytes round-trip.
	payload := make([]byte, 3<<20)
	_, _ = rand.Read(payload)
	leaf, got, err := mediamtx(t, r.relay.Addr().String(), payload)
	if err != nil {
		t.Fatalf("mediamtx: %v", err)
	}
	sum := sha256.Sum256(leaf.Raw)
	if hex.EncodeToString(sum[:]) != cam.fingerprint {
		t.Fatalf("fingerprint through the tunnel %x, camera's %s", sum, cam.fingerprint)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("echo mismatch through the tunnel")
	}
	if n := cam.accepted.Load(); n != 1 {
		t.Fatalf("camera accepted %d connections", n)
	}

	// 5. A second connection to the same target works; a different target
	// is refused by the connector's single-target policy.
	if _, got, err := mediamtx(t, r.relay.Addr().String(), []byte("again")); err != nil || string(got) != "again" {
		t.Fatalf("second stream: %q %v", got, err)
	}
	if code, _ := r.ctl(t, "POST", "/target", map[string]any{"host": "127.0.0.1", "port": port + 1}); code != 200 {
		t.Fatalf("retarget: %d", code)
	}
	if _, _, err := mediamtx(t, r.relay.Addr().String(), []byte("x")); err == nil {
		t.Fatal("connector relayed to a second target")
	}
	if n := cam.accepted.Load(); n != 2 {
		t.Fatalf("camera accepted %d connections", n)
	}

	// 6. Clear tears the tunnel down; the relay refuses local connections.
	if code, _ := r.ctl(t, "POST", "/clear", nil); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel survived clear")
	}
	if _, _, err := mediamtx(t, r.relay.Addr().String(), []byte("x")); err == nil {
		t.Fatal("relay accepted without a connector")
	}
}

func TestPublicTargetIsRefused(t *testing.T) {
	r := setup(t)
	ticket, ticketHash := mintTicket(t)
	r.expect(t, ticketHash)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun, err := r.dialer.Dial(ctx, r.wsSrv.URL, ticket, ticketHash)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	go func() { _ = tun.Serve(ctx) }()
	waitStatus(t, r, func(s gateway.Status) bool { return s.Connected })
	if code, _ := r.ctl(t, "POST", "/target", map[string]any{"host": "93.184.216.34", "port": 443}); code != 200 {
		t.Fatalf("target: %d", code)
	}
	conn, err := net.Dial("tcp", r.relay.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("relay stayed open to a public target")
	}
}

func TestWrongTicketAndWrongKey(t *testing.T) {
	r := setup(t)
	ticket, ticketHash := mintTicket(t)
	ctx := context.Background()

	// No expectation yet: 401.
	if _, err := r.dialer.Dial(ctx, r.wsSrv.URL, ticket, ticketHash); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("dial without expectation: %v", err)
	}
	r.expect(t, ticketHash)

	// Right ticket, wrong identity: the challenge fails.
	other, _ := identity.Load(t.TempDir())
	imposter := &tunnel.Dialer{Identity: other, Attester: r.attester, RootCAs: r.dialer.RootCAs, Logger: r.dialer.Logger}
	if _, err := imposter.Dial(ctx, r.wsSrv.URL, ticket, ticketHash); err == nil || !strings.Contains(err.Error(), "bad proof") {
		t.Fatalf("imposter: %v", err)
	}
	if r.gw.Status().Connected {
		t.Fatal("imposter attached")
	}

	// Wrong ticket: 401.
	badTicket, _ := mintTicket(t)
	if _, err := r.dialer.Dial(ctx, r.wsSrv.URL, badTicket, ticketHash); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong ticket: %v", err)
	}

	// Wrong TLS key: the pinned dial fails before any WebSocket.
	var wrong [32]byte
	pinned := &tunnel.Dialer{Identity: r.connector, Attester: &fakeAttester{spki: wrong}, RootCAs: r.dialer.RootCAs, Logger: r.dialer.Logger}
	if _, err := pinned.Dial(ctx, r.wsSrv.URL, ticket, ticketHash); err == nil || !strings.Contains(err.Error(), "attested key") {
		t.Fatalf("wrong pin: %v", err)
	}

	// The real connector still gets in.
	tun, err := r.dialer.Dial(ctx, r.wsSrv.URL, ticket, ticketHash)
	if err != nil {
		t.Fatal(err)
	}
	tun.Close()
}

func TestReconnectReplacesAndTicketOutlivesExpiryOnceAttached(t *testing.T) {
	r := setup(t)
	ticket, ticketHash := mintTicket(t)
	code, _ := r.ctl(t, "POST", "/expect", map[string]any{
		"connectorKey": r.connector.PublicKeyString(),
		"ticketHash":   ticketHash,
		"expiresAt":    time.Now().Add(time.Second).Unix(),
	})
	if code != 200 {
		t.Fatal(code)
	}
	ctx := context.Background()
	first, err := r.dialer.Dial(ctx, r.wsSrv.URL, ticket, ticketHash)
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, r, func(s gateway.Status) bool { return s.Connected })
	time.Sleep(1100 * time.Millisecond) // past expiresAt
	second, err := r.dialer.Dial(ctx, r.wsSrv.URL, ticket, ticketHash)
	if err != nil {
		t.Fatalf("reconnect after expiry: %v", err)
	}
	defer second.Close()
	select {
	case <-first.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first tunnel not replaced")
	}
	if !r.gw.Status().Connected {
		t.Fatal("second tunnel not attached")
	}
}

func TestControlValidation(t *testing.T) {
	r := setup(t)
	key := r.connector.PublicKeyString()
	future := time.Now().Add(time.Minute).Unix()
	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad key", map[string]any{"connectorKey": "nope", "ticketHash": strings.Repeat("ab", 32), "expiresAt": future}},
		{"short hash", map[string]any{"connectorKey": key, "ticketHash": "abcd", "expiresAt": future}},
		{"past", map[string]any{"connectorKey": key, "ticketHash": strings.Repeat("ab", 32), "expiresAt": 1}},
		{"unknown field", map[string]any{"connectorKey": key, "ticketHash": strings.Repeat("ab", 32), "expiresAt": future, "target": "x"}},
	}
	for _, tc := range cases {
		if code, _ := r.ctl(t, "POST", "/expect", tc.body); code != 400 {
			t.Errorf("%s: %d, want 400", tc.name, code)
		}
	}
	if code, _ := r.ctl(t, "POST", "/target", map[string]any{"host": "", "port": 1}); code != 400 {
		t.Errorf("empty host: %d", code)
	}
	code, st := r.ctl(t, "GET", "/status", nil)
	if code != 200 || st["expecting"] != false || st["connected"] != false || st["target"] != false {
		t.Errorf("status %d %v", code, st)
	}
	for _, k := range []string{"host", "port"} {
		if _, ok := st[k]; ok {
			t.Errorf("status leaks %s", k)
		}
	}
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	r.control.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("healthz %d %q", rec.Code, rec.Body.String())
	}
}

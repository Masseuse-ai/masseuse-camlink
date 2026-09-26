package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/attest"
	"github.com/FemLed/masseuse-camlink/internal/gateway"
	"github.com/FemLed/masseuse-camlink/internal/identity"
	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/FemLed/masseuse-camlink/internal/tunnel"
)

// pinAttester stands in for Confidential Space: the gateway's own key is
// the attested one.
type pinAttester struct{ spki [32]byte }

func (a *pinAttester) Verify(_ context.Context, origin string) (*attest.Result, error) {
	return &attest.Result{Origin: origin, SPKISHA256: a.spki, ImageDigest: "sha256:test"}, nil
}

// console captures what the manager prints for the person.
type console struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *console) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *console) count(line string) int { return strings.Count(c.String(), line) }

// harness is a gateway behind a counting handler, and a manager whose
// dialer trusts it.
type harness struct {
	gw     *gateway.Server
	srv    *httptest.Server
	dials  atomic.Int32
	mgr    *manager
	id     *identity.Identity
	origin string
	out    *console
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(gateway.Config{Logger: logger, PingInterval: 10 * time.Second})
	h := &harness{gw: gw, out: &console{}}
	ws := gw.WSHandler()
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.dials.Add(1)
		ws.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	h.origin = h.srv.URL

	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.id = id
	spki := sha256.Sum256(h.srv.Certificate().RawSubjectPublicKeyInfo)
	roots := x509.NewCertPool()
	roots.AddCert(h.srv.Certificate())
	sink, err := serve.New(serve.Config{StateDir: t.TempDir(), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	h.mgr = &manager{
		id: id, log: logger, out: h.out,
		cam: &camControl{sink: sink, log: logger, out: h.out},
		dialer: &tunnel.Dialer{
			Identity: id, Attester: &pinAttester{spki: spki}, RootCAs: roots, Logger: logger,
		},
		tunnels: map[string]*active{},
	}
	t.Cleanup(func() { h.mgr.closeAll("test over") })
	return h
}

func (h *harness) ticket(t *testing.T) (ticket, hash string) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), hex.EncodeToString(sum[:])
}

func (h *harness) control(t *testing.T, path string, body any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest("POST", path, &buf)
	rec := httptest.NewRecorder()
	h.gw.ControlHandler().ServeHTTP(rec, req)
	return rec.Code
}

func (h *harness) expect(t *testing.T, hash string) {
	t.Helper()
	code := h.control(t, "/expect", map[string]any{
		"connectorKey": h.id.PublicKeyString(), "ticketHash": hash, "expiresAt": time.Now().Add(time.Minute).Unix(),
	})
	if code != 200 {
		t.Fatalf("expect: %d", code)
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRefusedTicketIsDialedOnceAndClearStillReports(t *testing.T) {
	h := newHarness(t)
	out := h.out

	// The enclave is not expecting us (the phone removed the camera and the
	// gateway forgot the ticket): one dial, one line, no retry.
	ticket, hash := h.ticket(t)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s1", Origin: h.origin, Ticket: ticket, TicketHash: hash})
	waitUntil(t, "the hold line", func() bool { return out.count("Camera link on hold") == 1 })
	time.Sleep(2500 * time.Millisecond) // past the first two backoffs of the old loop
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dialed %d times", n)
	}
	if out.count("Camera link on hold") != 1 {
		t.Fatalf("hold line printed %d times", out.count("Camera link on hold"))
	}
	// The same dial re-sent by the service (at the head of a fresh event
	// stream) is dialed again: the entry is finished, so it is not
	// idempotent any more, and the outcome is the same.
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s1", Origin: h.origin, Ticket: ticket, TicketHash: hash})
	waitUntil(t, "the second hold line", func() bool { return out.count("Camera link on hold") == 2 })
	if n := h.dials.Load(); n != 2 {
		t.Fatalf("dialed %d times", n)
	}
	// The session entry stayed, so the clear is still reported.
	h.mgr.OnClear("s1", "ended")
	if out.count("Camera link closed.") != 1 {
		t.Fatalf("console:\n%s", out.String())
	}
	h.mgr.OnClear("s1", "ended") // gone now: nothing more
	if out.count("Camera link closed.") != 1 {
		t.Fatalf("console:\n%s", out.String())
	}
}

func TestEnclaveClosingTheLinkIsNotRedialed(t *testing.T) {
	h := newHarness(t)
	out := h.out
	ticket, hash := h.ticket(t)
	h.expect(t, hash)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s2", Origin: h.origin, Ticket: ticket, TicketHash: hash})
	waitUntil(t, "attach", func() bool { return h.gw.Status().Connected })
	if out.count("Camera link active") != 1 {
		t.Fatalf("console:\n%s", out.String())
	}
	// The enclave clears the session (the phone removed the camera).
	if code := h.control(t, "/clear", nil); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	waitUntil(t, "the closed line", func() bool { return out.count("The enclave closed the camera link (session cleared)") == 1 })
	time.Sleep(2500 * time.Millisecond)
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dialed %d times after the enclave closed the link", n)
	}
	// A new ticket for the same session is dialed at once.
	ticket2, hash2 := h.ticket(t)
	h.expect(t, hash2)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s2", Origin: h.origin, Ticket: ticket2, TicketHash: hash2})
	waitUntil(t, "second attach", func() bool { return h.gw.Status().Connected && h.dials.Load() == 2 })
	h.mgr.OnClear("s2", "ended")
	waitUntil(t, "detach", func() bool { return !h.gw.Status().Connected })
	if out.count("Camera link closed.") != 1 {
		t.Fatalf("console:\n%s", out.String())
	}
}

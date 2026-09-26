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
	"net"
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

// wire stands between the connector and the gateway so the test can cut the
// connection the way a network does: no close frame, no reason, just gone.
// TLS passes through untouched (the certificate names 127.0.0.1 either way,
// and the pin is on the gateway's key).
type wire struct {
	ln     net.Listener
	mu     sync.Mutex
	conns  []net.Conn
	origin string
}

func newWire(t *testing.T, to string) *wire {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := &wire{ln: ln, origin: "https://" + ln.Addr().String()}
	t.Cleanup(func() { _ = ln.Close(); w.cut() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", to)
			if err != nil {
				_ = c.Close()
				continue
			}
			w.mu.Lock()
			w.conns = append(w.conns, c, up)
			w.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
	return w
}

// cut drops every connection on the wire.
func (w *wire) cut() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.conns {
		_ = c.Close()
	}
	w.conns = nil
}

func TestANewTicketForAConnectedSessionKeepsTheTunnel(t *testing.T) {
	h := newHarness(t)
	out := h.out
	w := newWire(t, h.srv.Listener.Addr().String())

	ticket, hash := h.ticket(t)
	h.expect(t, hash)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s3", Origin: w.origin, Ticket: ticket, TicketHash: hash})
	waitUntil(t, "attach", func() bool { return h.gw.Status().Connected })
	since := *h.gw.Status().SinceMs

	// The service leases the enclave again (a production restart) and
	// brokers again: the gateway is told the new ticket, the connector is
	// dialed with it. The tunnel that is up stays up.
	ticket2, hash2 := h.ticket(t)
	h.expect(t, hash2)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s3", Origin: w.origin, Ticket: ticket2, TicketHash: hash2})
	time.Sleep(500 * time.Millisecond)
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dialed %d times: the new ticket replaced a working tunnel", n)
	}
	if st := h.gw.Status(); !st.Connected || *st.SinceMs != since {
		t.Fatalf("the tunnel changed: %+v (attached since %d)", st, since)
	}
	if out.count("Camera link active") != 1 {
		t.Fatalf("console:\n%s", out.String())
	}

	// The network drops the tunnel. The connector re-dials on its own, and
	// with the ticket the enclave now expects - the newest - so it is let
	// back in rather than refused.
	w.cut()
	waitUntil(t, "detach", func() bool { return !h.gw.Status().Connected })
	waitUntil(t, "re-attach", func() bool { return h.gw.Status().Connected && h.dials.Load() == 2 })
	waitUntil(t, "the second active line", func() bool { return out.count("Camera link active") == 2 })
	if out.count("Camera link on hold") != 0 {
		t.Fatalf("the re-dial was refused:\n%s", out.String())
	}
	h.mgr.OnClear("s3", "ended")
	waitUntil(t, "closed", func() bool { return !h.gw.Status().Connected })
}

func TestADialForAnotherEnclaveReplacesTheTunnel(t *testing.T) {
	h := newHarness(t)
	w := newWire(t, h.srv.Listener.Addr().String())
	ticket, hash := h.ticket(t)
	h.expect(t, hash)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s4", Origin: h.origin, Ticket: ticket, TicketHash: hash})
	waitUntil(t, "attach", func() bool { return h.gw.Status().Connected })

	// The session moved to another enclave (here: the same gateway by
	// another address): the tunnel to the old one goes, the new one is
	// dialed with its ticket.
	ticket2, hash2 := h.ticket(t)
	h.expect(t, hash2)
	h.mgr.OnDial(rendezvous.Dial{SessionID: "s4", Origin: w.origin, Ticket: ticket2, TicketHash: hash2})
	waitUntil(t, "the second dial", func() bool { return h.dials.Load() == 2 })
	waitUntil(t, "attached over the wire", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return h.gw.Status().Connected && len(w.conns) > 0
	})
	h.mgr.OnClear("s4", "ended")
	waitUntil(t, "closed", func() bool { return !h.gw.Status().Connected })
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
	// The gateway is attached a moment before the connector says so.
	waitUntil(t, "the active line", func() bool { return out.count("Camera link active") == 1 })
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

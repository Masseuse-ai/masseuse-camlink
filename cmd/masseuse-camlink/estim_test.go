package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312/fakebox"
	"github.com/FemLed/masseuse-camlink/internal/identity"
	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
)

// estimService records signed device link posts the way the service does.
type estimService struct {
	mu    sync.Mutex
	posts []struct {
		session  string
		messages []map[string]any
	}
	status int
}

func (s *estimService) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/camlink/estim", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key       string `json:"key"`
			Ts        int64  `json:"ts"`
			SessionID string `json:"sessionId"`
			Messages  string `json:"messages"`
			Sig       string `json:"sig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		pub, err := identity.ParsePublicKey(body.Key)
		sig, _ := base64.RawURLEncoding.DecodeString(body.Sig)
		if err != nil || !ed25519.Verify(pub, identity.EstimMessage(body.Ts, body.Key, body.SessionID, body.Messages), sig) {
			http.Error(w, "bad signature", 401)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.status != 0 {
			http.Error(w, "no", s.status)
			return
		}
		var msgs []map[string]any
		if err := json.Unmarshal([]byte(body.Messages), &msgs); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.posts = append(s.posts, struct {
			session  string
			messages []map[string]any
		}{body.SessionID, msgs})
		w.WriteHeader(204)
	})
	return m
}

func (s *estimService) find(typ string) (string, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.posts {
		for _, m := range p.messages {
			if m["type"] == typ {
				return p.session, m
			}
		}
	}
	return "", nil
}

func (s *estimService) waitFor(t *testing.T, typ string) (string, map[string]any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sid, m := s.find(typ); m != nil {
			return sid, m
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("service never received %q", typ)
	return "", nil
}

func TestEstimLinkEndToEnd(t *testing.T) {
	svc := &estimService{}
	srv := httptest.NewServer(svc.handler())
	defer srv.Close()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box := fakebox.New()
	var out console
	link := newEstimLink(t.TempDir(), "", log, func(format string, args ...any) { _, _ = out.Write([]byte(strings.TrimSpace(format))) })
	link.rt.Connect = func(ctx context.Context) (estim.Driver, error) {
		box.Reopen()
		dev := mk312.New(box, "fake")
		dev.Sleep = func(context.Context, time.Duration) error { return nil }
		dev.Timeout = 30 * time.Millisecond
		dev.Ramp = 0
		return mk312.Connect(ctx, dev, nil, nil)
	}
	link.client = &rendezvous.Client{Service: srv.URL, Identity: id, Version: "test", HTTP: srv.Client(), Logger: log}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); link.run(ctx) }()

	// The device is found, released, and reported at connector level.
	sid, dev := svc.waitFor(t, "device")
	if sid != "" || dev["kind"] != "mk312bt" || dev["connected"] != true {
		t.Fatalf("device report %q %v", sid, dev)
	}
	if !strings.Contains(out.String(), "Stimulation device connected") {
		t.Fatalf("console: %q", out.String())
	}
	mgr := &manager{log: log, estim: link, tunnels: map[string]*active{}}

	// The service attaches a session: the device arms and reports so.
	mgr.OnEstim("s1", json.RawMessage(`{"type":"control","payload":{"type":"companion_attached","sessionId":"s1","levelCap":85}}`))
	deadline := time.Now().Add(5 * time.Second)
	for !link.rt.Armed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !link.rt.Armed() || box.Power() != mk312.PowerHigh {
		t.Fatal("attach did not arm")
	}
	mgr.OnEstim("s1", json.RawMessage(`{"type":"control","payload":{"type":"mk312_command","commandId":"c1","sessionId":"s1","command":{"verb":"set_level","channel":"a","level":6}}}`))
	sid, ack := svc.waitFor(t, "device_ack")
	if sid != "s1" || ack["ok"] != true || box.LevelA() != 6 {
		t.Fatalf("ack %q %v (level %d)", sid, ack, box.LevelA())
	}
	// A hello after a reconnect repeats the device report.
	before := len(svc.posts)
	mgr.OnOnline(true)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		n := len(svc.posts)
		svc.mu.Unlock()
		if n > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The service clearing the session releases the device.
	mgr.OnClear("s1", "session ended")
	if _, attached := link.session.Attached(); attached || link.rt.Armed() || box.LevelA() != 0 {
		t.Fatal("clear did not release")
	}
	if _, m := svc.waitFor(t, "detached"); m["reason"] != "session cleared" {
		t.Fatalf("detached %v", m)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("link did not stop")
	}
	if box.Key() != mk312.NoKey || !box.Closed() {
		t.Fatal("exit must unkey and close the device")
	}
}

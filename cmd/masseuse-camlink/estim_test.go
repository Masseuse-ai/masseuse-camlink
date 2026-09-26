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

	"github.com/FemLed/masseuse-camlink/internal/ble"
	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago/fakeunit"
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

// findCommand is the device_ack for one command, when it has arrived.
func (s *estimService) findCommand(commandID string) (string, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.posts {
		for _, m := range p.messages {
			if m["type"] == "device_ack" && m["commandId"] == commandID {
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

// TestEstimLinkBluetoothUnit runs the link against a fake Mastago unit
// through the registered finder, the way the program does: found,
// released, reported as kind mastago, armed with its countdown set,
// driven by a device_command, released when the session is cleared.
func TestEstimLinkBluetoothUnit(t *testing.T) {
	svc := &estimService{}
	srv := httptest.NewServer(svc.handler())
	defer srv.Close()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	unit := fakeunit.New("unit-1", "MASTOGO G-12AB")
	unit.PushLevel(9)
	central := &fakeunit.Central{Units: []*fakeunit.Unit{unit}}
	var out console
	link := newEstimLink(t.TempDir(), log, func(format string, args ...any) { _, _ = out.Write([]byte(strings.TrimSpace(format))) })
	finder := mastago.NewFinder("", log)
	finder.Open = func(context.Context, *slog.Logger) (ble.Central, error) { return central, nil }
	finder.ScanWindow = 50 * time.Millisecond
	link.finders = estim.Finders{finder}
	link.rt.Connect = func(ctx context.Context) (estim.Driver, error) {
		d, err := link.finders.Find(ctx)
		if err != nil {
			return nil, err
		}
		drv := d.(*mastago.Driver)
		drv.Gap, drv.Step, drv.Timeout = 0, 0, 500*time.Millisecond
		drv.Sleep = func(context.Context, time.Duration) error { return nil }
		return drv, nil
	}
	link.client = &rendezvous.Client{Service: srv.URL, Identity: id, Version: "test", HTTP: srv.Client(), Logger: log}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); link.run(ctx) }()

	sid, dev := svc.waitFor(t, "device")
	if sid != "" || dev["kind"] != "mastago" || dev["connected"] != true || dev["label"] != "Mastago TENS G-12AB" {
		t.Fatalf("device report %q %v", sid, dev)
	}
	caps, _ := dev["capabilities"].(map[string]any)
	if caps["levelMax"] != float64(25) || caps["timer"] != true || caps["loadDetect"] != true || caps["levelMaxDefault"] != float64(15) {
		t.Fatalf("capabilities %v", caps)
	}
	if unit.Level() != 0 || unit.Outputting() {
		t.Fatal("a fresh connection must be released")
	}
	if !strings.Contains(out.String(), "Stimulation device connected") || link.rt.Descriptor().Label != "Mastago TENS G-12AB" {
		t.Fatalf("console: %q, descriptor %+v", out.String(), link.rt.Descriptor())
	}
	mgr := &manager{log: log, estim: link, tunnels: map[string]*active{}}

	mgr.OnEstim("s1", json.RawMessage(`{"type":"control","payload":{"type":"companion_attached","sessionId":"s1","levelCap":15}}`))
	deadline := time.Now().Add(5 * time.Second)
	for !link.rt.Armed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !link.rt.Armed() || unit.TimerS() != int(estim.MaxArmWindow.Seconds()) {
		t.Fatalf("attach did not arm the unit's countdown: armed=%v timer=%d", link.rt.Armed(), unit.TimerS())
	}
	mgr.OnEstim("s1", json.RawMessage(`{"type":"control","payload":{"type":"device_command","commandId":"c1","sessionId":"s1","command":{"verb":"set_level","channel":"a","level":6}}}`))
	sid, ack := svc.waitFor(t, "device_ack")
	if sid != "s1" || ack["ok"] != true || unit.Level() != 6 || !unit.Outputting() {
		t.Fatalf("ack %q %v (level %d outputting %v)", sid, ack, unit.Level(), unit.Outputting())
	}
	st, _ := ack["status"].(map[string]any)
	if st["outputting"] != true || st["loadDetected"] != true || st["timerRemainingS"] == nil {
		t.Fatalf("status in the ack: %v", st)
	}
	// A level past the session's cap is refused before it reaches the unit.
	mgr.OnEstim("s1", json.RawMessage(`{"type":"control","payload":{"type":"device_command","commandId":"c2","sessionId":"s1","command":{"verb":"set_level","channel":"a","level":20}}}`))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, m := svc.findCommand("c2"); m != nil {
			if m["ok"] != false {
				t.Fatalf("a level past the cap was accepted: %v", m)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A hello after a reconnect repeats the device report.
	svc.mu.Lock()
	before := len(svc.posts)
	svc.mu.Unlock()
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
	svc.mu.Lock()
	repeated := false
	for _, p := range svc.posts[before:] {
		for _, m := range p.messages {
			if m["type"] == "device" && m["kind"] == "mastago" {
				repeated = true
			}
		}
	}
	svc.mu.Unlock()
	if !repeated {
		t.Fatal("the device report was not repeated after the reconnect")
	}
	// The service clearing the session releases the device and says so.
	mgr.OnClear("s1", "session ended")
	if _, attached := link.session.Attached(); attached || link.rt.Armed() || unit.Level() != 0 || unit.Outputting() {
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
	if unit.Connections() != 0 || !central.Closed() {
		t.Fatal("exit must disconnect the unit and let go of the Bluetooth central")
	}
}

func TestDeviceFinderRegistry(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := finderConfig{stateDir: t.TempDir(), log: log}
	names := familyNames()
	if len(names) == 0 || names[0] != "Mastago TENS (Bluetooth)" {
		t.Fatalf("families = %v; the Bluetooth unit is tried first", names)
	}
	// Every family on.
	*estimBLE = ""
	t.Cleanup(func() { *estimBLE = "" })
	fs := deviceFinders(cfg)
	if len(fs) != len(families) {
		t.Fatalf("%d finders for %d families", len(fs), len(families))
	}
	if f, ok := fs[0].(*mastago.Finder); !ok || f.Pin != "" {
		t.Fatalf("first finder = %T %+v", fs[0], fs[0])
	}
	// Pinned.
	*estimBLE = "G-12AB"
	if f, ok := deviceFinders(cfg)[0].(*mastago.Finder); !ok || f.Pin != "G-12AB" {
		t.Fatalf("pinned finder = %+v", deviceFinders(cfg)[0])
	}
	// Off.
	*estimBLE = "OFF"
	for _, f := range deviceFinders(cfg) {
		if _, ok := f.(*mastago.Finder); ok {
			t.Fatal("-estim-ble=off still registers the Bluetooth finder")
		}
	}
}

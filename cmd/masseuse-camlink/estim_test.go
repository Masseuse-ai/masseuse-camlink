package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// The service's answers as the session reads them: a 400 (a message it
// cannot read), 413 or 422 is a refusal of the messages themselves
// (estim.ErrRefused, dropped by the session); 401, 408, 429 and 5xx are
// about the moment and are retried as they were.
func TestEstimLinkSendMapsRefusals(t *testing.T) {
	svc := &estimService{}
	srv := httptest.NewServer(svc.handler())
	defer srv.Close()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	link := newEstimLink(t.TempDir(), log, newConsole(io.Discard))
	link.client = &rendezvous.Client{Service: srv.URL, Identity: id, Version: "test", HTTP: srv.Client(), Logger: log}
	msgs := []json.RawMessage{json.RawMessage(`{"type":"device"}`)}
	for _, tc := range []struct {
		status  int
		refused bool
	}{{400, true}, {404, true}, {413, true}, {422, true}, {401, false}, {408, false}, {429, false}, {500, false}, {503, false}} {
		svc.mu.Lock()
		svc.status = tc.status
		svc.mu.Unlock()
		err := link.Send(context.Background(), "", msgs)
		if err == nil {
			t.Fatalf("%d: no error", tc.status)
		}
		if got := errors.Is(err, estim.ErrRefused); got != tc.refused {
			t.Errorf("%d: refused=%v, want %v (%v)", tc.status, got, tc.refused, err)
		}
		if errors.Is(err, estim.ErrSessionGone) {
			t.Errorf("%d: %v", tc.status, err)
		}
	}
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
	link := newEstimLink(t.TempDir(), log, newConsole(&out))
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

// twoUnitLink is an estimLink over a fake central with two units and a
// pipe for the console picker's stdin, running against svc.
func twoUnitLink(t *testing.T, svc *estimService, stateDir string) (*estimLink, *fakeunit.Unit, *fakeunit.Unit, *console, io.WriteCloser, func()) {
	t.Helper()
	srv := httptest.NewServer(svc.handler())
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := fakeunit.New("unit-a", "MASTOGO G-12AB")
	b := fakeunit.New("unit-b", "MASTOGO G-34CD")
	central := &fakeunit.Central{Units: []*fakeunit.Unit{a, b}}
	var out console
	link := newEstimLink(stateDir, log, newConsole(&out))
	finder := mastago.NewFinder(link.selection, log)
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
	link.rt.ListInterval = 100 * time.Millisecond
	pr, pw := io.Pipe()
	link.stdin = pr
	link.client = &rendezvous.Client{Service: srv.URL, Identity: id, Version: "test", HTTP: srv.Client(), Logger: log}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); link.run(ctx) }()
	stop := func() {
		cancel()
		_ = pw.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("link did not stop")
		}
		srv.Close()
	}
	return link, a, b, &out, pw, stop
}

// TestEstimLinkTwoUnits: with two units in reach the report lists both
// with the served one's id, the console prints a numbered list, a number
// typed switches units (remembered in estim.json), a switch is refused
// while a session has the unit armed, and the phone's device_select goes
// the same way.
func TestEstimLinkTwoUnits(t *testing.T) {
	svc := &estimService{}
	stateDir := t.TempDir()
	link, a, b, out, stdin, stop := twoUnitLink(t, svc, stateDir)
	defer stop()

	_, dev := svc.waitFor(t, "device")
	if dev["id"] != "unit-a" || dev["connected"] != true {
		t.Fatalf("device report %v", dev)
	}
	waitUntil(t, "the units listed", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		for _, p := range svc.posts {
			for _, m := range p.messages {
				if units, ok := m["units"].([]any); ok && len(units) == 2 {
					return true
				}
			}
		}
		return false
	})
	waitUntil(t, "the console list", func() bool { return strings.Contains(out.String(), "2  Mastago TENS G-34CD") })
	if s := out.String(); !strings.Contains(s, "Stimulation units in reach (2)") || !strings.Contains(s, "1  Mastago TENS G-12AB  (serving this one)") || !strings.Contains(s, "Type a number and Enter") {
		t.Fatalf("console: %q", s)
	}

	// Typing the other unit's number switches to it and remembers it.
	if _, err := io.WriteString(stdin, "2\n"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the switch to unit-b", func() bool { return link.rt.Descriptor().ID == "unit-b" && a.Connections() == 0 })
	if b.Connections() != 1 {
		t.Fatalf("unit-b connections = %d", b.Connections())
	}
	if unit, err := resolveEstimSelection(stateDir, ""); err != nil || unit != "unit-b" {
		t.Fatalf("remembered selection = %q, %v", unit, err)
	}
	waitUntil(t, "the switch reported", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		for _, p := range svc.posts {
			for _, m := range p.messages {
				if m["type"] == "device" && m["id"] == "unit-b" && m["connected"] == true {
					return true
				}
			}
		}
		return false
	})
	if !strings.Contains(out.String(), "Switching to Mastago TENS G-34CD.") {
		t.Fatalf("console: %q", out.String())
	}
	// A number off the list is said so.
	if _, err := io.WriteString(stdin, "7\n"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the off-list answer", func() bool { return strings.Contains(out.String(), "no unit 7") })

	// Armed by a session: the switch waits for the phone to stop the unit.
	mgr := &manager{log: link.log, estim: link, tunnels: map[string]*active{}}
	mgr.OnEstim("s1", json.RawMessage(`{"type":"control","payload":{"type":"companion_attached","sessionId":"s1"}}`))
	waitUntil(t, "armed", link.rt.Armed)
	if _, err := io.WriteString(stdin, "1\n"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the refusal", func() bool { return strings.Contains(out.String(), "Stop the unit on the phone first") })
	if link.rt.Descriptor().ID != "unit-b" || !link.rt.Armed() {
		t.Fatal("a switch while armed must change nothing")
	}
	// The phone's device_select, connector-level, is refused the same way...
	mgr.OnEstim("", json.RawMessage(`{"type":"control","payload":{"type":"device_select","id":"unit-a"}}`))
	link.session.Wait()
	if link.rt.Descriptor().ID != "unit-b" {
		t.Fatal("device_select while armed must change nothing")
	}
	// ...and goes once the session has stopped the unit.
	mgr.OnClear("s1", "session ended")
	mgr.OnEstim("", json.RawMessage(`{"type":"control","payload":{"type":"device_select","id":"unit-a"}}`))
	waitUntil(t, "the switch to unit-a", func() bool { return link.rt.Descriptor().ID == "unit-a" && b.Connections() == 0 })
	if unit, _ := resolveEstimSelection(stateDir, ""); unit != "unit-a" {
		t.Fatalf("remembered selection = %q", unit)
	}
}

// TestEstimLinkRemembersTheUnit: a saved selection restricts the finders
// at the next start; -estim-unit sets it; "any" forgets it; a family's
// own pin flag is not overridden by a remembered selection.
func TestEstimLinkRemembersTheUnit(t *testing.T) {
	stateDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	quiet := newConsole(io.Discard)
	t.Cleanup(func() { *estimUnit, *estimBLE = "", "" })

	*estimUnit = "G-34CD"
	link := newEstimLink(stateDir, log, quiet)
	if link.selection != "G-34CD" {
		t.Fatalf("selection from the flag = %q", link.selection)
	}
	if f, ok := link.finders[0].(*mastago.Finder); !ok || f.Pin != "G-34CD" {
		t.Fatalf("the finder is pinned to the selection: %+v", link.finders[0])
	}
	if unit, err := resolveEstimSelection(stateDir, ""); err != nil || unit != "G-34CD" {
		t.Fatalf("saved = %q, %v", unit, err)
	}
	// The next start without the flag remembers it.
	*estimUnit = ""
	link = newEstimLink(stateDir, log, quiet)
	if link.selection != "G-34CD" || link.finders[0].(*mastago.Finder).Pin != "G-34CD" {
		t.Fatalf("remembered selection = %q", link.selection)
	}
	// A family's own pin on the command line is this run's word.
	*estimBLE = "G-56EF"
	link = newEstimLink(stateDir, log, quiet)
	if link.selection != "" || link.finders[0].(*mastago.Finder).Pin != "G-56EF" {
		t.Fatalf("-estim-ble overridden: selection=%q pin=%q", link.selection, link.finders[0].(*mastago.Finder).Pin)
	}
	*estimBLE = ""
	// "any" forgets the selection.
	*estimUnit = "any"
	link = newEstimLink(stateDir, log, quiet)
	if link.selection != "" || link.finders[0].(*mastago.Finder).Pin != "" {
		t.Fatalf("after any: selection=%q", link.selection)
	}
	if unit, _ := resolveEstimSelection(stateDir, ""); unit != "" {
		t.Fatalf("any must forget the saved selection: %q", unit)
	}
}

func TestUnitListing(t *testing.T) {
	units := []estim.Unit{
		{ID: "id-a", Kind: estim.KindMastago, Label: "Mastago TENS G-12AB", Held: true},
		{ID: "id-b", Kind: estim.KindMastago, Label: "Mastago TENS G-34CD"},
	}
	serving := &estim.Descriptor{ID: "id-b", Connected: true}
	got := unitListing(units, serving, true)
	want := "Stimulation units in reach (2):\n" +
		"  1  Mastago TENS G-12AB  (another program on this computer has it open)\n" +
		"  2  Mastago TENS G-34CD  (serving this one)\n" +
		"Type a number and Enter to serve another unit; the phone can pick one too. A unit in use by a session is switched once the session stops it.\n"
	if got != want {
		t.Fatalf("listing:\n%s\nwant:\n%s", got, want)
	}
	// Without a console to type at, no instruction; nothing served, no mark.
	got = unitListing(units, &estim.Descriptor{ID: "id-b", Connected: false}, false)
	if strings.Contains(got, "Type a number") || strings.Contains(got, "serving") {
		t.Fatalf("listing without picker:\n%s", got)
	}
}

// TestEstimLinkHeldUnitLine: a unit another program has open is served
// through the shared link, and the console says so.
func TestEstimLinkHeldUnitLine(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	held := fakeunit.New("held-1", "MASTOGO G-12AB")
	central := &fakeunit.Central{Held: []*fakeunit.Unit{held}}
	var out console
	link := newEstimLink(t.TempDir(), log, newConsole(&out))
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
	ctx := context.Background()
	if err := link.rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer link.rt.Close(ctx)
	d := link.rt.Descriptor()
	if !d.Held || d.ID != "held-1" {
		t.Fatalf("descriptor: %+v", d)
	}
	if s := out.String(); !strings.Contains(s, "Stimulation device connected: Mastago TENS G-12AB.") || !strings.Contains(s, heldByAnotherLine) {
		t.Fatalf("console: %q", s)
	}
}

package estim_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312/fakebox"
)

type sent struct {
	sessionID string
	messages  []map[string]any
}

type fakeUplink struct {
	mu   sync.Mutex
	sent []sent
	fail error
}

func (u *fakeUplink) Send(ctx context.Context, sessionID string, messages []json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fail != nil {
		return u.fail
	}
	var decoded []map[string]any
	for _, m := range messages {
		var d map[string]any
		if err := json.Unmarshal(m, &d); err != nil {
			return err
		}
		decoded = append(decoded, d)
	}
	u.sent = append(u.sent, sent{sessionID, decoded})
	return nil
}

// drain flushes and returns every message sent so far, then forgets them.
func (u *fakeUplink) drain(ctx context.Context, s *estim.Session) []sent {
	s.Wait()
	s.Flush(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.sent
	u.sent = nil
	return out
}

func types(batches []sent) []string {
	var out []string
	for _, b := range batches {
		for _, m := range b.messages {
			out = append(out, fmt.Sprint(m["type"]))
		}
	}
	return out
}

func find(batches []sent, typ string) map[string]any {
	for _, b := range batches {
		for _, m := range b.messages {
			if m["type"] == typ {
				return m
			}
		}
	}
	return nil
}

func newSession(t *testing.T) (*estim.Session, *estim.Runtime, *fakebox.Box, *fakeUplink) {
	t.Helper()
	box := fakebox.New()
	rt, _ := boxRuntime(t, box)
	if err := rt.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	up := &fakeUplink{}
	s := &estim.Session{Runtime: rt, Uplink: up}
	return s, rt, box, up
}

func control(payload string) json.RawMessage {
	return json.RawMessage(`{"type":"control","payload":` + payload + `}`)
}

func TestSessionAttachArmsAndReportsState(t *testing.T) {
	ctx := context.Background()
	s, rt, box, up := newSession(t)
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1","levelCap":85,"allowedModes":[118]}`))
	s.Wait()
	if sid, ok := s.Attached(); !ok || sid != "sess-1" {
		t.Fatal("not attached")
	}
	if !rt.Armed() || box.Power() != mk312.PowerHigh {
		t.Fatal("attach must arm")
	}
	got := up.drain(ctx, s)
	// State before arming, then state after: device_status + armed twice,
	// every batch for the session.
	ts := types(got)
	if len(ts) != 4 || ts[0] != "device_status" || ts[1] != "armed" || ts[2] != "device_status" || ts[3] != "armed" {
		t.Fatalf("messages = %v", ts)
	}
	for _, b := range got {
		if b.sessionID != "sess-1" {
			t.Fatalf("batch for %q", b.sessionID)
		}
	}
	last := got[len(got)-1].messages
	armed := last[len(last)-1]
	if armed["armed"] != true || armed["expiresAt"] == nil || armed["heldOff"] != false {
		t.Fatalf("armed = %v", armed)
	}
	status := last[len(last)-2]["status"].(map[string]any)
	if status["connected"] != true || status["power"] != "high" || status["levelA"] != 0.0 {
		t.Fatalf("status = %v", status)
	}
}

func TestSessionCommandsAckAndNack(t *testing.T) {
	ctx := context.Background()
	s, rt, box, up := newSession(t)
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)

	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"c1","sessionId":"sess-1","command":{"verb":"set_level","channel":"a","level":9}}`))
	got := up.drain(ctx, s)
	ack := find(got, "device_ack")
	if ack == nil || ack["commandId"] != "c1" || ack["ok"] != true {
		t.Fatalf("ack = %v", ack)
	}
	if res := ack["result"].(map[string]any); res["verb"] != "set_level" || res["level"] != 9.0 {
		t.Fatalf("result = %v", res)
	}
	if st := ack["status"].(map[string]any); st["levelA"] != 9.0 {
		t.Fatalf("ack status = %v", st)
	}
	if ts := types(got); len(ts) != 3 || ts[1] != "device_status" || ts[2] != "armed" {
		t.Fatalf("ack must be followed by the runtime state: %v", ts)
	}
	if box.LevelA() != 9 {
		t.Fatal("device not at 9")
	}

	// A command for another session is refused; release is not.
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"c2","sessionId":"other","command":{"verb":"set_level","level":1}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false || n["error"] != "live session ID mismatch" {
		t.Fatalf("mismatch nack = %v", n)
	}
	if box.LevelA() != 9 {
		t.Fatal("mismatched command ran")
	}

	// A command that fails caps is nacked and the device released.
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"c3","sessionId":"sess-1","command":{"verb":"set_level","level":86}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false {
		t.Fatalf("cap nack = %v", n)
	}
	if box.LevelA() != 0 || rt.Armed() {
		t.Fatal("a failed command must release")
	}
	// The next heartbeat acknowledgment arms again for the attached session.
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"heartbeat_ack","serverTime":"2026-09-10T00:00:00Z"}`))
	s.Wait()
	if !rt.Armed() {
		t.Fatal("ack after a release must re-arm")
	}
	up.drain(ctx, s)

	// Malformed commands are nacked with the reason.
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"c4","sessionId":"sess-1","command":{"verb":"set_level","level":"9"}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false || n["commandId"] != "c4" {
		t.Fatalf("malformed nack = %v", n)
	}
	// Release verb: outputs down, latch set, acked.
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"c5","sessionId":"sess-1","command":{"verb":"set_level","level":3}}`))
	s.Wait()
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"c6","sessionId":"sess-1","command":{"verb":"release"}}`))
	got = up.drain(ctx, s)
	var acks []map[string]any
	for _, b := range got {
		for _, m := range b.messages {
			if m["type"] == "device_ack" {
				acks = append(acks, m)
			}
		}
	}
	if len(acks) != 2 || acks[1]["ok"] != true || acks[1]["result"].(map[string]any)["released"] != true {
		t.Fatalf("acks = %v", acks)
	}
	if box.LevelA() != 0 || rt.Armed() {
		t.Fatal("release verb did not release")
	}
}

type gate struct {
	estim.Driver
	release chan struct{}
}

func (g gate) Execute(ctx context.Context, cmd estim.Command, cancelled func() bool) (estim.Result, error) {
	<-g.release
	return g.Driver.Execute(ctx, cmd, cancelled)
}

func TestSessionOneCommandAtATime(t *testing.T) {
	ctx := context.Background()
	box := fakebox.New()
	rt, _ := boxRuntime(t, box)
	inner := rt.Connect
	release := make(chan struct{})
	rt.Connect = func(ctx context.Context) (estim.Driver, error) {
		d, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		return gate{d, release}, nil
	}
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	up := &fakeUplink{}
	s := &estim.Session{Runtime: rt, Uplink: up}
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"slow","sessionId":"sess-1","command":{"verb":"set_level","level":2}}`))
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"second","sessionId":"sess-1","command":{"verb":"set_level","level":3}}`))
	s.Flush(ctx)
	up.mu.Lock()
	got := up.sent
	up.sent = nil
	up.mu.Unlock()
	n := find(got, "device_ack")
	if n == nil || n["commandId"] != "second" || n["ok"] != false || n["error"] != "another command is still in progress" {
		t.Fatalf("busy nack = %v", n)
	}
	// Telemetry sampled meanwhile records the device as busy.
	if f := rt.Sample(ctx, time.Now()); !f.Skipped || *f.SkipReason != "busy" {
		t.Fatalf("sample during a command = %+v", f)
	}
	close(release)
	got = up.drain(ctx, s)
	if a := find(got, "device_ack"); a == nil || a["commandId"] != "slow" || a["ok"] != true {
		t.Fatalf("slow ack = %v", a)
	}
}

func TestSessionStaleAckReleasesAndDetaches(t *testing.T) {
	ctx := context.Background()
	s, rt, box, up := newSession(t)
	now := time.Now()
	s.Now = func() time.Time { return now }
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"c1","sessionId":"sess-1","command":{"verb":"set_level","level":4}}`))
	up.drain(ctx, s)
	if box.LevelA() != 4 {
		t.Fatal("setup")
	}
	// Heartbeats keep flowing while acknowledgments are fresh.
	now = now.Add(10 * time.Second)
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"heartbeat_ack"}`))
	now = now.Add(10 * time.Second)
	s.HeartbeatForTest(ctx)
	got := up.drain(ctx, s)
	if ts := types(got); len(ts) < 3 || ts[0] != "heartbeat" {
		t.Fatalf("heartbeat batch = %v", ts)
	}
	if _, ok := s.Attached(); !ok || box.LevelA() != 4 {
		t.Fatal("fresh ack must keep the session")
	}
	// Sixteen seconds without one: released, detached, and the service told.
	now = now.Add(16 * time.Second)
	s.HeartbeatForTest(ctx)
	got = up.drain(ctx, s)
	if _, ok := s.Attached(); ok || box.LevelA() != 0 || rt.Armed() {
		t.Fatal("stale ack must release and detach")
	}
	d := find(got, "detached")
	if d == nil || d["reason"] != "server_heartbeat_stale" {
		t.Fatalf("detached = %v (all %v)", d, types(got))
	}
	// Nothing more goes up for a detached session.
	s.HeartbeatForTest(ctx)
	if got := up.drain(ctx, s); len(got) != 0 {
		t.Fatalf("messages after detach: %v", types(got))
	}
}

func TestSessionServiceDetachAndDeviceReports(t *testing.T) {
	ctx := context.Background()
	s, rt, box, up := newSession(t)
	s.DeviceChanged(ctx, rt.Descriptor())
	got := up.drain(ctx, s)
	if len(got) != 1 || got[0].sessionID != "" {
		t.Fatalf("device report must be connector-level: %+v", got)
	}
	dev := got[0].messages[0]
	if dev["type"] != "device" || dev["kind"] != "mk312bt" || dev["connected"] != true || dev["label"] != mk312.Label {
		t.Fatalf("device = %v", dev)
	}
	caps := dev["capabilities"].(map[string]any)
	if caps["levelMax"] != 85.0 || caps["tempo"] != true || len(caps["modes"].([]any)) != 11 {
		t.Fatalf("capabilities = %v", caps)
	}

	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)
	// The service closing the link: release without a detached message.
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"detach","reason":"companion_replaced"}`))
	got = up.drain(ctx, s)
	if _, ok := s.Attached(); ok || rt.Armed() || box.Power() != mk312.PowerNormal {
		t.Fatal("service detach must release")
	}
	if find(got, "detached") != nil {
		t.Fatal("no detached message is sent back to the service that detached")
	}
	// Commands after that are refused.
	s.Handle(ctx, "sess-1", control(`{"type":"mk312_command","commandId":"c9","sessionId":"sess-1","command":{"verb":"set_level","level":1}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false {
		t.Fatalf("nack after detach = %v", n)
	}
	// A device lost mid-session reports itself and the arm is off.
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)
	box.SetDead(true)
	rt.HealthCheck(ctx)
	s.DeviceChanged(ctx, rt.Descriptor())
	got = up.drain(ctx, s)
	if d := find(got, "device"); d == nil || d["connected"] != false {
		t.Fatalf("device lost report = %v", d)
	}
	if a := find(got, "armed"); a == nil || a["armed"] != false {
		t.Fatalf("armed after loss = %v", a)
	}
	if st := find(got, "device_status")["status"].(map[string]any); st["connected"] != false || st["levelA"] != nil {
		t.Fatalf("status after loss = %v", st)
	}
}

func TestSessionFlushFailures(t *testing.T) {
	ctx := context.Background()
	s, rt, _, up := newSession(t)
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	// A network failure keeps the messages for the next flush.
	up.mu.Lock()
	up.fail = errors.New("network")
	up.mu.Unlock()
	s.Flush(ctx)
	up.mu.Lock()
	up.fail = nil
	up.mu.Unlock()
	if got := up.drain(ctx, s); len(types(got)) != 4 {
		t.Fatalf("retained messages = %v", types(got))
	}
	// A session the service dropped: release and detach.
	up.mu.Lock()
	up.fail = estim.ErrSessionGone
	up.mu.Unlock()
	s.HeartbeatForTest(ctx)
	s.Flush(ctx)
	if _, ok := s.Attached(); ok || rt.Armed() {
		t.Fatal("ErrSessionGone must detach")
	}
	up.mu.Lock()
	up.fail = nil
	up.mu.Unlock()
	up.drain(ctx, s)

	// A service with no device link: sending stops for good.
	s.Handle(ctx, "sess-2", control(`{"type":"companion_attached","sessionId":"sess-2"}`))
	s.Wait()
	up.mu.Lock()
	up.fail = estim.ErrUnsupported
	up.mu.Unlock()
	s.Flush(ctx)
	up.mu.Lock()
	up.fail = nil
	up.mu.Unlock()
	s.HeartbeatForTest(ctx)
	if got := up.drain(ctx, s); len(got) != 0 {
		t.Fatalf("messages after ErrUnsupported: %v", types(got))
	}
}

func TestSessionOutboxBounded(t *testing.T) {
	ctx := context.Background()
	s, _, _, up := newSession(t)
	up.mu.Lock()
	up.fail = errors.New("offline")
	up.mu.Unlock()
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	for i := 0; i < 100; i++ {
		s.Runtime.Telemetry.Offer(estim.Frame{AtMs: int64(i)})
		s.TelemetryForTest(ctx)
		s.HeartbeatForTest(ctx)
	}
	s.Flush(ctx)
	up.mu.Lock()
	up.fail = nil
	up.mu.Unlock()
	got := up.drain(ctx, s)
	n := len(types(got))
	if n > estim.MaxOutbox || n == 0 {
		t.Fatalf("outbox delivered %d messages", n)
	}
	for _, typ := range types(got) {
		if typ == "device_telemetry" {
			t.Fatal("telemetry should be the first thing dropped")
		}
	}
}

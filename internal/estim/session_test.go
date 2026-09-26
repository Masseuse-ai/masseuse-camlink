package estim_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago/fakeunit"
)

// The Session's protocol with the service, over a fake Mastago unit
// (unitRuntime, runtime_mastago_test.go): 25 levels, 15 until a session
// sets its maximum, one power range (named `normal`), a countdown set by
// the arm.

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

// findLast is the most recent message of a type.
func findLast(batches []sent, typ string) map[string]any {
	var last map[string]any
	for _, b := range batches {
		for _, m := range b.messages {
			if m["type"] == typ {
				last = m
			}
		}
	}
	return last
}

func newSession(t *testing.T) (*estim.Session, *estim.Runtime, *fakeunit.Unit, *fakeUplink) {
	t.Helper()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	if err := rt.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	up := &fakeUplink{}
	s := &estim.Session{Runtime: rt, Uplink: up}
	return s, rt, u, up
}

func control(payload string) json.RawMessage {
	return json.RawMessage(`{"type":"control","payload":` + payload + `}`)
}

func TestSessionAttachArmsAndReportsState(t *testing.T) {
	ctx := context.Background()
	s, rt, u, up := newSession(t)
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1","levelCap":15,"allowedModes":[4]}`))
	s.Wait()
	if sid, ok := s.Attached(); !ok || sid != "sess-1" {
		t.Fatal("not attached")
	}
	if !rt.Armed() || u.TimerS() < 19*60 {
		t.Fatalf("attach must arm and set the countdown: armed=%v timer=%d", rt.Armed(), u.TimerS())
	}
	got := up.drain(ctx, s)
	// State before arming and the settings in force, then state after:
	// device_status + armed, device_settings, device_status + armed, every
	// batch for the session.
	ts := types(got)
	if len(ts) != 5 || ts[0] != "device_status" || ts[1] != "armed" || ts[2] != "device_settings" || ts[3] != "device_status" || ts[4] != "armed" {
		t.Fatalf("messages = %v", ts)
	}
	for _, b := range got {
		if b.sessionID != "sess-1" {
			t.Fatalf("batch for %q", b.sessionID)
		}
	}
	if settings := find(got, "device_settings"); settings["powerMode"] != "normal" || settings["levelMax"] != 15.0 {
		t.Fatalf("settings = %v", settings)
	}
	last := got[len(got)-1].messages
	armed := last[len(last)-1]
	if armed["armed"] != true || armed["expiresAt"] == nil || armed["heldOff"] != false {
		t.Fatalf("armed = %v", armed)
	}
	status := last[len(last)-2]["status"].(map[string]any)
	if status["connected"] != true || status["power"] != "normal" || status["levelA"] != 0.0 || status["outputting"] != false || status["loadDetected"] != true || status["timerRemainingS"] == nil {
		t.Fatalf("status = %v", status)
	}
}

func TestSessionSettings(t *testing.T) {
	ctx := context.Background()
	s, rt, u, up := newSession(t)
	defaults := estim.Settings{PowerMode: "normal", LevelMax: 15}
	// Settings before any session attach go nowhere.
	s.Handle(ctx, "sess-1", control(`{"type":"device_settings","sessionId":"sess-1","powerMode":"normal","levelMax":10}`))
	s.Wait()
	if got := up.drain(ctx, s); len(got) != 0 || rt.Settings() != defaults {
		t.Fatalf("settings without a session: %v %+v", types(got), rt.Settings())
	}
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)
	if !rt.Armed() || rt.Settings() != defaults {
		t.Fatalf("the defaults arm: armed=%v settings=%+v", rt.Armed(), rt.Settings())
	}

	// The session's settings while armed at zero: a power range change
	// releases and arms again (the arm is what selects the range, even on
	// a device with one); the service hears the settings and the state.
	s.Handle(ctx, "sess-1", control(`{"type":"device_settings","sessionId":"sess-1","powerMode":"high","levelMax":10}`))
	s.Wait()
	got := up.drain(ctx, s)
	if settings := find(got, "device_settings"); settings == nil || settings["powerMode"] != "high" || settings["levelMax"] != 10.0 {
		t.Fatalf("settings = %v (all %v)", settings, types(got))
	}
	if !rt.Armed() || rt.Settings() != (estim.Settings{PowerMode: "high", LevelMax: 10}) {
		t.Fatalf("armed=%v settings=%+v", rt.Armed(), rt.Settings())
	}
	// The state after the release, then the state armed again.
	if a := find(got, "armed"); a == nil || a["armed"] != false {
		t.Fatalf("released state not reported: %v", types(got))
	}
	if a := findLast(got, "armed"); a == nil || a["armed"] != true {
		t.Fatalf("re-armed state not reported: %v", types(got))
	}

	// Commands run inside them.
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c1","sessionId":"sess-1","command":{"verb":"set_level","level":11}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false {
		t.Fatalf("level over the maximum: %v", n)
	}
	// (the failed command released; the next acknowledgment arms again)
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"heartbeat_ack"}`))
	s.Wait()
	up.drain(ctx, s)
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c2","sessionId":"sess-1","command":{"verb":"set_level","level":8}}`))
	got = up.drain(ctx, s)
	if a := find(got, "device_ack"); a == nil || a["ok"] != true || u.Level() != 8 || !u.Outputting() {
		t.Fatalf("set_level 8: %v level=%d", a, u.Level())
	}

	// A maximum above the level applies in place: the device keeps running.
	s.Handle(ctx, "sess-1", control(`{"type":"device_settings","sessionId":"sess-1","powerMode":"high","levelMax":20}`))
	s.Wait()
	got = up.drain(ctx, s)
	if u.Level() != 8 || !u.Outputting() || !rt.Armed() {
		t.Fatalf("raised maximum must not release: level=%d armed=%v", u.Level(), rt.Armed())
	}
	if settings := find(got, "device_settings"); settings == nil || settings["levelMax"] != 20.0 {
		t.Fatalf("settings = %v", settings)
	}
	if find(got, "armed") != nil {
		t.Fatalf("no state report for a change applied in place: %v", types(got))
	}
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c3","sessionId":"sess-1","command":{"verb":"set_level","level":20}}`))
	got = up.drain(ctx, s)
	if a := find(got, "device_ack"); a == nil || a["ok"] != true || u.Level() != 20 {
		t.Fatalf("set_level 20: %v level=%d", a, u.Level())
	}

	// A maximum below the level: released, then armed again within it.
	s.Handle(ctx, "sess-1", control(`{"type":"device_settings","sessionId":"sess-1","powerMode":"high","levelMax":6}`))
	s.Wait()
	got = up.drain(ctx, s)
	if u.Level() != 0 || u.Outputting() || !rt.Armed() || rt.Settings().LevelMax != 6 {
		t.Fatalf("lowered maximum: level=%d armed=%v settings=%+v", u.Level(), rt.Armed(), rt.Settings())
	}
	if settings := find(got, "device_settings"); settings == nil || settings["levelMax"] != 6.0 {
		t.Fatalf("settings = %v", settings)
	}

	// Refused whole: another session's, or values off the scale; the
	// service is told what stands.
	for _, bad := range []string{
		`{"type":"device_settings","sessionId":"other","powerMode":"normal","levelMax":5}`,
		`{"type":"device_settings","sessionId":"sess-1","powerMode":"low","levelMax":5}`,
		`{"type":"device_settings","sessionId":"sess-1","powerMode":"normal","levelMax":100}`,
		`{"type":"device_settings","sessionId":"sess-1","powerMode":"normal"}`,
	} {
		s.Handle(ctx, "sess-1", control(bad))
		s.Wait()
		got = up.drain(ctx, s)
		if rt.Settings() != (estim.Settings{PowerMode: "high", LevelMax: 6}) {
			t.Fatalf("%s changed the settings: %+v", bad, rt.Settings())
		}
		if strings.Contains(bad, `"sessionId":"sess-1"`) {
			if settings := find(got, "device_settings"); settings == nil || settings["levelMax"] != 6.0 {
				t.Fatalf("%s: the settings in force must be re-reported: %v", bad, types(got))
			}
		} else if len(got) != 0 {
			t.Fatalf("another session's settings drew a reply: %v", types(got))
		}
	}

	// A detach restores the device's defaults; the next session starts from them.
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"detach","reason":"companion_replaced"}`))
	up.drain(ctx, s)
	if rt.Settings() != defaults {
		t.Fatalf("settings after detach = %+v", rt.Settings())
	}
	s.Handle(ctx, "sess-2", control(`{"type":"companion_attached","sessionId":"sess-2"}`))
	s.Wait()
	got = up.drain(ctx, s)
	if settings := find(got, "device_settings"); settings == nil || settings["powerMode"] != "normal" || settings["levelMax"] != 15.0 {
		t.Fatalf("settings for the next session = %v", settings)
	}
	if !rt.Armed() {
		t.Fatal("the next session arms")
	}
	// The same session attaching again keeps what it set.
	s.Handle(ctx, "sess-2", control(`{"type":"device_settings","sessionId":"sess-2","powerMode":"normal","levelMax":12}`))
	s.Wait()
	up.drain(ctx, s)
	s.Handle(ctx, "sess-2", control(`{"type":"companion_attached","sessionId":"sess-2"}`))
	s.Wait()
	got = up.drain(ctx, s)
	if settings := find(got, "device_settings"); settings == nil || settings["levelMax"] != 12.0 {
		t.Fatalf("settings on re-attach = %v", settings)
	}
	// Another session attaching starts from the defaults again.
	s.Handle(ctx, "sess-3", control(`{"type":"companion_attached","sessionId":"sess-3"}`))
	s.Wait()
	got = up.drain(ctx, s)
	if settings := find(got, "device_settings"); settings == nil || settings["levelMax"] != 15.0 || rt.Settings() != defaults {
		t.Fatalf("settings for a third session = %v", settings)
	}
}

func TestSessionCommandsAckAndNack(t *testing.T) {
	ctx := context.Background()
	s, rt, u, up := newSession(t)
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)

	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c1","sessionId":"sess-1","command":{"verb":"set_level","channel":"a","level":9}}`))
	got := up.drain(ctx, s)
	ack := find(got, "device_ack")
	if ack == nil || ack["commandId"] != "c1" || ack["ok"] != true {
		t.Fatalf("ack = %v", ack)
	}
	if res := ack["result"].(map[string]any); res["verb"] != "set_level" || res["level"] != 9.0 {
		t.Fatalf("result = %v", res)
	}
	if st := ack["status"].(map[string]any); st["levelA"] != 9.0 || st["outputting"] != true {
		t.Fatalf("ack status = %v", st)
	}
	if ts := types(got); len(ts) != 3 || ts[1] != "device_status" || ts[2] != "armed" {
		t.Fatalf("ack must be followed by the runtime state: %v", ts)
	}
	if u.Level() != 9 || !u.Outputting() {
		t.Fatal("device not at 9")
	}

	// A command for another session is refused; release is not.
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c2","sessionId":"other","command":{"verb":"set_level","level":1}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false || n["error"] != "live session ID mismatch" || n["code"] != estim.NackSessionMismatch {
		t.Fatalf("mismatch nack = %v", n)
	}
	if u.Level() != 9 {
		t.Fatal("mismatched command ran")
	}

	// A command that fails caps is nacked and the device released.
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c3","sessionId":"sess-1","command":{"verb":"set_level","level":16}}`))
	got = up.drain(ctx, s)
	// A fault of the unit's, not a refusal of the connector's: no code.
	if n := find(got, "device_ack"); n == nil || n["ok"] != false || n["code"] != nil {
		t.Fatalf("cap nack = %v", n)
	}
	if u.Level() != 0 || u.Outputting() || rt.Armed() {
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
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c4","sessionId":"sess-1","command":{"verb":"set_level","level":"9"}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false || n["commandId"] != "c4" {
		t.Fatalf("malformed nack = %v", n)
	}
	// A command the unit itself refuses (a step with the electrodes off
	// the skin) is nacked with the unit's reason, and the device released.
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"heartbeat_ack"}`))
	s.Wait()
	up.drain(ctx, s)
	u.SetLoad(false)
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c4b","sessionId":"sess-1","command":{"verb":"set_level","level":3}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false || !strings.Contains(fmt.Sprint(n["error"]), "electrodes are not on the skin") {
		t.Fatalf("no-contact nack = %v", n)
	}
	if st, _ := find(got, "device_ack")["status"].(map[string]any); st["loadDetected"] != false {
		t.Fatalf("the nack's status must say the pads are off: %v", st)
	}
	if u.Outputting() || rt.Armed() {
		t.Fatal("a refused command must release")
	}
	u.SetLoad(true)
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"heartbeat_ack"}`))
	s.Wait()
	up.drain(ctx, s)
	// Release verb: outputs down, latch set, acked.
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c5","sessionId":"sess-1","command":{"verb":"set_level","level":3}}`))
	s.Wait()
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c6","sessionId":"sess-1","command":{"verb":"release"}}`))
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
	if u.Level() != 0 || u.Outputting() || rt.Armed() {
		t.Fatal("release verb did not release")
	}
}

type gate struct {
	estim.Driver
	entered chan struct{}
	release chan struct{}
}

func (g gate) Execute(ctx context.Context, cmd estim.Command, levelMax int, cancelled func() bool) (estim.Result, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
	return g.Driver.Execute(ctx, cmd, levelMax, cancelled)
}

func TestSessionOneCommandAtATime(t *testing.T) {
	ctx := context.Background()
	rt := unitRuntime(t, fakeunit.New("id-1", "MASTOGO G-12AB"))
	inner := rt.Connect
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	rt.Connect = func(ctx context.Context) (estim.Driver, error) {
		d, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		return gate{d, entered, release}, nil
	}
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	up := &fakeUplink{}
	s := &estim.Session{Runtime: rt, Uplink: up}
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"slow","sessionId":"sess-1","command":{"verb":"set_level","level":2}}`))
	// The slow command holds the device once it reaches the driver.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow command never reached the driver")
	}
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"second","sessionId":"sess-1","command":{"verb":"set_level","level":3}}`))
	s.Flush(ctx)
	up.mu.Lock()
	got := up.sent
	up.sent = nil
	up.mu.Unlock()
	n := find(got, "device_ack")
	if n == nil || n["commandId"] != "second" || n["ok"] != false || n["error"] != "another command is still in progress" || n["code"] != estim.NackBusy {
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
	s, rt, u, up := newSession(t)
	now := time.Now()
	s.Now = func() time.Time { return now }
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c1","sessionId":"sess-1","command":{"verb":"set_level","level":4}}`))
	up.drain(ctx, s)
	if u.Level() != 4 {
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
	if _, ok := s.Attached(); !ok || u.Level() != 4 {
		t.Fatal("fresh ack must keep the session")
	}
	// Sixteen seconds without one: released, detached, and the service told.
	now = now.Add(16 * time.Second)
	s.HeartbeatForTest(ctx)
	got = up.drain(ctx, s)
	if _, ok := s.Attached(); ok || u.Level() != 0 || u.Outputting() || rt.Armed() {
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
	s, rt, u, up := newSession(t)
	s.DeviceChanged(ctx, rt.Descriptor())
	got := up.drain(ctx, s)
	if len(got) != 1 || got[0].sessionID != "" {
		t.Fatalf("device report must be connector-level: %+v", got)
	}
	dev := got[0].messages[0]
	if dev["type"] != "device" || dev["kind"] != "mastago" || dev["connected"] != true || dev["label"] != "Mastago TENS G-12AB" {
		t.Fatalf("device = %v", dev)
	}
	caps := dev["capabilities"].(map[string]any)
	if caps["levelMax"] != 25.0 || caps["levelMaxDefault"] != 15.0 || caps["tempo"] != false || caps["timer"] != true || caps["loadDetect"] != true || len(caps["modes"].([]any)) != 32 {
		t.Fatalf("capabilities = %v", caps)
	}
	if _, ok := caps["powerModes"]; ok {
		t.Fatalf("a device with one power range names none: %v", caps)
	}

	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)
	// The service closing the link: release without a detached message.
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"detach","reason":"companion_replaced"}`))
	got = up.drain(ctx, s)
	if _, ok := s.Attached(); ok || rt.Armed() || u.Outputting() {
		t.Fatal("service detach must release")
	}
	if find(got, "detached") != nil {
		t.Fatal("no detached message is sent back to the service that detached")
	}
	// Commands after that are refused.
	s.Handle(ctx, "sess-1", control(`{"type":"device_command","commandId":"c9","sessionId":"sess-1","command":{"verb":"set_level","level":1}}`))
	got = up.drain(ctx, s)
	if n := find(got, "device_ack"); n == nil || n["ok"] != false {
		t.Fatalf("nack after detach = %v", n)
	}
	// A device lost mid-session (the unit switched itself off) reports
	// itself and the arm is off.
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	up.drain(ctx, s)
	u.AutoOff(0)
	rt.HealthCheck(ctx)
	s.DeviceChanged(ctx, rt.Descriptor())
	got = up.drain(ctx, s)
	if d := find(got, "device"); d == nil || d["connected"] != false || d["reason"] != estim.ReasonIdleOff {
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
	if got := up.drain(ctx, s); len(types(got)) != 5 {
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

func TestSessionDeviceReportNamesTheUnitAndTheList(t *testing.T) {
	ctx := context.Background()
	rt, _, _, _, _, _ := twoUnitRuntime(t)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	up := &fakeUplink{}
	s := &estim.Session{Runtime: rt, Uplink: up}
	rt.OnDevice, rt.OnUnits = s.DeviceChanged, s.UnitsChanged
	// Before any listing: the id, no units.
	s.DeviceChanged(ctx, rt.Descriptor())
	got := up.drain(ctx, s)
	dev := find(got, "device")
	if dev == nil || dev["id"] != "id-a" || dev["connected"] != true {
		t.Fatalf("device = %v", dev)
	}
	if _, ok := dev["units"]; ok {
		t.Fatalf("no units before a listing: %v", dev)
	}
	if _, ok := dev["held"]; ok {
		t.Fatalf("held is only said when true: %v", dev)
	}
	// The listing is a device report of its own, connector-level, with
	// the units.
	if _, err := rt.ListUnits(ctx); err != nil {
		t.Fatal(err)
	}
	got = up.drain(ctx, s)
	if len(got) != 1 || got[0].sessionID != "" {
		t.Fatalf("units report must be connector-level: %+v", got)
	}
	dev = find(got, "device")
	units, _ := dev["units"].([]any)
	if dev["id"] != "id-a" || len(units) != 2 {
		t.Fatalf("device with units = %v", dev)
	}
	first := units[0].(map[string]any)
	if first["id"] != "id-a" || first["kind"] != "mastago" || first["label"] != "Mastago TENS G-12AB" || first["held"] != false {
		t.Fatalf("unit = %v", first)
	}
}

// The units are listed before any unit has been opened (the one in reach
// is held by another program, or the first scan missed it): there is no
// descriptor to report, and a `device` report without a kind is what the
// service refuses as malformed, for good, with everything queued behind
// it. Nothing is sent; the list rides with the report of the first unit
// that connects.
func TestSessionSendsNoDeviceReportBeforeAnyUnitIsKnown(t *testing.T) {
	ctx := context.Background()
	held := errors.New("mastago: the unit is open in another program")
	rt := &estim.Runtime{
		Connect: func(context.Context) (estim.Driver, error) { return nil, held },
		List: func(context.Context) ([]estim.Unit, error) {
			return []estim.Unit{{ID: "id-a", Kind: estim.KindMastago, Label: "Mastago TENS G-12AB", Held: true}}, nil
		},
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })
	up := &fakeUplink{}
	s := &estim.Session{Runtime: rt, Uplink: up}
	rt.OnDevice, rt.OnUnits = s.DeviceChanged, s.UnitsChanged
	if err := rt.Open(ctx); !errors.Is(err, held) {
		t.Fatalf("open: %v", err)
	}
	if _, err := rt.ListUnits(ctx); err != nil {
		t.Fatal(err)
	}
	if got := up.drain(ctx, s); len(got) != 0 {
		t.Fatalf("a report with no device to describe was sent: %+v", got)
	}
	// The unit connects: one report, with the list.
	u := fakeunit.New("id-a", "MASTOGO G-12AB")
	rt.Connect = unitRuntime(t, u).Connect
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	dev := find(up.drain(ctx, s), "device")
	units, _ := dev["units"].([]any)
	if dev == nil || dev["kind"] != "mastago" || dev["id"] != "id-a" || len(units) != 1 {
		t.Fatalf("device = %v", dev)
	}
}

// A batch the service refuses (400: a message it cannot read) is dropped
// with a warning rather than retried: the same bytes would be refused
// again, and everything queued after them would wait forever. The other
// answers keep their meaning: a session gone detaches, a service without
// the link ends sending, anything else is retried.
func TestSessionDropsABatchTheServiceRefuses(t *testing.T) {
	ctx := context.Background()
	s, _, _, up := newSession(t)
	up.drain(ctx, s)
	up.mu.Lock()
	up.fail = fmt.Errorf("%w: service answered 400: {\"error\":\"device report malformed\"}", estim.ErrRefused)
	up.mu.Unlock()
	s.DeviceChanged(ctx, s.Runtime.Descriptor())
	s.Flush(ctx)
	up.mu.Lock()
	up.fail = nil
	up.mu.Unlock()
	if got := up.drain(ctx, s); len(got) != 0 {
		t.Fatalf("the refused batch was retried: %+v", got)
	}
	// A failure of any other kind keeps the messages for the next flush.
	up.mu.Lock()
	up.fail = errors.New("dial tcp: connection refused")
	up.mu.Unlock()
	s.DeviceChanged(ctx, s.Runtime.Descriptor())
	s.Flush(ctx)
	up.mu.Lock()
	up.fail = nil
	up.mu.Unlock()
	if got := types(up.drain(ctx, s)); len(got) == 0 || got[0] != "device" {
		t.Fatalf("the batch was not retried after a network failure: %v", got)
	}
}

func TestSessionDeviceSelect(t *testing.T) {
	ctx := context.Background()
	rt, a, b, _, _, _ := twoUnitRuntime(t)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	up := &fakeUplink{}
	var selected []string
	s := &estim.Session{Runtime: rt, Uplink: up, Select: func(ctx context.Context, id string) error {
		selected = append(selected, id)
		return rt.SelectUnit(ctx, id)
	}}
	rt.OnDevice, rt.OnUnits = s.DeviceChanged, s.UnitsChanged
	// Connector-level control: the phone picked the other unit.
	s.Handle(ctx, "", control(`{"type":"device_select","id":"id-b"}`))
	s.Wait()
	if len(selected) != 1 || selected[0] != "id-b" {
		t.Fatalf("selected = %v", selected)
	}
	if rt.Descriptor().ID != "id-b" || a.Connections() != 0 || b.Connections() != 1 {
		t.Fatalf("after device_select: id=%s a=%d b=%d", rt.Descriptor().ID, a.Connections(), b.Connections())
	}
	got := up.drain(ctx, s)
	var ids []any
	for _, batch := range got {
		for _, m := range batch.messages {
			if m["type"] == "device" {
				ids = append(ids, m["id"], m["connected"])
			}
		}
	}
	if len(ids) != 4 || ids[0] != "id-a" || ids[1] != false || ids[2] != "id-b" || ids[3] != true {
		t.Fatalf("device reports around a switch = %v", ids)
	}
	// Without an id, or without a way to select, the control is ignored.
	s.Handle(ctx, "", control(`{"type":"device_select"}`))
	s.Wait()
	if len(selected) != 1 {
		t.Fatal("device_select without an id must be ignored")
	}
	// Refused while armed: the attached session's current is not cut.
	s.Handle(ctx, "sess-1", control(`{"type":"companion_attached","sessionId":"sess-1"}`))
	s.Wait()
	if !rt.Armed() {
		t.Fatal("attach must arm")
	}
	up.drain(ctx, s)
	s.Handle(ctx, "", control(`{"type":"device_select","id":"id-a"}`))
	s.Wait()
	if rt.Descriptor().ID != "id-b" || !rt.Armed() || b.Connections() != 1 {
		t.Fatal("a device_select while armed must change nothing")
	}
	if len(selected) != 2 {
		t.Fatalf("the hook is asked and refuses: %v", selected)
	}
	// Once the phone stops the unit (the service detaches), the switch goes.
	s.Handle(ctx, "sess-1", json.RawMessage(`{"type":"detach","reason":"stopped"}`))
	s.Handle(ctx, "", control(`{"type":"device_select","id":"id-a"}`))
	s.Wait()
	if rt.Descriptor().ID != "id-a" || b.Connections() != 0 {
		t.Fatalf("after the stop the switch must go: id=%s b=%d", rt.Descriptor().ID, b.Connections())
	}
	none := &estim.Session{Runtime: rt, Uplink: up}
	none.Handle(ctx, "", control(`{"type":"device_select","id":"id-b"}`))
	none.Wait()
	if rt.Descriptor().ID != "id-a" {
		t.Fatal("a session without a Select hook ignores the control")
	}
}

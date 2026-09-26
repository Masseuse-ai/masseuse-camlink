package estim

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Session timings.
const (
	// HeartbeatInterval is how often the connector tells the service the
	// device link is alive, with the device's status and arm state.
	HeartbeatInterval = 5 * time.Second
	// AckStale is how long the service may go without acknowledging a
	// heartbeat before the device is released and the session detached.
	AckStale = 15 * time.Second
	// TelemetryBatchInterval is how often queued frames go up.
	TelemetryBatchInterval = 2 * time.Second
	// TelemetryMaxBatch bounds one upload.
	TelemetryMaxBatch = 32
	// FlushInterval is how often queued messages are sent.
	FlushInterval = time.Second
	// MaxOutbox bounds the messages kept while the service is unreachable;
	// the oldest go first, telemetry before anything else.
	MaxOutbox = 64
)

// An Uplink carries companion messages to the service: those for the
// attached session under its id, connector-level ones (the device
// descriptor) under "".
type Uplink interface {
	Send(ctx context.Context, sessionID string, messages []json.RawMessage) error
}

// ErrUnsupported is what an Uplink returns when the service takes no
// device link at all; the Session then stops sending.
var ErrUnsupported = errors.New("estim: the service takes no device link")

// ErrSessionGone is what an Uplink returns when the service no longer has
// the session bound to this connector; the Session releases and detaches.
var ErrSessionGone = errors.New("estim: the session is no longer bound to this connector")

type queued struct {
	sessionID string
	message   json.RawMessage
	telemetry bool
}

// Session speaks the companion protocol for the one live session the
// service attaches. It arms the device when the session attaches, renews
// the window on every heartbeat acknowledgment, relays commands one at a
// time, and releases the device when the service goes quiet or detaches.
type Session struct {
	Runtime *Runtime
	Uplink  Uplink
	Log     *slog.Logger
	// Now is the clock (time.Now).
	Now func() time.Time

	mu           sync.Mutex
	sessionID    string
	attached     bool
	lastAck      time.Time
	busy         bool
	arming       bool
	lastDeferral string
	outbox       []queued
	unsupported  bool
	wg           sync.WaitGroup
}

func (s *Session) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Session) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Attached reports the attached session, if any.
func (s *Session) Attached() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID, s.attached
}

// -- outbound ------------------------------------------------------------------

func (s *Session) queue(sessionID string, telemetry bool, msg any) {
	raw, err := json.Marshal(msg)
	if err != nil {
		s.log().Error("estim: could not encode a message", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsupported {
		return
	}
	s.outbox = append(s.outbox, queued{sessionID, raw, telemetry})
	s.trimLocked()
}

// trimLocked keeps the outbox bounded: telemetry goes first, then the
// oldest of the rest.
func (s *Session) trimLocked() {
	for len(s.outbox) > MaxOutbox {
		dropped := false
		for i, q := range s.outbox {
			if q.telemetry {
				s.outbox = append(s.outbox[:i], s.outbox[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			s.outbox = s.outbox[1:]
		}
	}
}

func (s *Session) queueRuntimeState(sessionID string) {
	rt := s.Runtime
	s.queue(sessionID, false, map[string]any{"type": "device_status", "status": rt.LastStatus()})
	var expires *string
	armed := rt.Armed()
	if until, ok := rt.ArmedUntil(); ok {
		expires = String(until.UTC().Format(time.RFC3339Nano))
	}
	// heldOff is what a device with a local stop control reports; this
	// connector has none, so the arm is off only for the reasons the
	// status shows.
	s.queue(sessionID, false, map[string]any{"type": "armed", "armed": armed, "expiresAt": expires, "heldOff": false})
}

func (s *Session) queueNack(sessionID string, commandID any, msg string) {
	s.queue(sessionID, false, map[string]any{"type": "device_ack", "commandId": commandID, "ok": false, "error": truncate(msg, 300), "status": s.Runtime.LastStatus()})
}

// Flush sends everything queued, grouped by session. Failures keep the
// messages for the next flush, except that a service without a device link
// ends sending for good and a session the service dropped is detached.
func (s *Session) Flush(ctx context.Context) {
	s.mu.Lock()
	pending := s.outbox
	s.outbox = nil
	s.mu.Unlock()
	for len(pending) > 0 {
		sid := pending[0].sessionID
		n := 1
		for n < len(pending) && pending[n].sessionID == sid {
			n++
		}
		batch := make([]json.RawMessage, n)
		for i := range batch {
			batch[i] = pending[i].message
		}
		err := s.Uplink.Send(ctx, sid, batch)
		switch {
		case err == nil:
			pending = pending[n:]
			continue
		case errors.Is(err, ErrUnsupported):
			s.log().Info("estim: the service takes no device link; the device stays released")
			s.mu.Lock()
			s.unsupported = true
			s.outbox = nil
			s.mu.Unlock()
			return
		case errors.Is(err, ErrSessionGone) && sid != "":
			s.log().Info("estim: the service no longer has the session; releasing", "session", sid)
			pending = pending[n:]
			s.mu.Lock()
			stillIt := s.attached && s.sessionID == sid
			s.mu.Unlock()
			if stillIt {
				s.detach(ctx, "session_unbound", false)
			}
			continue
		case ctx.Err() != nil:
			return
		}
		s.log().Warn("estim: sending to the service failed; will retry", "err", err)
		// Put back what was not sent, behind anything queued meanwhile.
		s.mu.Lock()
		s.outbox = append(pending, s.outbox...)
		s.trimLocked()
		s.mu.Unlock()
		return
	}
}

// -- lifecycle -------------------------------------------------------------------

// DeviceChanged reports the device to the service and, when it has just
// connected while a session is attached, arms it for that session.
func (s *Session) DeviceChanged(ctx context.Context, d Descriptor) {
	msg := map[string]any{"type": "device", "kind": d.Kind, "label": d.Label, "connected": d.Connected, "capabilities": d.Capabilities}
	s.queue("", false, msg)
	s.mu.Lock()
	sid, attached := s.sessionID, s.attached
	s.mu.Unlock()
	if attached {
		s.queueRuntimeState(sid)
		if d.Connected {
			s.autoArm(ctx, "device connected")
		}
	}
}

// Detach releases the device and forgets the session, telling the service
// when tell is set (not when the service itself asked).
func (s *Session) Detach(ctx context.Context, reason string) {
	s.detach(ctx, reason, true)
}

func (s *Session) detach(ctx context.Context, reason string, tell bool) {
	s.mu.Lock()
	sid, attached := s.sessionID, s.attached
	s.attached = false
	s.mu.Unlock()
	s.Runtime.RequestCancel()
	if _, err := s.Runtime.Release(ctx, reason); err != nil {
		s.log().Warn("estim: release on detach failed", "err", err)
	}
	if attached {
		s.log().Info("estim: session detached", "session", sid, "reason", reason)
		if tell {
			s.queue(sid, false, map[string]any{"type": "detached", "reason": reason})
		}
	}
}

// Wait blocks until command goroutines have finished.
func (s *Session) Wait() { s.wg.Wait() }

// Run flushes, heartbeats and ships telemetry until ctx ends; then it
// releases the device, detaches and makes one last attempt to say so.
func (s *Session) Run(ctx context.Context) {
	flush := time.NewTicker(FlushInterval)
	defer flush.Stop()
	heartbeat := time.NewTicker(HeartbeatInterval)
	defer heartbeat.Stop()
	telemetry := time.NewTicker(TelemetryBatchInterval)
	defer telemetry.Stop()
	for {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if _, attached := s.Attached(); attached {
				s.detach(cctx, "connector_stopped", true)
				s.Wait()
				s.Flush(cctx)
			}
			cancel()
			return
		case <-flush.C:
			s.Flush(ctx)
		case <-heartbeat.C:
			s.heartbeat(ctx)
		case <-telemetry.C:
			s.telemetryTick()
		}
	}
}

func (s *Session) telemetryTick() {
	s.mu.Lock()
	sid, attached := s.sessionID, s.attached
	s.mu.Unlock()
	if !attached {
		return
	}
	if frames := s.Runtime.Telemetry.Drain(TelemetryMaxBatch); len(frames) > 0 {
		s.queue(sid, true, map[string]any{"type": "device_telemetry", "frames": frames})
	}
}

func (s *Session) heartbeat(ctx context.Context) {
	s.mu.Lock()
	sid, attached, lastAck := s.sessionID, s.attached, s.lastAck
	s.mu.Unlock()
	if !attached {
		return
	}
	if s.now().Sub(lastAck) > AckStale {
		s.log().Warn("estim: the service stopped acknowledging heartbeats; releasing")
		s.detach(ctx, "server_heartbeat_stale", true)
		return
	}
	s.queue(sid, false, map[string]any{"type": "heartbeat"})
	s.queueRuntimeState(sid)
}

// -- inbound ------------------------------------------------------------------------

type inbound struct {
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	ServerTime any             `json:"serverTime"`
	Message    string          `json:"message"`
	Reason     string          `json:"reason"`
}

type controlPayload struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	CommandID any             `json:"commandId"`
	Command   json.RawMessage `json:"command"`
	IssuedAt  any             `json:"issuedAt"`
}

// Handle takes one message the service sent for sessionID.
func (s *Session) Handle(ctx context.Context, sessionID string, raw json.RawMessage) {
	var m inbound
	if err := json.Unmarshal(raw, &m); err != nil {
		s.log().Debug("estim: ignoring a message that is not JSON", "err", err)
		return
	}
	switch m.Type {
	case "heartbeat_ack":
		s.mu.Lock()
		s.lastAck = s.now()
		attached := s.attached && (sessionID == "" || sessionID == s.sessionID)
		s.mu.Unlock()
		if attached {
			s.renewArm(ctx)
		}
	case "control":
		var p controlPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			s.log().Debug("estim: ignoring a control message with a bad payload", "err", err)
			return
		}
		s.handleControl(ctx, sessionID, p)
	case "detach":
		s.mu.Lock()
		mine := s.attached && (sessionID == "" || sessionID == s.sessionID)
		s.mu.Unlock()
		if mine {
			reason := m.Reason
			if reason == "" {
				reason = "service_detached"
			}
			s.detach(ctx, reason, false)
		}
	case "error":
		s.log().Warn("estim: the service rejected a message", "message", m.Message)
	default:
		s.log().Debug("estim: ignoring message", "type", m.Type)
	}
}

func (s *Session) handleControl(ctx context.Context, envelopeSession string, p controlPayload) {
	switch p.Type {
	case "companion_attached":
		sid := p.SessionID
		if sid == "" {
			sid = envelopeSession
		}
		if sid == "" || (envelopeSession != "" && sid != envelopeSession) {
			s.log().Warn("estim: companion_attached without a usable session id")
			return
		}
		s.mu.Lock()
		if s.attached && s.sessionID != sid {
			s.log().Info("estim: live session changed", "from", s.sessionID, "to", sid)
		}
		s.sessionID, s.attached, s.lastAck = sid, true, s.now()
		s.mu.Unlock()
		s.log().Info("estim: attached to live session", "session", sid)
		s.queueRuntimeState(sid)
		s.autoArm(ctx, "attach")
	case "mk312_command", "device_command":
		s.mu.Lock()
		sid, attached := s.sessionID, s.attached
		s.mu.Unlock()
		if !attached {
			s.queueNack(envelopeSession, p.CommandID, "no live session is attached")
			return
		}
		var cmd Command
		parsed, perr := ParseCommand(p.Command)
		if perr == nil {
			cmd = parsed
		}
		isRelease := perr == nil && cmd.Verb == "release"
		if (p.SessionID != sid || (envelopeSession != "" && envelopeSession != sid)) && !isRelease {
			s.queueNack(sid, p.CommandID, "live session ID mismatch")
			return
		}
		if isRelease {
			s.Runtime.RequestCancel()
		} else {
			s.mu.Lock()
			busy := s.busy
			if !busy {
				s.busy = true
			}
			s.mu.Unlock()
			if busy {
				s.queueNack(sid, p.CommandID, "another command is still in progress")
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runCommand(ctx, sid, p, cmd, perr, isRelease)
		}()
	default:
		s.log().Debug("estim: ignoring control", "type", p.Type)
	}
}

func (s *Session) runCommand(ctx context.Context, sid string, p controlPayload, cmd Command, perr error, isRelease bool) {
	defer func() {
		if !isRelease {
			s.mu.Lock()
			s.busy = false
			s.mu.Unlock()
		}
	}()
	commandID, hasID := p.CommandID.(string)
	if !hasID || perr != nil {
		if isRelease {
			if _, err := s.Runtime.Release(ctx, "server_release"); err != nil {
				s.log().Warn("estim: release failed", "err", err)
			}
			s.queueRuntimeState(sid)
			return
		}
		msg := "malformed command"
		if perr != nil {
			msg = perr.Error()
		}
		s.queueNack(sid, p.CommandID, msg)
		return
	}
	res, status, err := s.Runtime.Execute(ctx, cmd)
	if err != nil {
		if _, rerr := s.Runtime.Release(ctx, "command_failed"); rerr != nil {
			s.log().Warn("estim: release after a failed command failed", "err", rerr)
		}
		s.queue(sid, false, map[string]any{"type": "device_ack", "commandId": commandID, "ok": false, "error": truncate(err.Error(), 300), "status": s.Runtime.LastStatus()})
	} else {
		s.queue(sid, false, map[string]any{"type": "device_ack", "commandId": commandID, "ok": true, "result": res, "status": status})
	}
	s.queueRuntimeState(sid)
}

// -- arming ------------------------------------------------------------------------

// renewArm: a heartbeat acknowledgment while attached means the service is
// alive, so an armed window rolls forward; a session attached but not
// armed gets another arming attempt instead.
func (s *Session) renewArm(ctx context.Context) {
	if s.Runtime.ExtendArm() {
		return
	}
	s.autoArm(ctx, "heartbeat_ack")
}

// autoArm arms for the attached session unless something stands in the
// way. The attach is the consent: the service only attaches for a live
// session the person started from the phone. A refusal is logged once per
// reason so an absent device does not fill the log every five seconds.
func (s *Session) autoArm(ctx context.Context, trigger string) {
	s.mu.Lock()
	if !s.attached || s.arming || s.Runtime.Armed() {
		s.mu.Unlock()
		return
	}
	if !s.Runtime.Connected() {
		s.deferArmLocked("device not connected")
		s.mu.Unlock()
		return
	}
	s.arming = true
	sid := s.sessionID
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := s.Runtime.Arm(ctx)
		s.mu.Lock()
		s.arming = false
		if err != nil {
			s.deferArmLocked(truncate(err.Error(), 160))
		} else {
			s.lastDeferral = ""
		}
		stillAttached := s.attached && s.sessionID == sid
		s.mu.Unlock()
		if err == nil {
			s.log().Info("estim: armed", "trigger", trigger)
		}
		if stillAttached {
			s.queueRuntimeState(sid)
		}
	}()
}

func (s *Session) deferArmLocked(reason string) {
	if reason != s.lastDeferral {
		s.lastDeferral = reason
		s.log().Info("estim: arm deferred", "reason", reason)
	}
}

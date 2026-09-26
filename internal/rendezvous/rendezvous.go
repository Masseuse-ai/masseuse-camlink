// Package rendezvous is the connector's client for the masseuse.ai service
// (docs/PROTOCOL.md, section 2): POST /api/camlink/hello with a signed
// identity, then hold GET /api/camlink/events open and dispatch its events,
// heartbeating while it is. It reconnects forever with jittered exponential
// backoff.
package rendezvous

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/identity"
)

// Dial is the service's instruction to open a tunnel.
type Dial struct {
	SessionID  string
	Origin     string
	Ticket     string
	TicketHash string
	ExpiresAt  time.Time
}

// Handler receives events. Methods are called from the client's goroutine,
// one at a time; they must not block for long.
type Handler interface {
	OnCode(code string, expiresAt time.Time)
	OnPaired(phoneTokenHash string)
	OnDial(d Dial)
	OnClear(sessionID, reason string)
	// OnOnline reports whether the event stream is attached.
	OnOnline(online bool)
}

// Source describes the camera the connector offers through its own stream
// (docs/PROTOCOL.md, section 2.3). The service shows it to the paired phone,
// which decides whether to use it.
type Source struct {
	// Kind is "capture" (the computer's own camera and microphone) or
	// "camera" (a camera on the connector's network the connector proxies).
	Kind string `json:"kind"`
	// Label names it for the person, at most 64 characters.
	Label string `json:"label"`
	// Ready says whether the connector can serve it now (ffmpeg found, the
	// devices present, the camera reachable at startup).
	Ready bool `json:"ready"`
}

// SourceReporter is implemented by a Handler that offers a source; the
// client reports it after every hello (the service forgets a connector's
// source when it forgets the connector).
type SourceReporter interface {
	CurrentSource() (Source, bool)
}

// ErrNoSourceReports is returned by ReportSource when the service has no
// such route: an older service, which shows no source card.
var ErrNoSourceReports = errors.New("rendezvous: the service does not take source reports")

// EstimReceiver is implemented by a Handler that serves a stimulation
// device (docs/PROTOCOL.md, section 7); the client hands it every `estim`
// event. A Handler without it ignores them.
type EstimReceiver interface {
	// OnEstim receives one message the service sent for the session
	// ("" for a connector-level message).
	OnEstim(sessionID string, message json.RawMessage)
}

// ErrNoEstim is returned by PostEstim when the service has no such route:
// an older service, which links no devices.
var ErrNoEstim = errors.New("rendezvous: the service does not take device link messages")

// ErrEstimSessionGone is returned by PostEstim when the service no longer
// has the session bound to this connector.
var ErrEstimSessionGone = errors.New("rendezvous: the session is not bound to this connector")

// Client talks to one service.
type Client struct {
	Service  string
	Identity *identity.Identity
	Version  string
	HTTP     *http.Client
	Logger   *slog.Logger
	// MinBackoff and MaxBackoff bound the reconnect delay; 0 means 1 s / 60 s.
	MinBackoff, MaxBackoff time.Duration
	// IdleTimeout ends a stream that sends nothing (not even keepalives) for
	// this long; 0 means 60 s.
	IdleTimeout time.Duration
}

type helloResponse struct {
	StreamToken     string `json:"streamToken"`
	Code            string `json:"code"`
	CodeExpiresAtMs int64  `json:"codeExpiresAtMs"`
	// HeartbeatEveryMs is how often to POST /api/camlink/heartbeat while
	// the stream is attached; 0 means the service takes none.
	HeartbeatEveryMs int64 `json:"heartbeatEveryMs"`
}

// ErrNoHeartbeats is returned by Heartbeat when the service has no such
// route: an older service, which judges the connector by its stream alone.
var ErrNoHeartbeats = errors.New("rendezvous: the service does not take heartbeats")

// StatusError is a non-2xx answer from the service.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("service answered %d: %s", e.Status, e.Body)
}

func (c *Client) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 0}
}

// Run hellos, streams and reconnects until ctx ends.
func (c *Client) Run(ctx context.Context, h Handler) error {
	minB, maxB := c.MinBackoff, c.MaxBackoff
	if minB == 0 {
		minB = time.Second
	}
	if maxB == 0 {
		maxB = 60 * time.Second
	}
	backoff := minB
	for {
		start := time.Now()
		err := c.once(ctx, h)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = minB
		}
		delay := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
			delay = max(delay, 30*time.Second)
		}
		c.log().Warn("rendezvous: disconnected", "err", err, "retryIn", delay.Round(time.Second).String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, maxB)
	}
}

func (c *Client) once(ctx context.Context, h Handler) error {
	hello, err := c.Hello(ctx)
	if err != nil {
		return err
	}
	if hello.Code != "" {
		h.OnCode(hello.Code, time.UnixMilli(hello.CodeExpiresAtMs))
	}
	if sr, ok := h.(SourceReporter); ok {
		if src, ok := sr.CurrentSource(); ok {
			if err := c.ReportSource(ctx, src); err != nil && !errors.Is(err, ErrNoSourceReports) {
				c.log().Warn("rendezvous: source report failed", "err", err)
			}
		}
	}
	return c.Stream(ctx, hello.StreamToken, time.Duration(hello.HeartbeatEveryMs)*time.Millisecond, h)
}

// Heartbeat tells the service the connector is still there (docs/PROTOCOL.md,
// section 2.4). It is signed like hello.
func (c *Client) Heartbeat(ctx context.Context) error {
	ts := time.Now().Unix()
	key := c.Identity.PublicKeyString()
	body, _ := json.Marshal(map[string]any{
		"key": key,
		"ts":  ts,
		"sig": b64(c.Identity.Sign(identity.HeartbeatMessage(ts, key))),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Service, "/")+"/api/camlink/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "masseuse-camlink/"+c.Version)
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := c.http().Do(req.WithContext(hctx))
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoHeartbeats
	case resp.StatusCode/100 != 2:
		return &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(truncate(string(raw), 200))}
	}
	return nil
}

// heartbeats sends one every interval until ctx ends, or until the service
// says it takes none. Failures are logged and retried at the next tick: the
// service's remedy for a connector that cannot reach it is the same as for
// one that is gone.
func (c *Client) heartbeats(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		err := c.Heartbeat(ctx)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoHeartbeats):
			c.log().Debug("rendezvous: the service takes no heartbeats")
			return
		case ctx.Err() != nil:
			return
		default:
			c.log().Warn("rendezvous: heartbeat failed", "err", err)
		}
	}
}

// ReportSource tells the service which camera the connector offers. It is
// signed like hello, so only the connector can describe itself.
func (c *Client) ReportSource(ctx context.Context, src Source) error {
	if len(src.Label) > 64 {
		src.Label = src.Label[:64]
	}
	ts := time.Now().Unix()
	key := c.Identity.PublicKeyString()
	body, _ := json.Marshal(map[string]any{
		"key":    key,
		"ts":     ts,
		"source": src,
		"sig":    b64(c.Identity.Sign(identity.SourceMessage(ts, key, src.Kind, src.Ready, src.Label))),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Service, "/")+"/api/camlink/source", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "masseuse-camlink/"+c.Version)
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := c.http().Do(req.WithContext(hctx))
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoSourceReports
	case resp.StatusCode/100 != 2:
		return &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(truncate(string(raw), 200))}
	}
	return nil
}

// PostEstim sends device link messages for a session (docs/PROTOCOL.md,
// section 2.5). It is signed like hello over the exact JSON text of the
// messages, so only the connector can speak for its device.
func (c *Client) PostEstim(ctx context.Context, sessionID string, messages []json.RawMessage) error {
	if len(messages) == 0 {
		return nil
	}
	msgs, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	ts := time.Now().Unix()
	key := c.Identity.PublicKeyString()
	body, _ := json.Marshal(map[string]any{
		"key":       key,
		"ts":        ts,
		"sessionId": sessionID,
		// The array travels as the text it was signed as, inside a JSON
		// string: a string survives any decoder byte for byte, where a
		// re-encoded array need not (escaping, number forms).
		"messages": string(msgs),
		"sig":      b64(c.Identity.Sign(identity.EstimMessage(ts, key, sessionID, string(msgs)))),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Service, "/")+"/api/camlink/estim", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "masseuse-camlink/"+c.Version)
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := c.http().Do(req.WithContext(hctx))
	if err != nil {
		return fmt.Errorf("estim: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoEstim
	case resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w: %s", ErrEstimSessionGone, strings.TrimSpace(truncate(string(raw), 200)))
	case resp.StatusCode/100 != 2:
		return &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(truncate(string(raw), 200))}
	}
	return nil
}

// Hello authenticates and returns a stream token.
func (c *Client) Hello(ctx context.Context) (*helloResponse, error) {
	ts := time.Now().Unix()
	key := c.Identity.PublicKeyString()
	paired := c.Identity.PairedHashes()
	body, _ := json.Marshal(map[string]any{
		"key":          key,
		"ts":           ts,
		"pairedPhones": paired,
		"version":      c.Version,
		"sig":          b64(c.Identity.Sign(identity.HelloMessage(ts, key, paired))),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Service, "/")+"/api/camlink/hello", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "masseuse-camlink/"+c.Version)
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := c.http().Do(req.WithContext(hctx))
	if err != nil {
		return nil, fmt.Errorf("hello: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(truncate(string(raw), 200))}
	}
	var out helloResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("hello: %w", err)
	}
	if out.StreamToken == "" {
		return nil, errors.New("hello: no streamToken")
	}
	return &out, nil
}

// Stream attaches to the event stream and dispatches until it ends,
// heartbeating every heartbeatEvery while attached (0: not at all).
func (c *Client) Stream(ctx context.Context, streamToken string, heartbeatEvery time.Duration, h Handler) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, strings.TrimRight(c.Service, "/")+"/api/camlink/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+streamToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "masseuse-camlink/"+c.Version)
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(truncate(string(raw), 200))}
	}
	h.OnOnline(true)
	defer h.OnOnline(false)
	if heartbeatEvery > 0 {
		// Stops with the stream: sctx is cancelled on the way out.
		go c.heartbeats(sctx, heartbeatEvery)
	}

	idle := c.IdleTimeout
	if idle == 0 {
		idle = 60 * time.Second
	}
	timer := time.AfterFunc(idle, cancel)
	defer timer.Stop()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var event string
	var data strings.Builder
	for sc.Scan() {
		timer.Reset(idle)
		line := sc.Text()
		switch {
		case line == "":
			if event != "" || data.Len() > 0 {
				c.dispatch(h, event, data.String())
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
			// keepalive comment
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		if sctx.Err() != nil {
			return errors.New("events: stream went silent")
		}
		return fmt.Errorf("events: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("events: stream ended")
}

func (c *Client) dispatch(h Handler, event, data string) {
	switch event {
	case "code":
		var v struct {
			Code        string `json:"code"`
			ExpiresAtMs int64  `json:"expiresAtMs"`
		}
		if json.Unmarshal([]byte(data), &v) == nil && v.Code != "" {
			h.OnCode(v.Code, time.UnixMilli(v.ExpiresAtMs))
		}
	case "paired":
		var v struct {
			Hash string `json:"phoneTokenHash"`
		}
		if json.Unmarshal([]byte(data), &v) == nil && v.Hash != "" {
			h.OnPaired(v.Hash)
		}
	case "dial":
		var v struct {
			SessionID   string `json:"sessionId"`
			Origin      string `json:"origin"`
			Ticket      string `json:"ticket"`
			TicketHash  string `json:"ticketHash"`
			ExpiresAtMs int64  `json:"expiresAtMs"`
		}
		if json.Unmarshal([]byte(data), &v) != nil || v.Origin == "" || v.Ticket == "" || v.TicketHash == "" {
			c.log().Warn("rendezvous: malformed dial event")
			return
		}
		h.OnDial(Dial{SessionID: v.SessionID, Origin: v.Origin, Ticket: v.Ticket, TicketHash: v.TicketHash, ExpiresAt: time.UnixMilli(v.ExpiresAtMs)})
	case "clear":
		var v struct {
			SessionID string `json:"sessionId"`
			Reason    string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(data), &v)
		h.OnClear(v.SessionID, v.Reason)
	case "estim":
		er, ok := h.(EstimReceiver)
		if !ok {
			return
		}
		var v struct {
			SessionID string          `json:"sessionId"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal([]byte(data), &v) != nil || len(v.Message) == 0 {
			c.log().Warn("rendezvous: malformed estim event")
			return
		}
		er.OnEstim(v.SessionID, v.Message)
	default:
		c.log().Debug("rendezvous: ignoring event", "event", event)
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

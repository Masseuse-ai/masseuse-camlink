package rendezvous

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/identity"
)

type recorder struct {
	mu     sync.Mutex
	events []string
	dials  []Dial
	online []bool
	got    chan string
}

func newRecorder() *recorder { return &recorder{got: make(chan string, 64)} }

func (r *recorder) record(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
	r.got <- s
}
func (r *recorder) OnCode(code string, exp time.Time) { r.record("code:" + code) }
func (r *recorder) OnPaired(h string)                 { r.record("paired:" + h) }
func (r *recorder) OnDial(d Dial) {
	r.mu.Lock()
	r.dials = append(r.dials, d)
	r.mu.Unlock()
	r.record("dial:" + d.SessionID)
}
func (r *recorder) OnClear(id, reason string) { r.record("clear:" + id + ":" + reason) }
func (r *recorder) OnOnline(on bool) {
	r.mu.Lock()
	r.online = append(r.online, on)
	r.mu.Unlock()
	r.record(fmt.Sprintf("online:%v", on))
}

func (r *recorder) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-r.got:
			if got == want {
				return
			}
		case <-deadline:
			r.mu.Lock()
			defer r.mu.Unlock()
			t.Fatalf("never saw %q; events: %v", want, r.events)
		}
	}
}

// fakeService verifies hellos like the trainer does and serves a scripted
// event stream.
type fakeService struct {
	t      *testing.T
	mu     sync.Mutex
	hellos int
	tokens map[string]bool
	events []string // raw SSE blocks to send after the code event
	drop   bool     // end the stream after the scripted events
	block  chan struct{}
	// noSource makes /api/camlink/source answer 404, an error like any
	// other to the client.
	noSource bool
	sources  []Source
	// heartbeatMs is the pace the hello announces (an hour when unset, so
	// a test that is not about heartbeats never sees one); noPace leaves
	// it out of the hello, which the client refuses; noHeartbeat makes the
	// heartbeat route answer 404.
	heartbeatMs int64
	noPace      bool
	noHeartbeat bool
	heartbeats  int
	badBeats    int
	// noEstim makes /api/camlink/estim answer 404; estimGone makes it
	// answer 409 (the session is not bound here).
	noEstim   bool
	estimGone bool
	estim     []estimPost
}

type estimPost struct {
	sessionID string
	messages  []json.RawMessage
}

func (f *fakeService) beats() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats, f.badBeats
}

// offering is a recorder that also offers a source.
type offering struct {
	*recorder
	src Source
}

func (o *offering) CurrentSource() (Source, bool) { return o.src, true }

func (f *fakeService) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/camlink/hello", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key          string   `json:"key"`
			Ts           int64    `json:"ts"`
			PairedPhones []string `json:"pairedPhones"`
			Version      string   `json:"version"`
			Sig          string   `json:"sig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		pub, err := identity.ParsePublicKey(body.Key)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(body.Sig)
		if !ed25519.Verify(pub, identity.HelloMessage(body.Ts, body.Key, body.PairedPhones), sig) {
			http.Error(w, "bad signature", 401)
			return
		}
		if d := time.Since(time.Unix(body.Ts, 0)); d > time.Minute || d < -time.Minute {
			http.Error(w, "stale", 401)
			return
		}
		f.mu.Lock()
		f.hellos++
		tok := fmt.Sprintf("tok-%d", f.hellos)
		f.tokens[tok] = true
		f.mu.Unlock()
		out := map[string]any{"streamToken": tok, "code": "7QK4-N2PX", "codeExpiresAtMs": time.Now().Add(10 * time.Minute).UnixMilli()}
		if !f.noPace {
			pace := f.heartbeatMs
			if pace <= 0 {
				pace = 3_600_000
			}
			out["heartbeatEveryMs"] = pace
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	m.HandleFunc("POST /api/camlink/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if f.noHeartbeat {
			f.mu.Lock()
			f.heartbeats++
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		var body struct {
			Key string `json:"key"`
			Ts  int64  `json:"ts"`
			Sig string `json:"sig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		pub, err := identity.ParsePublicKey(body.Key)
		sig, _ := base64.RawURLEncoding.DecodeString(body.Sig)
		if err != nil || !ed25519.Verify(pub, identity.HeartbeatMessage(body.Ts, body.Key), sig) {
			f.mu.Lock()
			f.badBeats++
			f.mu.Unlock()
			http.Error(w, "bad signature", 401)
			return
		}
		f.mu.Lock()
		f.heartbeats++
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /api/camlink/source", func(w http.ResponseWriter, r *http.Request) {
		if f.noSource {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Key    string          `json:"key"`
			Ts     int64           `json:"ts"`
			Source Source          `json:"source"`
			V      int             `json:"v"`
			Face   json.RawMessage `json:"-"`
			Sig    string          `json:"sig"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// The body always carries the version and both offers (face as
		// null when none), as the service requires.
		var shape struct {
			Source map[string]json.RawMessage `json:"source"`
		}
		_ = json.Unmarshal(raw, &shape)
		if _, ok := shape.Source["share"]; body.V != 2 || !ok {
			http.Error(w, "v must be 2 and share present", 400)
			return
		}
		if _, ok := shape.Source["face"]; !ok {
			http.Error(w, "face must be present (null when none)", 400)
			return
		}
		pub, err := identity.ParsePublicKey(body.Key)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(body.Sig)
		faceKind, faceLabel, faceReady := "", "", false
		if body.Source.Face != nil {
			faceKind, faceLabel, faceReady = body.Source.Face.Kind, body.Source.Face.Label, body.Source.Face.Ready
		}
		msg := identity.SourceMessage(body.Ts, body.Key, body.Source.Kind, body.Source.Ready,
			body.Source.Share.Wanted, faceKind, faceReady, body.Source.Label, faceLabel)
		if !ed25519.Verify(pub, msg, sig) {
			http.Error(w, "bad signature", 401)
			return
		}
		f.mu.Lock()
		f.sources = append(f.sources, body.Source)
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /api/camlink/estim", func(w http.ResponseWriter, r *http.Request) {
		if f.noEstim {
			http.NotFound(w, r)
			return
		}
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
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(body.Sig)
		// The signature covers the messages as sent: the exact JSON text.
		if !ed25519.Verify(pub, identity.EstimMessage(body.Ts, body.Key, body.SessionID, body.Messages), sig) {
			http.Error(w, "bad signature", 401)
			return
		}
		if f.estimGone {
			http.Error(w, "session not bound", 409)
			return
		}
		var msgs []json.RawMessage
		if err := json.Unmarshal([]byte(body.Messages), &msgs); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		f.estim = append(f.estim, estimPost{body.SessionID, msgs})
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	m.HandleFunc("GET /api/camlink/events", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		ok := f.tokens[tok]
		delete(f.tokens, tok)
		f.mu.Unlock()
		if !ok {
			http.Error(w, "bad token", 401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: code\ndata: {\"code\":\"7QK4-N2PX\",\"expiresAtMs\":1}\n\n")
		fl.Flush()
		for _, e := range f.events {
			_, _ = io.WriteString(w, e)
			fl.Flush()
		}
		if f.drop {
			return
		}
		select {
		case <-f.block:
		case <-r.Context().Done():
		}
	})
	return m
}

func newClient(t *testing.T, srv *httptest.Server) (*Client, *identity.Identity) {
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Client{
		Service:    srv.URL,
		Identity:   id,
		Version:    "test",
		HTTP:       srv.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond,
	}, id
}

func TestHelloStreamAndDispatch(t *testing.T) {
	f := &fakeService{t: t, tokens: map[string]bool{}, block: make(chan struct{})}
	f.events = []string{
		": keepalive\n\n",
		"event: paired\ndata: {\"phoneTokenHash\":\"" + strings.Repeat("ab", 32) + "\"}\n\n",
		"event: dial\ndata: {\"sessionId\":\"s1\",\"origin\":\"https://slot-3.tee.masseuse.ai\",\"ticket\":\"dGlja2V0\",\"ticketHash\":\"" + strings.Repeat("cd", 32) + "\",\"expiresAtMs\":1757400000000}\n\n",
		"event: clear\ndata: {\"sessionId\":\"s1\",\"reason\":\"session ended\"}\n\n",
		"event: unknown\ndata: {}\n\n",
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := newClient(t, srv)
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, rec) }()

	rec.wait(t, "code:7QK4-N2PX")
	rec.wait(t, "online:true")
	rec.wait(t, "paired:"+strings.Repeat("ab", 32))
	rec.wait(t, "dial:s1")
	rec.wait(t, "clear:s1:session ended")
	rec.mu.Lock()
	d := rec.dials[0]
	rec.mu.Unlock()
	if d.Origin != "https://slot-3.tee.masseuse.ai" || d.Ticket != "dGlja2V0" || d.TicketHash != strings.Repeat("cd", 32) || d.ExpiresAt.UnixMilli() != 1757400000000 {
		t.Fatalf("dial %+v", d)
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.online) < 2 || rec.online[len(rec.online)-1] != false {
		t.Fatalf("online transitions %v", rec.online)
	}
}

func TestSourceReportAfterHello(t *testing.T) {
	f := &fakeService{t: t, tokens: map[string]bool{}, drop: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, id := newClient(t, srv)
	rec := &offering{recorder: newRecorder(), src: Source{Kind: "capture", Label: "Insta360 Link + Yeti Stereo Microphone", Ready: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, rec) }()
	rec.wait(t, "online:true")
	rec.wait(t, "online:false")
	rec.wait(t, "online:true") // second hello after the drop: reported again
	f.mu.Lock()
	n := len(f.sources)
	first := f.sources[0]
	f.mu.Unlock()
	if n < 2 || first != rec.src {
		t.Fatalf("sources %d %+v", n, first)
	}
	cancel()

	// An explicit report on change, and a label cut to 64 characters.
	long := Source{Kind: "camera", Label: strings.Repeat("x", 80), Ready: false}
	if err := c.ReportSource(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.sources[len(f.sources)-1]
	f.mu.Unlock()
	if last.Kind != "camera" || len(last.Label) != 64 || last.Ready {
		t.Fatalf("last %+v", last)
	}

	// A report signed by someone else is refused.
	other, _ := identity.Load(t.TempDir())
	body, _ := json.Marshal(map[string]any{
		"key": id.PublicKeyString(), "ts": time.Now().Unix(), "v": 2, "source": rec.src,
		"sig": base64.RawURLEncoding.EncodeToString(other.Sign(identity.SourceMessage(time.Now().Unix(), id.PublicKeyString(), "capture", true, false, "", false, rec.src.Label, ""))),
	})
	resp, err := srv.Client().Post(srv.URL+"/api/camlink/source", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("imposter report: %d", resp.StatusCode)
	}

	// A route that is not there is an error like any other: reported,
	// nothing swallowed.
	f.noSource = true
	var se *StatusError
	if err := c.ReportSource(context.Background(), rec.src); !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("404 on the source report: %v", err)
	}
}

func TestSourceReportCarriesTheOffers(t *testing.T) {
	f := &fakeService{t: t, tokens: map[string]bool{}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := newClient(t, srv)
	// The camera alone: the offers travel all the same, share not wanted
	// and face null.
	if err := c.ReportSource(context.Background(), Source{Kind: "capture", Label: "This computer's camera", Ready: true}); err != nil {
		t.Fatal(err)
	}
	// With the offers, the face label cut to 64 characters like the camera's.
	offers := Source{Kind: "capture", Label: "This computer's camera", Ready: true,
		Share: ShareOffer{Wanted: true}, Face: &FaceOffer{Kind: "capture", Label: strings.Repeat("O", 70), Ready: true}}
	if err := c.ReportSource(context.Background(), offers); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := append([]Source(nil), f.sources...)
	f.mu.Unlock()
	if len(got) != 2 || got[0].Share.Wanted || got[0].Face != nil {
		t.Fatalf("camera alone: %+v", got)
	}
	if !got[1].Share.Wanted || got[1].Face == nil || len(got[1].Face.Label) != 64 || !got[1].Face.Ready {
		t.Fatalf("with offers: %+v", got[1])
	}
}

func TestReconnectsWithFreshHello(t *testing.T) {
	f := &fakeService{t: t, tokens: map[string]bool{}, drop: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := newClient(t, srv)
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, rec) }()
	// The stream ends immediately each time; the client must hello again.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := f.hellos
		f.mu.Unlock()
		if n >= 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d hellos", f.hellos)
}

func TestHeartbeatsWhileAttached(t *testing.T) {
	f := &fakeService{t: t, tokens: map[string]bool{}, block: make(chan struct{}), heartbeatMs: 40}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := newClient(t, srv)
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, rec) }()
	rec.wait(t, "online:true")
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, bad := f.beats()
		if bad != 0 {
			t.Fatalf("%d heartbeats had a bad signature", bad)
		}
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d heartbeats", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The stream ends: so do the heartbeats.
	close(f.block)
	rec.wait(t, "online:false")
	cancel()
	time.Sleep(50 * time.Millisecond)
	n, _ := f.beats()
	time.Sleep(200 * time.Millisecond)
	if m, _ := f.beats(); m != n {
		t.Fatalf("heartbeats went on after the stream: %d then %d", n, m)
	}
}

func TestHeartbeatsGoOnThroughA404AndAHelloNeedsThePace(t *testing.T) {
	// A heartbeat route that answers 404 is an error like any other: the
	// beat is warned and the next one still goes.
	f := &fakeService{t: t, tokens: map[string]bool{}, block: make(chan struct{}), heartbeatMs: 20, noHeartbeat: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := newClient(t, srv)
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, rec) }()
	rec.wait(t, "online:true")
	time.Sleep(300 * time.Millisecond)
	if n, _ := f.beats(); n < 3 {
		t.Fatalf("%d heartbeat attempts against a 404, want them to go on", n)
	}
	// A hello that names no heartbeat pace is malformed: refused, never
	// streamed from.
	f2 := &fakeService{t: t, tokens: map[string]bool{}, block: make(chan struct{}), noPace: true}
	srv2 := httptest.NewServer(f2.handler())
	defer srv2.Close()
	c2, _ := newClient(t, srv2)
	if _, err := c2.Hello(context.Background()); err == nil || !strings.Contains(err.Error(), "heartbeatEveryMs") {
		t.Fatalf("hello without a pace: %v", err)
	}
	cancel() // before the servers close, so their streams end
}

func TestHelloRejectsBadSignature(t *testing.T) {
	f := &fakeService{t: t, tokens: map[string]bool{}, block: make(chan struct{})}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := newClient(t, srv)
	// A different identity's signature over the same message is refused: we
	// simulate by pointing the client at an identity whose key the request
	// misreports. Simplest: tamper the service to require a signature the
	// client cannot produce, by running Hello against a handler that flips
	// the key.
	tamper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		other, _ := identity.Load(t.TempDir())
		body["key"] = other.PublicKeyString()
		buf, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/camlink/hello", strings.NewReader(string(buf)))
		resp, err := srv.Client().Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer tamper.Close()
	c.Service = tamper.URL
	_, err := c.Hello(context.Background())
	var se *StatusError
	if err == nil || !asStatus(err, &se) || se.Status != 401 {
		t.Fatalf("err %v", err)
	}
}

func asStatus(err error, target **StatusError) bool {
	se, ok := err.(*StatusError)
	if ok {
		*target = se
	}
	return ok
}

func TestIdleStreamIsDropped(t *testing.T) {
	f := &fakeService{t: t, tokens: map[string]bool{}, block: make(chan struct{})}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := newClient(t, srv)
	c.IdleTimeout = 100 * time.Millisecond
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, rec) }()
	rec.wait(t, "online:true")
	rec.wait(t, "online:false") // the silent stream was abandoned
	rec.wait(t, "online:true")  // and re-established
}

// serving is a recorder that also serves a device.
type serving struct {
	*recorder
}

func (s *serving) OnEstim(sessionID string, message json.RawMessage) {
	s.record("estim:" + sessionID + ":" + string(message))
}

func TestEstimEventAndPost(t *testing.T) {
	f := &fakeService{t: t, tokens: map[string]bool{}, block: make(chan struct{})}
	f.events = []string{
		"event: estim\ndata: {\"sessionId\":\"s1\",\"message\":{\"type\":\"heartbeat_ack\"}}\n\n",
		"event: estim\ndata: {\"sessionId\":\"s1\"}\n\n", // malformed: no message
		"event: estim\ndata: {\"sessionId\":\"\",\"message\":{\"type\":\"detach\"}}\n\n",
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, id := newClient(t, srv)
	rec := &serving{recorder: newRecorder()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, rec) }()
	rec.wait(t, "online:true")
	rec.wait(t, "estim:s1:{\"type\":\"heartbeat_ack\"}")
	rec.wait(t, "estim::{\"type\":\"detach\"}")
	cancel()

	// A handler that serves no device never sees the event.
	plain := newRecorder()
	c.dispatch(plain, "estim", "{\"sessionId\":\"s1\",\"message\":{\"type\":\"heartbeat_ack\"}}")
	plain.mu.Lock()
	n := len(plain.events)
	plain.mu.Unlock()
	if n != 0 {
		t.Fatalf("plain handler saw %v", plain.events)
	}

	msgs := []json.RawMessage{json.RawMessage("{\"type\":\"heartbeat\"}"), json.RawMessage("{\"type\":\"armed\",\"armed\":true}")}
	if err := c.PostEstim(context.Background(), "s1", msgs); err != nil {
		t.Fatal(err)
	}
	if err := c.PostEstim(context.Background(), "", nil); err != nil {
		t.Fatal("nothing to send should be fine")
	}
	f.mu.Lock()
	posts := append([]estimPost(nil), f.estim...)
	f.mu.Unlock()
	if len(posts) != 1 || posts[0].sessionID != "s1" || len(posts[0].messages) != 2 || string(posts[0].messages[1]) != "{\"type\":\"armed\",\"armed\":true}" {
		t.Fatalf("posts %+v", posts)
	}

	// A post signed by someone else is refused.
	other, _ := identity.Load(t.TempDir())
	raw, _ := json.Marshal(msgs)
	ts := time.Now().Unix()
	body, _ := json.Marshal(map[string]any{
		"key": id.PublicKeyString(), "ts": ts, "sessionId": "s1", "messages": string(raw),
		"sig": base64.RawURLEncoding.EncodeToString(other.Sign(identity.EstimMessage(ts, id.PublicKeyString(), "s1", string(raw)))),
	})
	resp, err := srv.Client().Post(srv.URL+"/api/camlink/estim", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("imposter post: %d", resp.StatusCode)
	}

	f.estimGone = true
	if err := c.PostEstim(context.Background(), "s1", msgs); !errors.Is(err, ErrEstimSessionGone) {
		t.Fatalf("unbound session: %v", err)
	}
	f.noEstim = true
	var se *StatusError
	if err := c.PostEstim(context.Background(), "s1", msgs); !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("404 on the device link: %v", err)
	}
}

package rendezvous

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
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
	// noSource makes the service an older one without /api/camlink/source.
	noSource bool
	sources  []Source
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
		_ = json.NewEncoder(w).Encode(map[string]any{"streamToken": tok, "code": "7QK4-N2PX", "codeExpiresAtMs": time.Now().Add(10 * time.Minute).UnixMilli()})
	})
	m.HandleFunc("POST /api/camlink/source", func(w http.ResponseWriter, r *http.Request) {
		if f.noSource {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Key    string `json:"key"`
			Ts     int64  `json:"ts"`
			Source Source `json:"source"`
			Sig    string `json:"sig"`
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
		if !ed25519.Verify(pub, identity.SourceMessage(body.Ts, body.Key, body.Source.Kind, body.Source.Ready, body.Source.Label), sig) {
			http.Error(w, "bad signature", 401)
			return
		}
		f.mu.Lock()
		f.sources = append(f.sources, body.Source)
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
		"key": id.PublicKeyString(), "ts": time.Now().Unix(), "source": rec.src,
		"sig": base64.RawURLEncoding.EncodeToString(other.Sign(identity.SourceMessage(time.Now().Unix(), id.PublicKeyString(), "capture", true, rec.src.Label))),
	})
	resp, err := srv.Client().Post(srv.URL+"/api/camlink/source", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("imposter report: %d", resp.StatusCode)
	}

	// An older service without the route is not an error worth retrying.
	f.noSource = true
	if err := c.ReportSource(context.Background(), rec.src); err != ErrNoSourceReports {
		t.Fatalf("old service: %v", err)
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

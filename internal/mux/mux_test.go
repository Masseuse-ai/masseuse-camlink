package mux

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/FemLed/masseuse-camlink/internal/frame"
)

func pair(t *testing.T) (opener, acceptor *Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	opener = New(c1, true)
	acceptor = New(c2, false)
	t.Cleanup(func() {
		opener.Close()
		acceptor.Close()
	})
	return opener, acceptor
}

// echoAcceptor accepts every OPEN whose target has the given prefix and
// echoes bytes back until the opener half-closes.
func echoAcceptor(t *testing.T, s *Session, targetPrefix string) {
	t.Helper()
	go func() {
		for {
			p, err := s.Accept(context.Background())
			if err != nil {
				return
			}
			if !strings.HasPrefix(p.Target, targetPrefix) {
				_ = p.Refuse("not the target: " + p.Target)
				continue
			}
			st, err := p.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(st, st)
				_ = st.CloseWrite()
			}()
		}
	}()
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOpenAcceptEcho(t *testing.T) {
	opener, acceptor := pair(t)
	echoAcceptor(t, acceptor, "camera")

	st, err := opener.Open(context.Background(), "camera:322")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	payload := randomBytes(t, 1<<20+12345)
	go func() {
		if _, err := st.Write(payload); err != nil {
			t.Error(err)
		}
		_ = st.CloseWrite()
	}()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: %d bytes back, %d sent", len(got), len(payload))
	}
	if _, err := st.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("after EOF: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if opener.lookup(st.ID()) != nil {
		t.Fatal("closed stream still registered")
	}
}

func TestRefuse(t *testing.T) {
	opener, acceptor := pair(t)
	echoAcceptor(t, acceptor, "camera")
	_, err := opener.Open(context.Background(), "8.8.8.8:53")
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err %v, want RefusedError", err)
	}
	if refused.Reason != "not the target: 8.8.8.8:53" {
		t.Fatalf("reason %q", refused.Reason)
	}
	if len(opener.streams) != 0 || len(acceptor.streams) != 0 {
		t.Fatalf("refused stream lingers: %d/%d", len(opener.streams), len(acceptor.streams))
	}
	// The session is still usable afterwards.
	st, err := opener.Open(context.Background(), "camera:1")
	if err != nil {
		t.Fatalf("open after refuse: %v", err)
	}
	st.Reset("done")
}

func TestConcurrentStreams(t *testing.T) {
	opener, acceptor := pair(t)
	echoAcceptor(t, acceptor, "camera")
	const streams = 16
	var wg sync.WaitGroup
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := opener.Open(context.Background(), "camera:1")
			if err != nil {
				t.Error(err)
				return
			}
			defer st.Close()
			payload := randomBytes(t, 200_000+i*1000)
			go func() {
				_, _ = st.Write(payload)
				_ = st.CloseWrite()
			}()
			got, err := io.ReadAll(st)
			if err != nil {
				t.Errorf("stream %d: %v", i, err)
				return
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("stream %d: mismatch", i)
			}
		}(i)
	}
	wg.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		opener.mu.Lock()
		n := len(opener.streams)
		opener.mu.Unlock()
		acceptor.mu.Lock()
		m := len(acceptor.streams)
		acceptor.mu.Unlock()
		if n == 0 && m == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("streams linger after close: opener %d acceptor %d", len(opener.streams), len(acceptor.streams))
}

func TestPeerResetPropagates(t *testing.T) {
	opener, acceptor := pair(t)
	go func() {
		p, err := acceptor.Accept(context.Background())
		if err != nil {
			return
		}
		st, _ := p.Accept()
		buf := make([]byte, 16)
		_, _ = st.Read(buf)
		st.Reset("camera went away")
	}()
	st, err := opener.Open(context.Background(), "camera:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("hello camera")); err != nil {
		t.Fatal(err)
	}
	_, err = st.Read(make([]byte, 8))
	var reset *ResetError
	if !errors.As(err, &reset) || reset.Reason != "camera went away" {
		t.Fatalf("read after peer reset: %v", err)
	}
	if _, err := st.Write([]byte("more")); !errors.As(err, &reset) {
		t.Fatalf("write after peer reset: %v", err)
	}
	if opener.lookup(st.ID()) != nil {
		t.Fatal("reset stream still registered on opener")
	}
}

func TestSessionCloseUnblocksEverything(t *testing.T) {
	opener, acceptor := pair(t)
	echoAcceptor(t, acceptor, "camera")
	st, err := opener.Open(context.Background(), "camera:1")
	if err != nil {
		t.Fatal(err)
	}
	readErr := make(chan error, 1)
	go func() {
		_, err := st.Read(make([]byte, 8))
		readErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	acceptor.Close() // the peer disappears
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("read returned nil after session loss")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not unblock")
	}
	select {
	case <-opener.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("opener did not notice the closed peer")
	}
	if _, err := opener.Open(context.Background(), "camera:1"); err == nil {
		t.Fatal("open succeeded on a dead session")
	}
	if _, err := st.Write([]byte("x")); err == nil {
		t.Fatal("write succeeded on a dead session")
	}
}

func TestSessionResetReason(t *testing.T) {
	opener, acceptor := pair(t)
	go acceptor.CloseWithReason("bad proof")
	select {
	case <-opener.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("opener did not see the reset")
	}
	var reset *ResetError
	if err := opener.Err(); !errors.As(err, &reset) || reset.Reason != "bad proof" {
		t.Fatalf("err %v", err)
	}
}

func TestSlowConsumerIsResetNotDeadlocked(t *testing.T) {
	opener, acceptor := pair(t)
	accepted := make(chan *Stream, 1)
	go func() {
		p, err := acceptor.Accept(context.Background())
		if err != nil {
			return
		}
		st, _ := p.Accept()
		accepted <- st // never read from
	}()
	st, err := opener.Open(context.Background(), "camera:1")
	if err != nil {
		t.Fatal(err)
	}
	slow := <-accepted
	chunk := make([]byte, frame.MaxPayload)
	var writeErr error
	for i := 0; i < (MaxInbound/frame.MaxPayload)+4; i++ {
		if _, writeErr = st.Write(chunk); writeErr != nil {
			break
		}
	}
	if writeErr == nil {
		// The reset races with the last write; one more write must see it.
		deadline := time.Now().Add(2 * time.Second)
		for writeErr == nil && time.Now().Before(deadline) {
			_, writeErr = st.Write(chunk[:1])
		}
	}
	var reset *ResetError
	if !errors.As(writeErr, &reset) || reset.Reason != "receive buffer overflow" {
		t.Fatalf("write err %v, want overflow reset", writeErr)
	}
	if _, err := slow.Read(make([]byte, 1)); err == nil {
		t.Fatal("overflowed stream still readable")
	}
	// A second stream still works: the session survived.
	echoAcceptor(t, acceptor, "camera")
	st2, err := opener.Open(context.Background(), "camera:2")
	if err != nil {
		t.Fatalf("open after overflow: %v", err)
	}
	go func() { _, _ = st2.Write([]byte("ping")); _ = st2.CloseWrite() }()
	got, err := io.ReadAll(st2)
	if err != nil || string(got) != "ping" {
		t.Fatalf("echo after overflow: %q %v", got, err)
	}
}

func TestProtocolErrors(t *testing.T) {
	t.Run("open to opener", func(t *testing.T) {
		c1, c2 := net.Pipe()
		s := New(c1, true)
		defer s.Close()
		defer c2.Close()
		go func() {
			_ = frame.NewWriter(c2).Write(frame.Frame{Type: frame.TypeOpen, Stream: 1, Payload: []byte("x:1")})
			_, _ = io.Copy(io.Discard, c2)
		}()
		select {
		case <-s.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("no failure")
		}
		var perr *ProtocolError
		if !errors.As(s.Err(), &perr) {
			t.Fatalf("err %v", s.Err())
		}
	})
	t.Run("even stream id", func(t *testing.T) {
		c1, c2 := net.Pipe()
		s := New(c1, false)
		defer s.Close()
		defer c2.Close()
		go func() {
			_ = frame.NewWriter(c2).Write(frame.Frame{Type: frame.TypeOpen, Stream: 2, Payload: []byte("x:1")})
			_, _ = io.Copy(io.Discard, c2)
		}()
		select {
		case <-s.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("no failure")
		}
	})
	t.Run("handshake frame after handshake", func(t *testing.T) {
		c1, c2 := net.Pipe()
		s := New(c1, false)
		defer s.Close()
		defer c2.Close()
		go func() {
			_ = frame.NewWriter(c2).Write(frame.Frame{Type: frame.TypeChallenge, Stream: 0, Payload: make([]byte, 32)})
			_, _ = io.Copy(io.Discard, c2)
		}()
		select {
		case <-s.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("no failure")
		}
	})
	t.Run("wrong role", func(t *testing.T) {
		opener, acceptor := pair(t)
		if _, err := acceptor.Open(context.Background(), "x"); err != ErrNotOpener {
			t.Fatal(err)
		}
		if _, err := opener.Accept(context.Background()); err != ErrNotAcceptor {
			t.Fatal(err)
		}
	})
}

func TestOpenCancelled(t *testing.T) {
	opener, _ := pair(t) // nobody accepts
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := opener.Open(ctx, "camera:1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if len(opener.streams) != 0 {
		t.Fatal("cancelled stream lingers")
	}
}

func TestRelayHalfCloseSemantics(t *testing.T) {
	opener, acceptor := pair(t)
	// The acceptor side relays each stream to a local TCP echo server, as the
	// connector does with the camera.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	go func() {
		p, err := acceptor.Accept(context.Background())
		if err != nil {
			return
		}
		conn, err := net.Dial("tcp", p.Target)
		if err != nil {
			_ = p.Refuse(err.Error())
			return
		}
		st, err := p.Accept()
		if err != nil {
			return
		}
		Relay(st, conn, 256<<10)
	}()
	st, err := opener.Open(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	payload := randomBytes(t, 3<<20)
	go func() {
		_, _ = st.Write(payload)
		_ = st.CloseWrite()
	}()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("relay mismatch: %d back, %d sent", len(got), len(payload))
	}
	_ = st.Close()
}

func TestOverWebSocket(t *testing.T) {
	accepted := make(chan *Session, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"camlink.v1"}})
		if err != nil {
			return
		}
		c.SetReadLimit(-1)
		s := New(websocket.NetConn(context.Background(), c, websocket.MessageBinary), false)
		accepted <- s
		<-s.Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ingest/tunnel", &websocket.DialOptions{Subprotocols: []string{"camlink.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(-1)
	opener := New(websocket.NetConn(context.Background(), c, websocket.MessageBinary), true)
	defer opener.Close()
	acceptor := <-accepted
	defer acceptor.Close()
	echoAcceptor(t, acceptor, "camera")

	st, err := opener.Open(ctx, "camera:7441")
	if err != nil {
		t.Fatal(err)
	}
	payload := randomBytes(t, 2<<20)
	go func() {
		_, _ = st.Write(payload)
		_ = st.CloseWrite()
	}()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("websocket echo mismatch")
	}
}

// Package mux multiplexes relayed TCP connections over one ordered byte
// stream (the tunnel WebSocket) using internal/frame.
//
// One side opens streams (the gateway, on behalf of the enclave's RTSP
// server); the other accepts or refuses them (the connector, which dials the
// camera). Data on a stream flows both ways until each side has sent CLOSE,
// or either side sends RESET.
//
// The read loop never blocks on a slow consumer: inbound bytes queue per
// stream up to MaxInbound, and a stream whose consumer falls further behind
// than that is reset. Two streams therefore cannot deadlock each other
// through the shared connection, and memory per stream is bounded.
package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/frame"
)

// MaxInbound is the most undelivered bytes a stream may hold before it is
// reset. Generous for a 4K RTSP stream that the enclave reads continuously;
// small enough that a wedged consumer cannot exhaust memory.
const MaxInbound = 8 << 20

// acceptBacklog is how many unanswered OPENs may queue before further ones
// are refused with "busy".
const acceptBacklog = 16

var (
	// ErrSessionClosed is returned once the underlying connection is gone.
	ErrSessionClosed = errors.New("mux: session closed")
	// ErrStreamClosed is returned by Write after CloseWrite or Close.
	ErrStreamClosed = errors.New("mux: stream closed for writing")
	// ErrNotOpener is returned by Open on the accepting side.
	ErrNotOpener = errors.New("mux: this side does not open streams")
	// ErrNotAcceptor is returned by Accept on the opening side.
	ErrNotAcceptor = errors.New("mux: this side does not accept streams")
)

// RefusedError is returned by Open when the peer refused the stream.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "mux: refused: " + e.Reason }

// ResetError is returned from Read and Write after the peer reset a stream,
// or from Err after the peer reset the whole session.
type ResetError struct{ Reason string }

func (e *ResetError) Error() string { return "mux: reset by peer: " + e.Reason }

// ProtocolError is a peer violation; the session is closed.
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "mux: protocol error: " + e.Msg }

// Session is one multiplexed connection.
type Session struct {
	conn   net.Conn
	w      *frame.Writer
	opener bool

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	err     error

	accept    chan *Pending
	done      chan struct{}
	closeOnce sync.Once
}

// New starts a session over conn. Exactly one side must be the opener. The
// caller has already completed any handshake on the raw connection; from
// here on the session owns conn and reads from it until it fails.
func New(conn net.Conn, opener bool) *Session {
	s := &Session{
		conn:    conn,
		w:       frame.NewWriter(conn),
		opener:  opener,
		streams: make(map[uint32]*Stream),
		nextID:  1,
		accept:  make(chan *Pending, acceptBacklog),
		done:    make(chan struct{}),
	}
	go s.readLoop()
	return s
}

// Done is closed when the session has failed or been closed.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err reports why the session ended (nil while it is alive).
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the session: the connection is closed and every stream fails.
func (s *Session) Close() error {
	return s.fail(ErrSessionClosed)
}

// CloseWithReason tells the peer why (RESET on stream 0), then closes.
func (s *Session) CloseWithReason(reason string) error {
	_ = s.w.Write(frame.Frame{Type: frame.TypeReset, Stream: 0, Payload: []byte(reason)})
	return s.fail(ErrSessionClosed)
}

// protocolError tells the peer why and fails the session with a
// ProtocolError.
func (s *Session) protocolError(msg string) {
	_ = s.w.Write(frame.Frame{Type: frame.TypeReset, Stream: 0, Payload: []byte(msg)})
	s.fail(&ProtocolError{Msg: msg})
}

func (s *Session) fail(err error) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		streams := make([]*Stream, 0, len(s.streams))
		for _, st := range s.streams {
			streams = append(streams, st)
		}
		s.streams = map[uint32]*Stream{}
		s.mu.Unlock()
		for _, st := range streams {
			st.markDead(err)
		}
		close(s.done)
		_ = s.conn.Close()
	})
	return nil
}

// Open asks the peer to dial target and returns the stream once accepted.
func (s *Session) Open(ctx context.Context, target string) (*Stream, error) {
	if !s.opener {
		return nil, ErrNotOpener
	}
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id)
	s.streams[id] = st
	s.mu.Unlock()

	if err := s.w.Write(frame.Frame{Type: frame.TypeOpen, Stream: id, Payload: []byte(target)}); err != nil {
		s.fail(err)
		return nil, err
	}
	select {
	case <-st.opened:
		if st.openErr != nil {
			s.remove(id)
			return nil, st.openErr
		}
		return st, nil
	case <-st.notifyDead:
		return nil, st.deadErr()
	case <-ctx.Done():
		st.Reset("open cancelled")
		return nil, ctx.Err()
	}
}

// Pending is an OPEN the accepting side has not answered yet.
type Pending struct {
	Target string
	st     *Stream
}

// Accept tells the peer the dial succeeded and returns the stream.
func (p *Pending) Accept() (*Stream, error) {
	if err := p.st.s.w.Write(frame.Frame{Type: frame.TypeAccept, Stream: p.st.id}); err != nil {
		p.st.s.fail(err)
		return nil, err
	}
	return p.st, nil
}

// Refuse tells the peer why the stream will not be opened.
func (p *Pending) Refuse(reason string) error {
	p.st.s.remove(p.st.id)
	p.st.markDead(&RefusedError{Reason: reason})
	if err := p.st.s.w.Write(frame.Frame{Type: frame.TypeRefuse, Stream: p.st.id, Payload: []byte(reason)}); err != nil {
		p.st.s.fail(err)
		return err
	}
	return nil
}

// Accept waits for the peer's next OPEN.
func (s *Session) Accept(ctx context.Context) (*Pending, error) {
	if s.opener {
		return nil, ErrNotAcceptor
	}
	select {
	case p := <-s.accept:
		return p, nil
	case <-s.done:
		return nil, s.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) lookup(id uint32) *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *Session) remove(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

func (s *Session) readLoop() {
	r := frame.NewReader(s.conn)
	for {
		f, err := r.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrSessionClosed
			}
			s.fail(err)
			return
		}
		switch f.Type {
		case frame.TypeOpen:
			if s.opener {
				s.protocolError("peer sent OPEN to the opener")
				return
			}
			if f.Stream == 0 || f.Stream%2 == 0 || s.lookup(f.Stream) != nil {
				s.protocolError(fmt.Sprintf("OPEN with stream id %d", f.Stream))
				return
			}
			st := newStream(s, f.Stream)
			s.mu.Lock()
			s.streams[f.Stream] = st
			s.mu.Unlock()
			pending := &Pending{Target: string(f.Payload), st: st}
			select {
			case s.accept <- pending:
			default:
				// Nobody is accepting and the backlog is full: answer now
				// rather than stall every other stream behind this one.
				_ = pending.Refuse("busy")
			}
		case frame.TypeAccept, frame.TypeRefuse:
			st := s.lookup(f.Stream)
			if st == nil {
				continue // already reset locally
			}
			st.openOnce.Do(func() {
				if f.Type == frame.TypeRefuse {
					st.openErr = &RefusedError{Reason: string(f.Payload)}
				}
				close(st.opened)
			})
		case frame.TypeData:
			if st := s.lookup(f.Stream); st != nil {
				st.deliver(f.Payload)
			}
		case frame.TypeClose:
			if st := s.lookup(f.Stream); st != nil {
				st.peerClose()
			}
		case frame.TypeReset:
			if f.Stream == 0 {
				s.fail(&ResetError{Reason: string(f.Payload)})
				return
			}
			if st := s.lookup(f.Stream); st != nil {
				s.remove(f.Stream)
				st.markDead(&ResetError{Reason: string(f.Payload)})
			}
		default:
			// CHALLENGE, PROVE and READY belong to the handshake, which is over.
			s.protocolError("unexpected " + f.Type.String() + " on a running session")
			return
		}
	}
}

// Stream is one relayed connection. It satisfies io.ReadWriteCloser plus
// CloseWrite, like a TCP connection.
type Stream struct {
	s  *Session
	id uint32

	mu          sync.Mutex
	queue       [][]byte
	queued      int
	peerClosed  bool
	writeClosed bool
	dead        bool
	err         error
	notify      chan struct{} // capacity 1: something changed
	notifyDead  chan struct{}
	deadOnce    sync.Once

	writeMu  sync.Mutex
	opened   chan struct{}
	openOnce sync.Once
	openErr  error
}

func newStream(s *Session, id uint32) *Stream {
	return &Stream{
		s:          s,
		id:         id,
		notify:     make(chan struct{}, 1),
		notifyDead: make(chan struct{}),
		opened:     make(chan struct{}),
	}
}

// ID is the stream's identifier within the session.
func (st *Stream) ID() uint32 { return st.id }

func (st *Stream) signal() {
	select {
	case st.notify <- struct{}{}:
	default:
	}
}

func (st *Stream) deliver(p []byte) {
	st.mu.Lock()
	if st.dead || st.peerClosed {
		st.mu.Unlock()
		return
	}
	if st.queued+len(p) > MaxInbound {
		st.mu.Unlock()
		st.Reset("receive buffer overflow")
		return
	}
	st.queue = append(st.queue, p)
	st.queued += len(p)
	st.mu.Unlock()
	st.signal()
}

func (st *Stream) peerClose() {
	st.mu.Lock()
	st.peerClosed = true
	both := st.writeClosed
	st.mu.Unlock()
	st.signal()
	if both {
		st.s.remove(st.id)
	}
}

func (st *Stream) markDead(err error) {
	st.deadOnce.Do(func() {
		st.mu.Lock()
		st.dead = true
		st.err = err
		st.queue = nil
		st.queued = 0
		st.mu.Unlock()
		close(st.notifyDead)
		st.signal()
		st.openOnce.Do(func() {
			st.openErr = err
			close(st.opened)
		})
	})
}

func (st *Stream) deadErr() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.err
}

// Read returns queued bytes, io.EOF after the peer's CLOSE, or the reset or
// session error.
func (st *Stream) Read(p []byte) (int, error) {
	for {
		st.mu.Lock()
		if len(st.queue) > 0 {
			head := st.queue[0]
			n := copy(p, head)
			if n == len(head) {
				st.queue = st.queue[1:]
			} else {
				st.queue[0] = head[n:]
			}
			st.queued -= n
			st.mu.Unlock()
			return n, nil
		}
		if st.dead {
			err := st.err
			st.mu.Unlock()
			return 0, err
		}
		if st.peerClosed {
			st.mu.Unlock()
			return 0, io.EOF
		}
		st.mu.Unlock()
		select {
		case <-st.notify:
		case <-st.s.done:
		}
	}
}

// Write sends p as DATA frames.
func (st *Stream) Write(p []byte) (int, error) {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	written := 0
	for len(p) > 0 {
		st.mu.Lock()
		if st.dead {
			err := st.err
			st.mu.Unlock()
			return written, err
		}
		if st.writeClosed {
			st.mu.Unlock()
			return written, ErrStreamClosed
		}
		st.mu.Unlock()
		n := min(len(p), frame.MaxPayload)
		if err := st.s.w.Write(frame.Frame{Type: frame.TypeData, Stream: st.id, Payload: p[:n]}); err != nil {
			st.s.fail(err)
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// CloseWrite sends CLOSE: no more data from this side. Reads still work.
func (st *Stream) CloseWrite() error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	st.mu.Lock()
	if st.dead {
		st.mu.Unlock()
		return nil
	}
	if st.writeClosed {
		st.mu.Unlock()
		return nil
	}
	st.writeClosed = true
	both := st.peerClosed
	st.mu.Unlock()
	if err := st.s.w.Write(frame.Frame{Type: frame.TypeClose, Stream: st.id}); err != nil {
		st.s.fail(err)
		return err
	}
	if both {
		st.s.remove(st.id)
	}
	return nil
}

// Reset aborts the stream in both directions.
func (st *Stream) Reset(reason string) {
	st.mu.Lock()
	if st.dead {
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	st.s.remove(st.id)
	st.markDead(&ResetError{Reason: reason})
	if err := st.s.w.Write(frame.Frame{Type: frame.TypeReset, Stream: st.id, Payload: []byte(reason)}); err != nil {
		st.s.fail(err)
	}
}

// Close finishes the stream. If the peer has already closed its side this
// is a clean close; otherwise it resets so the peer tears down promptly.
func (st *Stream) Close() error {
	st.mu.Lock()
	clean := st.peerClosed || st.dead
	st.mu.Unlock()
	if clean {
		_ = st.CloseWrite()
		st.s.remove(st.id)
		st.markDead(io.ErrClosedPipe)
		return nil
	}
	st.Reset("closed")
	return nil
}

// Relay copies both ways between st and conn until both directions end,
// then closes both. buf sizes the copies (256 KiB suits a video stream).
func Relay(st *Stream, conn net.Conn, bufSize int) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, bufSize)
		_, err := io.CopyBuffer(conn, st, buf)
		if err != nil && !errors.Is(err, io.EOF) {
			_ = conn.Close()
			return
		}
		closeWrite(conn)
	}()
	buf := make([]byte, bufSize)
	_, err := io.CopyBuffer(st, conn, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		st.Reset("local connection failed")
	} else {
		_ = st.CloseWrite()
	}
	// Give the other direction a moment to drain after our half-close, then
	// tear everything down.
	waitOrTimeout(&wg, 30*time.Second)
	_ = st.Close()
	_ = conn.Close()
}

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

func waitOrTimeout(wg *sync.WaitGroup, d time.Duration) {
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(d):
	}
}

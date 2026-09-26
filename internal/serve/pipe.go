package serve

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by Dial once the server is gone.
var ErrClosed = errors.New("serve: server closed")

// pipeListener hands the RTSP server the connections Dial creates. It has no
// socket: each connection is one end of a net.Pipe whose other end the
// tunnel relays to the enclave.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
	seq    atomic.Uint32
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn, 16), closed: make(chan struct{})}
}

// Accept implements net.Listener.
func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close implements net.Listener.
func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Addr implements net.Listener: the address the enclave names.
func (l *pipeListener) Addr() net.Addr { return listenAddr }

var (
	loopback   = net.IPv4(127, 0, 0, 1)
	listenAddr = &net.TCPAddr{IP: loopback, Port: Port}
)

// dial makes a connection pair: the returned end is the peer's (the tunnel
// relays it), the other is queued for Accept. The RTSP server reads TCP
// addresses off its connections, so both ends carry loopback ones.
func (l *pipeListener) dial() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, ErrClosed
	default:
	}
	peer := &net.TCPAddr{IP: loopback, Port: 40000 + int(l.seq.Add(1)%20000)}
	client, server := net.Pipe()
	select {
	case l.conns <- &addrConn{Conn: server, local: listenAddr, remote: peer}:
	case <-l.closed:
		_ = client.Close()
		_ = server.Close()
		return nil, ErrClosed
	}
	return &addrConn{Conn: client, local: peer, remote: listenAddr}, nil
}

// addrConn gives a pipe end TCP addresses.
type addrConn struct {
	net.Conn
	local, remote net.Addr
}

func (c *addrConn) LocalAddr() net.Addr  { return c.local }
func (c *addrConn) RemoteAddr() net.Addr { return c.remote }

// Package frame is the wire format shared by the connector and the gateway
// (docs/PROTOCOL.md, section 4).
//
// A frame is a 9-byte header followed by a payload:
//
//	type   uint8
//	stream uint32 big-endian
//	length uint32 big-endian, at most MaxPayload
//	payload [length]byte
//
// Frames travel over a single ordered byte stream (the WebSocket, as a
// net.Conn). The codec is deliberately tiny: there is nothing to negotiate
// and nothing optional, so a reader either has a frame or an error.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Type identifies a frame.
type Type uint8

// Frame types. Stream 0 carries the tunnel handshake (Challenge, Prove,
// Ready) and a whole-tunnel Reset; every other stream is one relayed TCP
// connection.
const (
	TypeOpen      Type = 1 // gateway -> connector: payload is the host:port to dial
	TypeAccept    Type = 2 // connector -> gateway: the dial succeeded
	TypeRefuse    Type = 3 // connector -> gateway: payload is the reason
	TypeData      Type = 4 // either direction: payload is relayed bytes
	TypeClose     Type = 5 // either direction: no more Data from the sender
	TypeReset     Type = 6 // either direction: abort the stream, payload is the reason
	TypeChallenge Type = 7 // gateway -> connector, stream 0: 32 random bytes
	TypeProve     Type = 8 // connector -> gateway, stream 0: Ed25519 signature
	TypeReady     Type = 9 // gateway -> connector, stream 0: the tunnel is up
)

const (
	// HeaderSize is the fixed size of a frame header.
	HeaderSize = 9
	// MaxPayload bounds a frame so a reader can allocate ahead of time.
	MaxPayload = 65536
	// ChallengeSize is the payload size of a Challenge frame.
	ChallengeSize = 32
)

var (
	// ErrShort is returned by Decode when b holds less than one frame.
	ErrShort = errors.New("frame: short buffer")
	// ErrTooLarge is returned when a payload length exceeds MaxPayload.
	ErrTooLarge = errors.New("frame: payload too large")
	// ErrBadType is returned for a type outside the defined set.
	ErrBadType = errors.New("frame: unknown type")
)

// Frame is one decoded frame. Payload aliases the buffer it was decoded
// from; copy it if it must outlive the buffer.
type Frame struct {
	Type    Type
	Stream  uint32
	Payload []byte
}

// Valid reports whether t is a defined type.
func (t Type) Valid() bool { return t >= TypeOpen && t <= TypeReady }

// String names the type for logs.
func (t Type) String() string {
	switch t {
	case TypeOpen:
		return "OPEN"
	case TypeAccept:
		return "ACCEPT"
	case TypeRefuse:
		return "REFUSE"
	case TypeData:
		return "DATA"
	case TypeClose:
		return "CLOSE"
	case TypeReset:
		return "RESET"
	case TypeChallenge:
		return "CHALLENGE"
	case TypeProve:
		return "PROVE"
	case TypeReady:
		return "READY"
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// Encode appends the wire form of f to dst and returns the extended slice.
func Encode(dst []byte, f Frame) ([]byte, error) {
	if !f.Type.Valid() {
		return dst, ErrBadType
	}
	if len(f.Payload) > MaxPayload {
		return dst, ErrTooLarge
	}
	var hdr [HeaderSize]byte
	hdr[0] = byte(f.Type)
	binary.BigEndian.PutUint32(hdr[1:5], f.Stream)
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(f.Payload)))
	dst = append(dst, hdr[:]...)
	return append(dst, f.Payload...), nil
}

// Decode parses the first frame in b and returns it with the number of
// bytes it occupied. ErrShort means b needs more bytes; any other error
// means the stream is corrupt.
func Decode(b []byte) (Frame, int, error) {
	if len(b) < HeaderSize {
		return Frame{}, 0, ErrShort
	}
	t := Type(b[0])
	if !t.Valid() {
		return Frame{}, 0, ErrBadType
	}
	n := binary.BigEndian.Uint32(b[5:9])
	if n > MaxPayload {
		return Frame{}, 0, ErrTooLarge
	}
	total := HeaderSize + int(n)
	if len(b) < total {
		return Frame{}, 0, ErrShort
	}
	return Frame{Type: t, Stream: binary.BigEndian.Uint32(b[1:5]), Payload: b[HeaderSize:total]}, total, nil
}

// Reader reads frames from a byte stream. It is not safe for concurrent
// use; a session has exactly one reader.
type Reader struct {
	r   io.Reader
	hdr [HeaderSize]byte
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Read blocks for the next frame. The returned payload is freshly
// allocated and owned by the caller. At a clean end of stream between
// frames it returns io.EOF; inside a frame, io.ErrUnexpectedEOF.
func (r *Reader) Read() (Frame, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		return Frame{}, err
	}
	t := Type(r.hdr[0])
	if !t.Valid() {
		return Frame{}, ErrBadType
	}
	n := binary.BigEndian.Uint32(r.hdr[5:9])
	if n > MaxPayload {
		return Frame{}, ErrTooLarge
	}
	f := Frame{Type: t, Stream: binary.BigEndian.Uint32(r.hdr[1:5])}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r.r, f.Payload); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}
	return f, nil
}

// Writer writes whole frames to a byte stream. It is safe for concurrent
// use: each frame is written in one call under a lock, so frames from
// different streams never interleave.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, buf: make([]byte, 0, HeaderSize+MaxPayload)}
}

// Write encodes and writes f.
func (w *Writer) Write(f Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	buf, err := Encode(w.buf[:0], f)
	if err != nil {
		return err
	}
	w.buf = buf[:0]
	_, err = w.w.Write(buf)
	return err
}

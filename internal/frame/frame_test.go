package frame

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
	}{
		{"open", Frame{Type: TypeOpen, Stream: 1, Payload: []byte("192.168.1.108:7441")}},
		{"accept empty", Frame{Type: TypeAccept, Stream: 1}},
		{"refuse", Frame{Type: TypeRefuse, Stream: 3, Payload: []byte("not the session target")}},
		{"data", Frame{Type: TypeData, Stream: 0xFFFFFFFF, Payload: bytes.Repeat([]byte{0xAB}, 1000)}},
		{"data max", Frame{Type: TypeData, Stream: 7, Payload: bytes.Repeat([]byte{1}, MaxPayload)}},
		{"close", Frame{Type: TypeClose, Stream: 5}},
		{"reset", Frame{Type: TypeReset, Stream: 0, Payload: []byte("bad proof")}},
		{"challenge", Frame{Type: TypeChallenge, Stream: 0, Payload: bytes.Repeat([]byte{9}, ChallengeSize)}},
		{"prove", Frame{Type: TypeProve, Stream: 0, Payload: bytes.Repeat([]byte{2}, 64)}},
		{"ready", Frame{Type: TypeReady, Stream: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := Encode(nil, tc.f)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if len(wire) != HeaderSize+len(tc.f.Payload) {
				t.Fatalf("wire length %d, want %d", len(wire), HeaderSize+len(tc.f.Payload))
			}
			got, n, err := Decode(append(wire, 0xEE, 0xEE))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if n != len(wire) {
				t.Fatalf("consumed %d, want %d", n, len(wire))
			}
			if got.Type != tc.f.Type || got.Stream != tc.f.Stream || !bytes.Equal(got.Payload, tc.f.Payload) {
				t.Fatalf("decoded %+v, want %+v", got, tc.f)
			}
			// The streaming reader agrees with Decode.
			r := NewReader(bytes.NewReader(wire))
			rf, err := r.Read()
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
			if rf.Type != tc.f.Type || rf.Stream != tc.f.Stream || !bytes.Equal(rf.Payload, tc.f.Payload) {
				t.Fatalf("reader decoded %+v, want %+v", rf, tc.f)
			}
			if _, err := r.Read(); err != io.EOF {
				t.Fatalf("after last frame: %v, want io.EOF", err)
			}
		})
	}
}

func TestEncodeRejects(t *testing.T) {
	if _, err := Encode(nil, Frame{Type: 0}); !errors.Is(err, ErrBadType) {
		t.Fatalf("type 0: %v", err)
	}
	if _, err := Encode(nil, Frame{Type: TypeReady + 1}); !errors.Is(err, ErrBadType) {
		t.Fatalf("type 10: %v", err)
	}
	if _, err := Encode(nil, Frame{Type: TypeData, Payload: make([]byte, MaxPayload+1)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		err  error
	}{
		{"empty", nil, ErrShort},
		{"partial header", []byte{4, 0, 0, 0}, ErrShort},
		{"bad type", []byte{0, 0, 0, 0, 1, 0, 0, 0, 0}, ErrBadType},
		{"type 200", []byte{200, 0, 0, 0, 1, 0, 0, 0, 0}, ErrBadType},
		{"too large", []byte{4, 0, 0, 0, 1, 0, 1, 0, 1}, ErrTooLarge},
		{"truncated payload", []byte{4, 0, 0, 0, 1, 0, 0, 0, 3, 1, 2}, ErrShort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, n, err := Decode(tc.b)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
			if n != 0 {
				t.Fatalf("consumed %d on error", n)
			}
		})
	}
}

func TestReaderErrors(t *testing.T) {
	// Truncated inside the payload is not a clean EOF.
	wire, _ := Encode(nil, Frame{Type: TypeData, Stream: 1, Payload: []byte("hello")})
	_, err := NewReader(bytes.NewReader(wire[:len(wire)-2])).Read()
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated payload: %v", err)
	}
	// Truncated inside the header is also unexpected.
	_, err = NewReader(bytes.NewReader(wire[:4])).Read()
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated header: %v", err)
	}
	// Oversize length and bad type are refused before allocating.
	_, err = NewReader(bytes.NewReader([]byte{4, 0, 0, 0, 1, 0xFF, 0xFF, 0xFF, 0xFF})).Read()
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	_, err = NewReader(bytes.NewReader([]byte{42, 0, 0, 0, 1, 0, 0, 0, 0})).Read()
	if !errors.Is(err, ErrBadType) {
		t.Fatalf("bad type: %v", err)
	}
}

func TestWriterConcurrentFramesDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	const writers, perWriter = 8, 200
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte(id)}, 100+id*37)
			for j := 0; j < perWriter; j++ {
				if err := w.Write(Frame{Type: TypeData, Stream: uint32(id), Payload: payload}); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	r := NewReader(&buf)
	counts := make([]int, writers)
	for {
		f, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		id := int(f.Stream)
		if len(f.Payload) != 100+id*37 {
			t.Fatalf("stream %d payload length %d", id, len(f.Payload))
		}
		for _, b := range f.Payload {
			if int(b) != id {
				t.Fatalf("stream %d payload byte %d: interleaved write", id, b)
			}
		}
		counts[id]++
	}
	for id, c := range counts {
		if c != perWriter {
			t.Fatalf("stream %d: %d frames, want %d", id, c, perWriter)
		}
	}
}

func TestTypeString(t *testing.T) {
	if TypeOpen.String() != "OPEN" || TypeReady.String() != "READY" {
		t.Fatal("names")
	}
	if Type(77).String() != "type(77)" {
		t.Fatalf("unknown: %s", Type(77))
	}
}

// FuzzDecode: Decode never panics, never over-consumes, and re-encoding a
// decoded frame reproduces the bytes it was decoded from.
func FuzzDecode(f *testing.F) {
	seed, _ := Encode(nil, Frame{Type: TypeOpen, Stream: 1, Payload: []byte("10.0.0.5:322")})
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{4, 0, 0, 0, 1, 0, 0, 0, 0})
	f.Add([]byte{9, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, n, err := Decode(b)
		if err != nil {
			if n != 0 {
				t.Fatalf("consumed %d with error %v", n, err)
			}
			return
		}
		if n < HeaderSize || n > len(b) {
			t.Fatalf("consumed %d of %d", n, len(b))
		}
		again, err := Encode(nil, fr)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(again, b[:n]) {
			t.Fatalf("re-encode mismatch")
		}
		rf, err := NewReader(bytes.NewReader(b)).Read()
		if err != nil {
			t.Fatalf("reader disagrees with Decode: %v", err)
		}
		if rf.Type != fr.Type || rf.Stream != fr.Stream || !bytes.Equal(rf.Payload, fr.Payload) {
			t.Fatal("reader and Decode differ")
		}
	})
}

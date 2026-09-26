package serve

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

func TestH264KeyStart(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"idr", []byte{0x65, 0x88}, true},
		{"sps", []byte{0x67, 0x42}, true},
		{"pps", []byte{0x68, 0xce}, true},
		{"non-idr slice", []byte{0x41, 0x9a}, false},
		{"sei", []byte{0x06, 0x05}, false},
		{"stap-a with sps", []byte{0x78, 0x00, 0x02, 0x67, 0x42, 0x00, 0x02, 0x68, 0xce}, true},
		{"stap-a without key", []byte{0x78, 0x00, 0x02, 0x41, 0x9a, 0x00, 0x01, 0x06}, false},
		{"stap-a truncated", []byte{0x78, 0x00, 0x09, 0x41}, false},
		{"fu-a start idr", []byte{0x7c, 0x85, 0x88}, true},
		{"fu-a middle idr", []byte{0x7c, 0x05, 0x88}, false},
		{"fu-a start non-idr", []byte{0x7c, 0x81, 0x9a}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		if got := h264KeyStart(tc.payload); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestH265KeyStart(t *testing.T) {
	nal := func(typ byte) []byte { return []byte{typ << 1, 0x01, 0xaf} }
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"idr_w_radl", nal(19), true},
		{"cra", nal(21), true},
		{"vps", nal(32), true},
		{"trail_r", nal(1), false},
		{"ap with sps", append([]byte{48 << 1, 0x01, 0x00, 0x03}, nal(33)...), true},
		{"ap without key", append([]byte{48 << 1, 0x01, 0x00, 0x03}, nal(1)...), false},
		{"fu start idr", []byte{49 << 1, 0x01, 0x80 | 19, 0xaf}, true},
		{"fu middle idr", []byte{49 << 1, 0x01, 19, 0xaf}, false},
		{"short", []byte{0x26}, false},
	}
	for _, tc := range cases {
		if got := h265KeyStart(tc.payload); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// videoTrack is an H.264 track as ffmpeg announces it.
func videoTrack() *track {
	return newTrack(&description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}})
}

func vpkt(seq uint16, ts uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 0x1234}, Payload: payload}
}

func TestGateDropsWholeFramesAndResumesAtKeyframe(t *testing.T) {
	tr := videoTrack()
	now := time.Now()
	// Frame 1 (two packets), not congested: both admitted.
	if ok, _ := tr.admit(vpkt(1, 3000, 0x41), false, 0, now); !ok {
		t.Fatal("clear frame dropped")
	}
	if ok, _ := tr.admit(vpkt(2, 3000, 0x41), false, 0, now); !ok {
		t.Fatal("clear frame dropped")
	}
	// Congestion arrives mid-frame 2: the frame already started stays whole.
	if ok, _ := tr.admit(vpkt(3, 6000, 0x41), false, 0, now); !ok {
		t.Fatal("frame start dropped")
	}
	if ok, _ := tr.admit(vpkt(4, 6000, 0x41), true, 2*time.Second, now); !ok {
		t.Fatal("frame cut in the middle")
	}
	// Frame 3 begins while congested: dropped, and so is every packet of it.
	for seq := uint16(5); seq <= 7; seq++ {
		if ok, _ := tr.admit(vpkt(seq, 9000, 0x41), true, 2*time.Second, now); ok {
			t.Fatalf("packet %d admitted while congested", seq)
		}
	}
	// Caught up, but frame 4 is not a keyframe: still dropped.
	if ok, _ := tr.admit(vpkt(8, 12000, 0x41), false, 0, now); ok {
		t.Fatal("resumed on a non-keyframe")
	}
	// Frame 5 is an IDR: resume, and the episode is reported once.
	ok, ended := tr.admit(vpkt(9, 15000, 0x65), false, 0, now.Add(time.Second))
	if !ok || ended == nil {
		t.Fatalf("resume at IDR: ok=%v ended=%v", ok, ended)
	}
	if ended.frames != 2 || ended.packets != 4 || ended.peak != 2*time.Second || ended.over != time.Second {
		t.Fatalf("episode %+v", *ended)
	}
	if ok, ended := tr.admit(vpkt(10, 15000, 0x65), false, 0, now); !ok || ended != nil {
		t.Fatal("rest of the keyframe")
	}
	// A keyframe while still congested does not resume.
	if ok, _ := tr.admit(vpkt(11, 18000, 0x41), true, time.Second, now); ok {
		t.Fatal("dropping again")
	}
	if ok, _ := tr.admit(vpkt(12, 21000, 0x65), true, time.Second, now); ok {
		t.Fatal("IDR admitted while still congested")
	}
}

func TestGateWithoutKnownCodecResumesAtAnyFrame(t *testing.T) {
	tr := newTrack(&description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{&format.Generic{PayloadTyp: 100, RTPMa: "X-CUSTOM/90000", ClockRat: 90000}}})
	now := time.Now()
	tr.admit(vpkt(1, 0, 0x00), true, 3*time.Second, now)
	if ok, ended := tr.admit(vpkt(2, 3000, 0x00), false, 0, now); !ok || ended == nil {
		t.Fatal("generic codec did not resume at the next frame")
	}
}

func TestStampKeepsOneStreamAcrossGenerations(t *testing.T) {
	tr := videoTrack()
	t0 := time.Now()
	first := tr.stamp(vpkt(100, 90000), t0)
	second := tr.stamp(vpkt(101, 93000), t0.Add(33*time.Millisecond))
	if first.SSRC != 0x1234 || first.SequenceNumber != 100 || first.Timestamp != 90000 {
		t.Fatalf("first generation rewritten: %+v", first.Header)
	}
	if second.SequenceNumber != 101 || second.Timestamp != 93000 {
		t.Fatalf("second packet: %+v", second.Header)
	}
	// The replacement publisher starts from its own SSRC, sequence and
	// timestamp, 500 ms later: the reader sees the same SSRC, the next
	// sequence number, and a timestamp 500 ms on.
	tr.fresh = true
	fresh := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 7, Timestamp: 5_000_000, SSRC: 0xabcd}, Payload: []byte{0x65}}
	third := tr.stamp(fresh, t0.Add(533*time.Millisecond))
	if third.SSRC != 0x1234 || third.SequenceNumber != 102 {
		t.Fatalf("continuity broken: %+v", third.Header)
	}
	if third.Timestamp != 93000+45000 {
		t.Fatalf("timestamp %d, want %d", third.Timestamp, 93000+45000)
	}
	fourth := tr.stamp(&rtp.Packet{Header: rtp.Header{SequenceNumber: 8, Timestamp: 5_003_000, SSRC: 0xabcd}}, t0.Add(566*time.Millisecond))
	if fourth.SequenceNumber != 103 || fourth.Timestamp != third.Timestamp+3000 {
		t.Fatalf("fourth: %+v", fourth.Header)
	}
	if fresh.SSRC != 0xabcd || fresh.SequenceNumber != 7 {
		t.Fatal("caller's packet was modified")
	}
}

func TestStampNeverRepeatsATimestampAtHandover(t *testing.T) {
	tr := videoTrack()
	t0 := time.Now()
	tr.stamp(vpkt(1, 1000), t0)
	tr.fresh = true
	out := tr.stamp(vpkt(1, 1000), t0) // no wall-clock gap at all
	if out.Timestamp != 1001 {
		t.Fatalf("timestamp %d", out.Timestamp)
	}
}

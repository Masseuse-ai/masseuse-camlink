package serve

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// pumpFrom writes IDR video and audio packets every 20 ms, starting from
// its own sequence numbers, timestamps and SSRC the way a fresh ffmpeg does.
func pumpFrom(ctx context.Context, pub *Publication, desc *description.Session, seq0 uint16, ts0, ssrc uint32) error {
	seq := seq0
	ts := ts0
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		for _, m := range desc.Medias {
			pkt := &rtp.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, PayloadType: m.Formats[0].PayloadType(), SequenceNumber: seq, Timestamp: ts, SSRC: ssrc},
				Payload: []byte{0x65, 1, 2, 3},
			}
			if err := pub.WritePacketRTP(m, pkt); err != nil {
				return err
			}
		}
		seq++
		ts += 1800
	}
}

func TestHandoverKeepsTheReaderAndTheStream(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	first := sampleDesc()
	pub, err := s.Publish(first)
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	go pumpFrom(ctx1, pub, first, 1000, 90000, 0x11111111)

	r, err := play(t, s.Dial)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	defer r.c.Close()
	waitFor(t, "first generation", func() bool { return r.video.Load() >= 10 })

	// The encoder is about to restart: the publisher arms the handover and
	// leaves. The publication stays, and so does the reader.
	pub.ArmHandover(3 * time.Second)
	cancel1()
	time.Sleep(50 * time.Millisecond)
	pub.Release()
	if !s.Stats().Publishing {
		t.Fatal("publication closed at release despite the armed handover")
	}
	if s.Readers() != 1 {
		t.Fatalf("readers %d after release", s.Readers())
	}
	seen := r.video.Load()

	// A fresh ffmpeg announces the same medias 400 ms later and joins:
	// Publish returns the same publication.
	time.Sleep(400 * time.Millisecond)
	second := sampleDesc()
	pub2, err := s.Publish(second)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if pub2 != pub {
		t.Fatal("join returned another publication")
	}
	if g := s.Stats().Generations; g != 2 {
		t.Fatalf("generations %d", g)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go pumpFrom(ctx2, pub2, second, 7, 5_000_000, 0x22222222)
	waitFor(t, "second generation", func() bool { return r.video.Load() >= seen+10 })

	// What the reader saw is one stream: one SSRC, sequence numbers without a
	// gap, timestamps that only move forward and jump by about the pause.
	hs := r.videoHeaders()
	for i := 1; i < len(hs); i++ {
		if hs[i].SSRC != hs[0].SSRC {
			t.Fatalf("SSRC changed at packet %d: %x then %x", i, hs[0].SSRC, hs[i].SSRC)
		}
		if hs[i].SequenceNumber != hs[i-1].SequenceNumber+1 {
			t.Fatalf("sequence gap at packet %d: %d then %d", i, hs[i-1].SequenceNumber, hs[i].SequenceNumber)
		}
		if int32(hs[i].Timestamp-hs[i-1].Timestamp) <= 0 {
			t.Fatalf("timestamp went backwards at packet %d: %d then %d", i, hs[i-1].Timestamp, hs[i].Timestamp)
		}
	}
	// (gortsplib gives readers its own SSRC per format; sequence numbers and
	// timestamps pass through.)
	if hs[0].SequenceNumber != 1000 || hs[0].Timestamp != 90000 {
		t.Fatalf("first generation rewritten: %+v", hs[0])
	}
	var jump uint32
	for i := 1; i < len(hs); i++ {
		if d := hs[i].Timestamp - hs[i-1].Timestamp; d > jump {
			jump = d
		}
	}
	// Around 450 ms of 90 kHz clock; generously bounded.
	if jump < 30_000 || jump > 90_000 {
		t.Fatalf("handover timestamp jump %d ticks", jump)
	}
}

func TestHandoverToleratesTheOldPublisherLeavingLate(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pub, err := s.Publish(sampleDesc())
	if err != nil {
		t.Fatal(err)
	}
	pub.ArmHandover(time.Second)
	// The replacement announces before the old ffmpeg's connection is seen
	// to close.
	if _, err := s.Publish(sampleDesc()); err != nil {
		t.Fatal(err)
	}
	pub.Release() // the old one, late
	if !s.Stats().Publishing {
		t.Fatal("the old publisher's late release ended the stream")
	}
	pub.Release() // the new one, for real
	if s.Stats().Publishing {
		t.Fatal("the current publisher's release did not end the stream")
	}
}

func TestHandoverRefusesIncompatibleAndExpires(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pub, err := s.Publish(sampleDesc())
	if err != nil {
		t.Fatal(err)
	}
	pub.ArmHandover(300 * time.Millisecond)
	// Video only, or another codec: not the same stream.
	videoOnly := &description.Session{Medias: sampleDesc().Medias[:1]}
	if _, err := s.Publish(videoOnly); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("video only joined: %v", err)
	}
	h265 := sampleDesc()
	h265.Medias[0].Formats = []format.Format{&format.H265{PayloadTyp: 96}}
	if _, err := s.Publish(h265); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("h265 joined: %v", err)
	}
	// Nobody comes within the grace: the publication ends.
	pub.Release()
	waitFor(t, "publication to close", func() bool { return !s.Stats().Publishing })
	// Without a handover armed a second publisher is still refused.
	pub2, err := s.Publish(sampleDesc())
	if err != nil {
		t.Fatal(err)
	}
	defer pub2.Close()
	if _, err := s.Publish(sampleDesc()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second publisher: %v", err)
	}
	if err := pub2.WritePacketRTP(sampleDesc().Medias[0], &rtp.Packet{}); !errors.Is(err, ErrUnknownMedia) {
		t.Fatalf("foreign media: %v", err)
	}
}

func TestPublicationGateUsesTheBacklog(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir(), Logger: quiet(), GateBacklog: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var backlog atomic.Int64
	s.SetBacklog(func() time.Duration { return time.Duration(backlog.Load()) })

	desc := sampleDesc()
	pub, err := s.Publish(desc)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	r, err := play(t, s.Dial)
	if err != nil {
		t.Fatal(err)
	}
	defer r.c.Close()

	write := func(seq uint16, ts uint32, video bool, payload byte) {
		m := desc.Medias[1]
		pt := uint8(97)
		if video {
			m, pt = desc.Medias[0], 96
		}
		if err := pub.WritePacketRTP(m, &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: 1}, Payload: []byte{payload}}); err != nil {
			t.Fatal(err)
		}
	}
	write(1, 3000, true, 0x65)
	write(1, 960, false, 0x00)
	backlog.Store(int64(2 * time.Second))
	for i := uint16(2); i <= 5; i++ {
		write(i, uint32(i)*3000, true, 0x41)
		write(i, uint32(i)*960, false, 0x00)
	}
	st := s.Stats()
	if !st.Congested || st.VideoFramesDropped != 4 || st.Episodes != 1 || st.VideoPackets != 1 || st.AudioPackets != 5 {
		t.Fatalf("while congested: %+v", st)
	}
	backlog.Store(0)
	write(6, 18000, true, 0x41) // not a keyframe: still dropped
	write(7, 21000, true, 0x65) // keyframe: resumes
	write(8, 24000, true, 0x41)
	st = s.Stats()
	if st.Congested || st.VideoFramesDropped != 5 || st.VideoPackets != 3 || st.Episodes != 1 {
		t.Fatalf("after congestion: %+v", st)
	}
	waitFor(t, "reader packets", func() bool { return r.video.Load() == 3 && r.audio.Load() == 5 })
	hs := r.videoHeaders()
	for i := 1; i < len(hs); i++ {
		if hs[i].SequenceNumber != hs[i-1].SequenceNumber+1 {
			t.Fatalf("dropped frames left a sequence gap: %d then %d", hs[i-1].SequenceNumber, hs[i].SequenceNumber)
		}
	}
	if hs[1].Timestamp != 21000 {
		t.Fatalf("resumed timestamp %d", hs[1].Timestamp)
	}
}

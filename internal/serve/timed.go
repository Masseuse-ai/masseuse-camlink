package serve

import (
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"
)

const (
	// HoldForTime bounds how long a TimedWriter holds a media's first
	// packets while waiting for the source's first sender report on it.
	HoldForTime = time.Second
	// HoldPackets bounds how many packets of a media a TimedWriter holds
	// meanwhile.
	HoldPackets = 256
)

// A TimedWriter writes a source's packets into a Publication timed by the
// source's own RTCP sender reports (Publication.WritePacketRTPWithNTP),
// through `timeOf`: the RTSP session's PacketNTP, which knows a packet's
// time once the source has reported on its media. Until it has, the media's
// packets are held - for at most HoldForTime or HoldPackets - and written
// when the report comes, each timed by it; the readers' own first sender
// report, which follows the first packet they get, then carries the
// source's time rather than the forwarding time. ffmpeg reports before its
// first packet and a gortsplib publisher right after it, so nothing is
// delayed noticeably. A source that has still not reported when the hold
// runs out gets its packets timed at their forwarding, as every packet used
// to be, until it does.
type TimedWriter struct {
	pub    *Publication
	timeOf func(*description.Media, *rtp.Packet) (time.Time, bool)
	hold   time.Duration

	mu     sync.Mutex
	medias map[*description.Media]*mediaHold
}

// mediaHold is one media's hold.
type mediaHold struct {
	settled bool // the hold is over: packets are written as they come
	held    []*rtp.Packet
	since   time.Time
}

// NewTimedWriter returns a TimedWriter into pub; timeOf may be nil, in
// which case every packet is timed at its writing.
func NewTimedWriter(pub *Publication, timeOf func(*description.Media, *rtp.Packet) (time.Time, bool)) *TimedWriter {
	return &TimedWriter{pub: pub, timeOf: timeOf, hold: HoldForTime, medias: map[*description.Media]*mediaHold{}}
}

// Write writes pkt, or holds it while the source's time for its media is
// still unknown. The error is the first of the writes it caused; each
// packet is written whatever happened to the ones before it.
func (w *TimedWriter) Write(medi *description.Media, pkt *rtp.Packet) error {
	var ntp time.Time
	ok := false
	if w.timeOf != nil {
		ntp, ok = w.timeOf(medi, pkt)
	}
	w.mu.Lock()
	h := w.medias[medi]
	if h == nil {
		h = &mediaHold{}
		w.medias[medi] = h
	}
	if h.settled {
		w.mu.Unlock()
		if !ok {
			ntp = time.Time{}
		}
		return w.pub.WritePacketRTPWithNTP(medi, pkt, ntp)
	}
	if ok {
		// The source has reported: its time is known for every timestamp
		// of this media, the held packets' included.
		held := h.settle()
		w.mu.Unlock()
		return w.flush(medi, held, true, pkt, ntp)
	}
	now := time.Now()
	if len(h.held) == 0 {
		h.since = now
	}
	// Held beyond the callback: the packet is the reader's to reuse.
	h.held = append(h.held, pkt.Clone())
	if now.Sub(h.since) < w.hold && len(h.held) < HoldPackets {
		w.mu.Unlock()
		return nil
	}
	// The source is not saying: the packets go timed at their forwarding.
	held := h.settle()
	w.mu.Unlock()
	return w.flush(medi, held, false, nil, time.Time{})
}

// settle ends the hold and takes the held packets. Caller holds w.mu.
func (h *mediaHold) settle() []*rtp.Packet {
	h.settled = true
	held := h.held
	h.held = nil
	return held
}

// flush writes the held packets - timed by the source when `timed` - then
// the current one, if any.
func (w *TimedWriter) flush(medi *description.Media, held []*rtp.Packet, timed bool, pkt *rtp.Packet, ntp time.Time) error {
	var first error
	for _, hp := range held {
		var t time.Time
		if timed {
			if ht, ok := w.timeOf(medi, hp); ok {
				t = ht
			}
		}
		if err := w.pub.WritePacketRTPWithNTP(medi, hp, t); err != nil && first == nil {
			first = err
		}
	}
	if pkt != nil {
		if err := w.pub.WritePacketRTPWithNTP(medi, pkt, ntp); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Held is how many packets are waiting for the source's first report.
func (w *TimedWriter) Held() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, h := range w.medias {
		n += len(h.held)
	}
	return n
}

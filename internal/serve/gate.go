package serve

import (
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// DefaultGateBacklog is the tunnel backlog at which video is dropped.
const DefaultGateBacklog = time.Second

// overflowWindow is how long a reader's write queue overflowing counts as
// congestion on its own.
const overflowWindow = 500 * time.Millisecond

// track is one media of the stream across every generation of publisher:
// the RTP continuity the readers see, and for video the gate that drops
// whole frames while the tunnel is behind.
type track struct {
	media *description.Media // the stream's media (generation 1's)
	video bool
	clock int
	// keyStart says whether a packet begins a keyframe access unit, for the
	// codec; nil when the codec is not known, in which case the gate resumes
	// at any access unit.
	keyStart func(payload []byte) bool

	// Continuity: one SSRC, sequence numbers without gaps (dropped frames
	// leave none), timestamps that keep running across publishers.
	started bool
	ssrc    uint32
	nextSeq uint16
	tsOff   uint32
	lastTS  uint32
	lastAt  time.Time
	fresh   bool // the next packet opens a new generation

	// Access units of the incoming stream (by timestamp change).
	inTS   uint32
	inHave bool

	// Gate state.
	dropping       bool
	droppedFrames  uint64
	droppedPackets uint64
	episodeAt      time.Time
	peak           time.Duration
}

func newTrack(medi *description.Media) *track {
	t := &track{media: medi, video: medi.Type == description.MediaTypeVideo, clock: 90000}
	if len(medi.Formats) > 0 {
		t.clock = medi.Formats[0].ClockRate()
		switch medi.Formats[0].(type) {
		case *format.H264:
			t.keyStart = h264KeyStart
		case *format.H265:
			t.keyStart = h265KeyStart
		}
	}
	return t
}

// beginsAccessUnit reports whether pkt starts a new access unit of the
// incoming stream and remembers its timestamp.
func (t *track) beginsAccessUnit(pkt *rtp.Packet) bool {
	if t.inHave && pkt.Timestamp == t.inTS {
		return false
	}
	t.inTS, t.inHave = pkt.Timestamp, true
	return true
}

// gateEpisode is what the gate reports when an episode of dropping ends.
type gateEpisode struct {
	frames  uint64
	packets uint64
	over    time.Duration
	peak    time.Duration
}

// admit decides whether a video packet goes out. congested says whether the
// tunnel is behind right now and by how much. Decisions are made at access
// unit boundaries: dropping starts at the first frame seen while behind and
// ends at the first keyframe seen once caught up, so what the enclave
// decodes is never a frame whose reference was dropped. ended is non-nil
// when this packet ends an episode.
func (t *track) admit(pkt *rtp.Packet, congested bool, backlog time.Duration, now time.Time) (ok bool, ended *gateEpisode) {
	begins := t.beginsAccessUnit(pkt)
	if begins {
		switch {
		case !t.dropping && congested:
			t.dropping = true
			t.episodeAt = now
			t.peak = backlog
			t.droppedFrames, t.droppedPackets = 0, 0
		case t.dropping && !congested && (t.keyStart == nil || t.keyStart(pkt.Payload)):
			t.dropping = false
			ended = &gateEpisode{frames: t.droppedFrames, packets: t.droppedPackets, over: now.Sub(t.episodeAt), peak: t.peak}
		}
	}
	if !t.dropping {
		return true, ended
	}
	if backlog > t.peak {
		t.peak = backlog
	}
	if begins {
		t.droppedFrames++
	}
	t.droppedPackets++
	return false, nil
}

// stamp returns pkt with this track's continuity applied: the first
// generation's SSRC, the next sequence number, and a timestamp that at a
// generation change continues from where the previous publisher left off,
// advanced by the wall-clock gap. The caller's packet is not modified.
func (t *track) stamp(pkt *rtp.Packet, now time.Time) *rtp.Packet {
	out := *pkt
	if !t.started {
		t.started = true
		t.ssrc = pkt.SSRC
		t.nextSeq = pkt.SequenceNumber
		t.tsOff = 0
		t.fresh = false
	} else if t.fresh {
		t.fresh = false
		gap := now.Sub(t.lastAt)
		if gap < 0 {
			gap = 0
		}
		want := t.lastTS + uint32(gap.Seconds()*float64(t.clock))
		if want == t.lastTS {
			want++ // never the same timestamp twice
		}
		t.tsOff = want - pkt.Timestamp
	}
	out.SSRC = t.ssrc
	out.SequenceNumber = t.nextSeq
	t.nextSeq++
	out.Timestamp = pkt.Timestamp + t.tsOff
	t.lastTS = out.Timestamp
	t.lastAt = now
	return &out
}

// h264KeyStart reports whether an H.264 RTP payload (RFC 6184) begins a
// keyframe: an IDR slice or a parameter set, as a single NAL unit, inside a
// STAP-A, or as the first fragment of an FU-A.
func h264KeyStart(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	switch typ := p[0] & 0x1F; typ {
	case 5, 7, 8:
		return true
	case 24: // STAP-A
		for off := 1; off+2 <= len(p); {
			size := int(p[off])<<8 | int(p[off+1])
			off += 2
			if size < 1 || off+size > len(p) {
				return false
			}
			if t := p[off] & 0x1F; t == 5 || t == 7 || t == 8 {
				return true
			}
			off += size
		}
	case 28: // FU-A
		if len(p) >= 2 && p[1]&0x80 != 0 {
			if t := p[1] & 0x1F; t == 5 || t == 7 || t == 8 {
				return true
			}
		}
	}
	return false
}

// h265KeyStart is h264KeyStart for H.265 (RFC 7798): IRAP slices (types 16
// to 21) or VPS/SPS/PPS (32 to 34), single, aggregated (48) or fragmented
// (49).
func h265KeyStart(p []byte) bool {
	if len(p) < 2 {
		return false
	}
	isKey := func(t byte) bool { return (t >= 16 && t <= 21) || (t >= 32 && t <= 34) }
	switch typ := (p[0] >> 1) & 0x3F; typ {
	case 48: // AP
		for off := 2; off+2 <= len(p); {
			size := int(p[off])<<8 | int(p[off+1])
			off += 2
			if size < 2 || off+size > len(p) {
				return false
			}
			if isKey((p[off] >> 1) & 0x3F) {
				return true
			}
			off += size
		}
	case 49: // FU
		if len(p) >= 3 && p[2]&0x80 != 0 && isKey(p[2]&0x3F) {
			return true
		}
	default:
		return isKey(typ)
	}
	return false
}

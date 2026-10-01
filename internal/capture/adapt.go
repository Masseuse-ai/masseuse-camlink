package capture

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Masseuse-ai/masseuse-camlink/internal/serve"
)

// BitrateFloor is the lowest bit rate the Adapter steps down to, in bits
// per second: where a 2500k ceiling's ladder has always ended. A ceiling
// below it is its own floor.
const BitrateFloor = 600_000

// steps are the first rungs, as fractions of the ceiling (Options.Bitrate);
// past them each rung is stepRatio of the one above, down to BitrateFloor
// (ladder).
var steps = []float64{1, 0.64, 0.40, 0.24}

const stepRatio = 0.6

// maxJump is the most rungs one step down goes (Adapter.below).
const maxJump = 3

// ladder is the bit rates the Adapter moves between under ceiling (bits
// per second), from the ceiling down, in whole kb/s: 2500k gives 2500k,
// 1600k, 1000k and 600k; a camera's larger picture has a higher ceiling
// and a longer ladder to the same floor, 24900k giving 24900k, 15936k,
// 9960k, 5976k, 3586k, 2151k, 1291k, 774k and 600k.
func ladder(ceiling int64) []int64 {
	rates := []int64{ceiling}
	if ceiling <= BitrateFloor {
		return rates
	}
	f := 1.0
	for i := 1; ; i++ {
		if i < len(steps) {
			f = steps[i]
		} else {
			f *= stepRatio
		}
		r := int64(float64(ceiling)*f/1000+0.5) * 1000
		if r <= BitrateFloor {
			return append(rates, BitrateFloor)
		}
		rates = append(rates, r)
	}
}

// Reshaper is what the Adapter drives: a Source.
type Reshaper interface {
	// Reshape sets the video bit rate; see Source.Reshape.
	Reshape(bitrate string) bool
}

// Adapter lowers the video bit rate while the connection cannot keep up
// and raises it again once it can. It reads the stream's gate (serve.Stats:
// whether video is being dropped, how many frames were, how much video got
// through) once a second:
//
//   - down when the gate was dropping for at least StepDownGating of the
//     last Window, or dropped at least StepDownDropFraction of the window's
//     frames: one rung, or as many as it takes to reach the rate that got
//     through while the gate was dropping, up to maxJump; then no further
//     step down for Hold;
//   - up one rung after StepUpAfter without any dropping.
//
// Every change goes through the Reshaper, which restarts the encoder at the
// new rate without the readers noticing, and is announced through Notify.
type Adapter struct {
	// Ceiling is the configured bit rate (ffmpeg syntax); FPS the frame
	// rate, to turn dropped frames into a fraction.
	Ceiling string
	FPS     int
	Stats   func() serve.Stats
	Target  Reshaper
	// Notify gets one line per change, for the console; nil prints nothing.
	Notify func(line string)
	Logger *slog.Logger

	// Tunables; zero means the default noted.
	Interval             time.Duration // 1 s
	Window               time.Duration // 20 s
	StepDownGating       time.Duration // 2 s
	StepDownDropFraction float64       // 0.10
	Hold                 time.Duration // 30 s
	StepUpAfter          time.Duration // 3 min

	rung       int
	samples    []sample
	lastChange time.Time
	cleanSince time.Time
	last       serve.Stats
	haveLast   bool
}

type sample struct {
	congested bool
	dropped   uint64
	sent      uint64 // video bytes that got through
}

func (a *Adapter) defaults() {
	if a.Interval == 0 {
		a.Interval = time.Second
	}
	if a.Window == 0 {
		a.Window = 20 * time.Second
	}
	if a.StepDownGating == 0 {
		a.StepDownGating = 2 * time.Second
	}
	if a.StepDownDropFraction == 0 {
		a.StepDownDropFraction = 0.10
	}
	if a.Hold == 0 {
		a.Hold = 30 * time.Second
	}
	if a.StepUpAfter == 0 {
		a.StepUpAfter = 3 * time.Minute
	}
	if a.FPS == 0 {
		a.FPS = Defaults.FPS
	}
	if a.Ceiling == "" {
		a.Ceiling = BitrateFor(Defaults.VideoSize, Defaults.FPS)
	}
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
}

// rates is the ladder under the ceiling; nil when the ceiling cannot be
// parsed.
func (a *Adapter) rates() []int64 {
	bits, ok := parseRate(a.Ceiling)
	if !ok || bits <= 0 {
		return nil
	}
	return ladder(bits)
}

// Rate is the bit rate of rung i under the ceiling, in ffmpeg syntax
// ("1600k"); "" when there is no such rung or the ceiling cannot be
// parsed.
func (a *Adapter) Rate(i int) string {
	rates := a.rates()
	if i < 0 || i >= len(rates) {
		return ""
	}
	return formatRate(rates[i])
}

// Rungs is how many rungs the ladder under the ceiling has.
func (a *Adapter) Rungs() int { return len(a.rates()) }

// Run evaluates every Interval until ctx ends, starting at the top rung:
// the source is at its ceiling whenever the camera comes on (Source.Stop
// puts it back). A Target that says its options (a Source) gives the
// ceiling and frame rate afresh at every start: a camera chosen again
// brings its own.
func (a *Adapter) Run(ctx context.Context) {
	if src, ok := a.Target.(interface{ Options() Options }); ok {
		o := src.Options()
		a.Ceiling, a.FPS = o.Bitrate, o.FPS
	}
	a.defaults()
	a.reset(time.Now())
	tick := time.NewTicker(a.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			a.Evaluate(now, a.Stats())
		}
	}
}

func (a *Adapter) reset(now time.Time) {
	a.rung = 0
	a.samples = a.samples[:0]
	a.lastChange = time.Time{}
	a.cleanSince = now
	a.haveLast = false
}

// Rung is the current rung (0 is the ceiling).
func (a *Adapter) Rung() int { return a.rung }

// Evaluate takes one sample of the stream and steps the bit rate if due.
// It is what Run calls every Interval; tests call it directly.
func (a *Adapter) Evaluate(now time.Time, st serve.Stats) {
	a.defaults()
	if !st.Publishing || st.Readers == 0 {
		// Nothing is being sent: nothing to learn from, and a new
		// publication starts its counters over.
		a.haveLast = false
		return
	}
	var dropped, sent uint64
	if a.haveLast && st.VideoFramesDropped >= a.last.VideoFramesDropped {
		dropped = st.VideoFramesDropped - a.last.VideoFramesDropped
	} else if !a.haveLast {
		dropped = 0
	} else {
		dropped = st.VideoFramesDropped // counters restarted with a new publication
	}
	if a.haveLast && st.VideoBytes >= a.last.VideoBytes {
		sent = st.VideoBytes - a.last.VideoBytes
	}
	a.last, a.haveLast = st, true
	a.samples = append(a.samples, sample{congested: st.Congested, dropped: dropped, sent: sent})
	if n := int(a.Window / a.Interval); n > 0 && len(a.samples) > n {
		a.samples = a.samples[len(a.samples)-n:]
	}
	if st.Congested || dropped > 0 {
		a.cleanSince = now
	}

	var gating time.Duration
	var droppedInWindow uint64
	for _, s := range a.samples {
		if s.congested {
			gating += a.Interval
		}
		droppedInWindow += s.dropped
	}
	windowFrames := float64(a.FPS) * a.Window.Seconds()
	tooMany := float64(droppedInWindow) >= a.StepDownDropFraction*windowFrames
	held := !a.lastChange.IsZero() && now.Sub(a.lastChange) < a.Hold

	rates := a.rates()
	switch {
	case (gating >= a.StepDownGating || tooMany) && !held && a.rung < len(rates)-1:
		a.rung = a.below(rates)
		a.change(now, "Connection cannot keep up: video now %s", "down", gating, droppedInWindow)
	case a.rung > 0 && now.Sub(a.cleanSince) >= a.StepUpAfter:
		a.rung--
		a.change(now, "Video back to %s", "up", gating, droppedInWindow)
	}
}

// below is the rung a step down goes to: the next one, or further, to the
// first at or below the rate that got through while the gate was dropping
// (got), up to maxJump rungs at once. A camera's larger picture starts high
// above a slow uplink, and one rung per Hold would leave it dropping video
// for minutes.
func (a *Adapter) below(rates []int64) int {
	next := a.rung + 1
	got := a.got()
	for got > 0 && next < len(rates)-1 && next < a.rung+maxJump && rates[next] > got {
		next++
	}
	return next
}

// got is the video bit rate that got through over the window's samples
// while the gate was dropping, the connection's own measure of what it
// carries; 0 when there were fewer than two such samples, or nothing got
// through to measure.
func (a *Adapter) got() int64 {
	var n int
	var bytes uint64
	for _, s := range a.samples {
		if s.congested {
			n++
			bytes += s.sent
		}
	}
	if n < 2 || bytes == 0 {
		return 0
	}
	return int64(float64(bytes) * 8 / (float64(n) * a.Interval.Seconds()))
}

func (a *Adapter) change(now time.Time, format, direction string, gating time.Duration, dropped uint64) {
	rate := a.Rate(a.rung)
	if rate == "" {
		return
	}
	a.lastChange = now
	a.cleanSince = now
	a.samples = a.samples[:0]
	a.Target.Reshape(rate)
	a.Logger.Info("capture: bit rate "+direction, "bitrate", rate, "rung", a.rung, "gating", gating.String(), "dropped_frames", dropped)
	if a.Notify != nil {
		a.Notify(fmt.Sprintf(format, HumanRate(rate)))
	}
}

// parseRate reads an ffmpeg rate ("2500k", "2.5M", "600000") as bits per
// second.
func parseRate(rate string) (int64, bool) {
	m := bitrateRe.FindStringSubmatch(strings.TrimSpace(rate))
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(m[2]) {
	case "k":
		v *= 1e3
	case "m":
		v *= 1e6
	}
	return int64(v + 0.5), true
}

// formatRate writes bits per second in ffmpeg syntax, in k where whole.
func formatRate(bits int64) string {
	if bits%1000 == 0 {
		return strconv.FormatInt(bits/1000, 10) + "k"
	}
	return strconv.FormatInt(bits, 10)
}

// HumanRate writes an ffmpeg rate for a person: "2.5 Mb/s", "600 kb/s".
func HumanRate(rate string) string {
	bits, ok := parseRate(rate)
	if !ok {
		return rate
	}
	switch {
	case bits >= 1_000_000:
		s := strconv.FormatFloat(float64(bits)/1e6, 'f', 1, 64)
		return strings.TrimSuffix(s, ".0") + " Mb/s"
	case bits >= 1000:
		return strconv.FormatInt(bits/1000, 10) + " kb/s"
	default:
		return strconv.FormatInt(bits, 10) + " b/s"
	}
}

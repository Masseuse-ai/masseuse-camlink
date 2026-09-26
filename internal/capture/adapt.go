package capture

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/serve"
)

// Rungs are the bit rates the Adapter moves between, as fractions of the
// configured bit rate (Options.Bitrate, the ceiling): 2500k gives 2500k,
// 1600k, 1000k and 600k.
var Rungs = []float64{1, 0.64, 0.40, 0.24}

// Reshaper is what the Adapter drives: a Source.
type Reshaper interface {
	// Reshape sets the video bit rate; see Source.Reshape.
	Reshape(bitrate string) bool
}

// Adapter lowers the video bit rate while the connection cannot keep up
// and raises it again once it can. It reads the stream's gate (serve.Stats:
// whether video is being dropped, how many frames were) once a second:
//
//   - down one rung when the gate was dropping for at least StepDownGating
//     of the last Window, or dropped at least StepDownDropFraction of the
//     window's frames; then no further step down for Hold;
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
		a.Ceiling = Defaults.Bitrate
	}
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
}

// Rate is the bit rate of rung i under the ceiling, in ffmpeg syntax
// ("1600k"); "" when the ceiling cannot be parsed.
func (a *Adapter) Rate(i int) string {
	bits, ok := parseRate(a.Ceiling)
	if !ok || i < 0 || i >= len(Rungs) {
		return ""
	}
	return formatRate(int64(float64(bits)*Rungs[i] + 0.5))
}

// Run evaluates every Interval until ctx ends, starting at the top rung:
// the source is at its ceiling whenever the camera comes on (Source.Stop
// puts it back).
func (a *Adapter) Run(ctx context.Context) {
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
	var dropped uint64
	if a.haveLast && st.VideoFramesDropped >= a.last.VideoFramesDropped {
		dropped = st.VideoFramesDropped - a.last.VideoFramesDropped
	} else if !a.haveLast {
		dropped = 0
	} else {
		dropped = st.VideoFramesDropped // counters restarted with a new publication
	}
	a.last, a.haveLast = st, true
	a.samples = append(a.samples, sample{congested: st.Congested, dropped: dropped})
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

	switch {
	case (gating >= a.StepDownGating || tooMany) && !held && a.rung < len(Rungs)-1:
		a.rung++
		a.change(now, "Connection cannot keep up: video now %s", "down", gating, droppedInWindow)
	case a.rung > 0 && now.Sub(a.cleanSince) >= a.StepUpAfter:
		a.rung--
		a.change(now, "Video back to %s", "up", gating, droppedInWindow)
	}
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

package capture

import (
	"reflect"
	"testing"
	"time"

	"github.com/Masseuse-ai/masseuse-camlink/internal/serve"
)

type fakeReshaper struct{ rates []string }

func (f *fakeReshaper) Reshape(rate string) bool {
	f.rates = append(f.rates, rate)
	return true
}

func TestAdapterRates(t *testing.T) {
	for _, tc := range []struct {
		ceiling string
		want    []string
	}{
		{"2500k", []string{"2500k", "1600k", "1000k", "600k"}},
		{"2.5M", []string{"2500k", "1600k", "1000k", "600k"}},
		{"4M", []string{"4000k", "2560k", "1600k", "960k", "600k"}},
		// A 4K camera's ceiling goes on down to the same floor.
		{"24900k", []string{"24900k", "15936k", "9960k", "5976k", "3586k", "2151k", "1291k", "774k", "600k"}},
		{"800k", []string{"800k", "600k"}},
		{"500k", []string{"500k"}}, // below the floor: its own
	} {
		a := &Adapter{Ceiling: tc.ceiling}
		var got []string
		for i := range a.Rungs() {
			got = append(got, a.Rate(i))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v", tc.ceiling, got)
		}
	}
	if a := (&Adapter{Ceiling: "fast"}); a.Rate(0) != "" || a.Rungs() != 0 || (&Adapter{Ceiling: "2500k"}).Rate(4) != "" {
		t.Error("bad ceiling or rung")
	}
	for rate, want := range map[string]string{"2500k": "2.5 Mb/s", "1600k": "1.6 Mb/s", "1000k": "1 Mb/s", "600k": "600 kb/s", "999": "999 b/s", "x": "x"} {
		if got := HumanRate(rate); got != want {
			t.Errorf("HumanRate(%s) = %s, want %s", rate, got, want)
		}
	}
}

// drive feeds the adapter one sample per second for n seconds; sending is
// the video bytes per second that get through.
type drive struct {
	a       *Adapter
	now     time.Time
	st      serve.Stats
	lines   []string
	fake    *fakeReshaper
	sending uint64
}

func newDrive() *drive {
	d := &drive{now: time.Unix(1_700_000_000, 0), fake: &fakeReshaper{}}
	d.a = &Adapter{Ceiling: "2500k", FPS: 30, Target: d.fake, Notify: func(l string) { d.lines = append(d.lines, l) }}
	d.a.Logger = quiet()
	d.st = serve.Stats{Publishing: true, Readers: 1}
	return d
}

// step advances n seconds; congested marks every sample so, and dropped
// frames are added at the first.
func (d *drive) step(n int, congested bool, dropped uint64) {
	for i := 0; i < n; i++ {
		d.now = d.now.Add(time.Second)
		d.st.Congested = congested
		d.st.VideoBytes += d.sending
		if i == 0 {
			d.st.VideoFramesDropped += dropped
		}
		d.a.Evaluate(d.now, d.st)
	}
}

func TestAdapterStepsDownOnGatingAndHolds(t *testing.T) {
	d := newDrive()
	d.step(60, false, 0)
	if d.a.Rung() != 0 || len(d.fake.rates) != 0 {
		t.Fatalf("changed on a clean stream: %v", d.fake.rates)
	}
	// One second of gating is not enough; two within the window is.
	d.step(1, true, 5)
	d.step(5, false, 0)
	if d.a.Rung() != 0 {
		t.Fatal("stepped down after one second of gating")
	}
	d.step(1, true, 5)
	if d.a.Rung() != 1 || !reflect.DeepEqual(d.fake.rates, []string{"1600k"}) {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
	if !reflect.DeepEqual(d.lines, []string{"Connection cannot keep up: video now 1.6 Mb/s"}) {
		t.Fatalf("lines %q", d.lines)
	}
	// Still gating: the hold keeps the next step 30 s away.
	d.step(25, true, 30)
	if d.a.Rung() != 1 {
		t.Fatalf("stepped again within the hold: rung %d", d.a.Rung())
	}
	d.step(6, true, 30)
	if d.a.Rung() != 2 || d.fake.rates[len(d.fake.rates)-1] != "1000k" {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
	// Down to the floor, never below it.
	d.step(31, true, 30)
	d.step(31, true, 30)
	d.step(31, true, 30)
	if d.a.Rung() != d.a.Rungs()-1 || d.fake.rates[len(d.fake.rates)-1] != "600k" {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
	if len(d.fake.rates) != 3 {
		t.Fatalf("rates %v", d.fake.rates)
	}
}

// A 4K camera's 24.9 Mb/s on an uplink that carries 4 Mb/s: the first step
// goes three rungs at once toward what got through while the gate was
// dropping, the next lands on the first rung below it, and no further.
func TestAdapterStepsDownToWhatGotThrough(t *testing.T) {
	d := newDrive()
	d.a.Ceiling = "24900k"
	d.sending = 3_100_000 // the ceiling's worth, while it gets through
	d.step(10, false, 0)
	d.sending = 500_000 // 4 Mb/s
	d.step(2, true, 10)
	if d.a.Rung() != 3 || !reflect.DeepEqual(d.fake.rates, []string{"5976k"}) {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
	d.step(31, true, 10)
	if d.a.Rung() != 4 || d.fake.rates[len(d.fake.rates)-1] != "3586k" {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
	// What gets through now fits: no further step.
	d.sending = 448_000
	d.step(120, false, 0)
	if d.a.Rung() != 4 || len(d.fake.rates) != 2 {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
	// Gating with nothing measured getting through steps one rung.
	d.sending = 0
	d.step(2, true, 10)
	if d.a.Rung() != 5 || d.fake.rates[len(d.fake.rates)-1] != "2151k" {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
}

func TestAdapterStepsDownOnDroppedFrames(t *testing.T) {
	d := newDrive()
	d.step(1, false, 0) // the first sample only sets the baseline
	// 10 % of a 20 s window at 30 fps is 60 frames, dropped in one burst with
	// the gate already open again by the time it is sampled.
	d.step(1, false, 59)
	if d.a.Rung() != 0 {
		t.Fatal("stepped down under the threshold")
	}
	d.step(1, false, 1)
	if d.a.Rung() != 1 {
		t.Fatalf("rung %d after 60 dropped frames", d.a.Rung())
	}
}

func TestAdapterStepsUpAfterAClean3Minutes(t *testing.T) {
	d := newDrive()
	d.step(2, true, 10)
	d.step(31, true, 10)
	if d.a.Rung() != 2 {
		t.Fatalf("rung %d", d.a.Rung())
	}
	// Clean, but a blip at 2 minutes restarts the clock.
	d.step(120, false, 0)
	d.step(1, false, 3)
	d.step(170, false, 0)
	if d.a.Rung() != 2 {
		t.Fatalf("stepped up %s after a blip", "less than 3 minutes")
	}
	d.step(11, false, 0)
	if d.a.Rung() != 1 || d.fake.rates[len(d.fake.rates)-1] != "1600k" {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
	if d.lines[len(d.lines)-1] != "Video back to 1.6 Mb/s" {
		t.Fatalf("lines %q", d.lines)
	}
	d.step(181, false, 0)
	if d.a.Rung() != 0 || d.fake.rates[len(d.fake.rates)-1] != "2500k" {
		t.Fatalf("rung %d rates %v", d.a.Rung(), d.fake.rates)
	}
	d.step(600, false, 0)
	if d.a.Rung() != 0 || len(d.fake.rates) != 4 {
		t.Fatalf("kept changing at the ceiling: %v", d.fake.rates)
	}
}

func TestAdapterIgnoresIdleAndCounterRestarts(t *testing.T) {
	d := newDrive()
	d.st.VideoFramesDropped = 500 // a previous publication's tally
	d.step(5, false, 0)
	// A new publication starts its counters over: not a burst of drops.
	d.st.VideoFramesDropped = 0
	d.step(1, false, 2)
	if d.a.Rung() != 0 {
		t.Fatal("a counter restart looked like dropped frames")
	}
	// Nothing sent: samples are not taken.
	d.st.Readers = 0
	d.step(10, true, 100)
	if d.a.Rung() != 0 {
		t.Fatal("stepped down with no reader")
	}
	d.st.Readers = 1
	d.st.Publishing = false
	d.step(10, true, 100)
	if d.a.Rung() != 0 {
		t.Fatal("stepped down while not publishing")
	}
}

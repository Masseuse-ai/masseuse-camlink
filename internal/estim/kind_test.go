package estim_test

import (
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago"
)

func TestKindsAreKnownAndStable(t *testing.T) {
	want := []estim.Kind{"mastago", "estim-2b", "dglabs-coyote", "tens"}
	if len(estim.Kinds) != len(want) {
		t.Fatalf("Kinds = %v, want %v", estim.Kinds, want)
	}
	for i, k := range want {
		if estim.Kinds[i] != k {
			t.Fatalf("Kinds[%d] = %q, want %q", i, estim.Kinds[i], k)
		}
		if !k.Known() {
			t.Fatalf("%q should be known", k)
		}
	}
	for _, k := range []estim.Kind{"", "MASTAGO", "Mastago", "coyote"} {
		if k.Known() {
			t.Fatalf("%q should not be known", k)
		}
	}
}

func TestDriverKindIsKnown(t *testing.T) {
	// The driver this program carries reports a kind from the list the
	// service validates against.
	if k := estim.KindMastago; !k.Known() {
		t.Fatalf("driver kind %q is not in Kinds", k)
	}
	if mastago.LabelPrefix == "" {
		t.Fatal("driver label is empty")
	}
}

func TestDefaultSettingsFor(t *testing.T) {
	if got := estim.DefaultSettingsFor(estim.Capabilities{}); got != estim.DefaultSettings() {
		t.Fatalf("no caps: %+v", got)
	}
	twoRanges := estim.Capabilities{LevelMax: 99, PowerModes: []string{estim.PowerModeNormal, estim.PowerModeHigh}}
	if got := estim.DefaultSettingsFor(twoRanges); got.PowerMode != estim.PowerModeHigh || got.LevelMax != estim.DefaultLevelCap {
		t.Fatalf("two ranges: %+v", got)
	}
	oneRange := estim.Capabilities{LevelMax: 25, LevelMaxDefault: 15}
	if got := estim.DefaultSettingsFor(oneRange); got.PowerMode != estim.PowerModeNormal || got.LevelMax != 15 {
		t.Fatalf("one range: %+v", got)
	}
}

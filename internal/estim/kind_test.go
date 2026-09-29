package estim_test

import (
	"testing"

	"github.com/Masseuse-ai/masseuse-camlink/internal/estim"
	"github.com/Masseuse-ai/masseuse-camlink/internal/estim/mastago"
)

func TestKindsAreKnownAndStable(t *testing.T) {
	want := []estim.Kind{"mastago", "tens"}
	if len(estim.Kinds) != len(want) {
		t.Fatalf("Kinds = %v, want %v", estim.Kinds, want)
	}
	for i, k := range want {
		if estim.Kinds[i] != k {
			t.Fatalf("Kinds[%d] = %q, want %q", i, estim.Kinds[i], k)
		}
		if !estim.Known(k) {
			t.Fatalf("%q should be known", k)
		}
	}
	for _, k := range []estim.Kind{"", "MASTAGO", "Mastago", "other"} {
		if estim.Known(k) {
			t.Fatalf("%q should not be known", k)
		}
	}
}

func TestDriverKindIsKnown(t *testing.T) {
	// The driver this program carries reports a kind from the list the
	// service validates against.
	if k := estim.KindMastago; !estim.Known(k) {
		t.Fatalf("driver kind %q is not in Kinds", k)
	}
	if mastago.LabelPrefix == "" {
		t.Fatal("driver label is empty")
	}
}

func TestIdentityLabels(t *testing.T) {
	// The one-string label composes the three parts; the tag in
	// parentheses only when there is one.
	cases := []struct {
		id   estim.Identity
		want string
	}{
		{estim.Identity{Maker: "Mastogo", Model: "Wireless TENS", Tag: "G-12AB"}, "Mastogo Wireless TENS (G-12AB)"},
		{estim.Identity{Maker: "E-Stim Systems", Model: "2B"}, "E-Stim Systems 2B"},
		{estim.Identity{Model: "2B", Tag: "COM5"}, "2B (COM5)"},
		{estim.Identity{Tag: "4A56"}, "4A56"},
		{estim.Identity{}, ""},
	}
	for _, c := range cases {
		if got := estim.LabelOf(c.id); got != c.want {
			t.Errorf("LabelOf(%+v) = %q, want %q", c.id, got, c.want)
		}
	}
	// Read back out of a label, for a helper that predates the parts: the
	// trailing parenthesis is the tag, the rest the model, no maker.
	from := []struct {
		label string
		want  estim.Identity
	}{
		{"DG-Lab Coyote 3.0 (7C3B)", estim.Identity{Model: "DG-Lab Coyote 3.0", Tag: "7C3B"}},
		{"E-Stim Systems 2B (cu.usbserial-FTCILMWQ)", estim.Identity{Model: "E-Stim Systems 2B", Tag: "cu.usbserial-FTCILMWQ"}},
		{"E-Stim Systems 2B", estim.Identity{Model: "E-Stim Systems 2B"}},
		{"  Some unit  ", estim.Identity{Model: "Some unit"}},
		{"(odd)", estim.Identity{Model: "(odd)"}},
		{"", estim.Identity{}},
	}
	for _, c := range from {
		if got := estim.IdentityFromLabel(c.label); got != c.want {
			t.Errorf("IdentityFromLabel(%q) = %+v, want %+v", c.label, got, c.want)
		}
	}
	// The Mastogo driver names its unit in three parts, and its label is
	// the three composed.
	if id := mastago.IdentityFor("MASTOGO G-12AB"); id != (estim.Identity{Maker: "Mastogo", Model: "Wireless TENS", Tag: "G-12AB"}) || mastago.LabelFor("MASTOGO G-12AB") != estim.LabelOf(id) {
		t.Fatalf("mastago identity %+v, label %q", id, mastago.LabelFor("MASTOGO G-12AB"))
	}
	if mastago.LabelFor("MASTOGO") != "Mastogo Wireless TENS" || mastago.TagFor(" MASTOGO ") != "" {
		t.Fatalf("a unit advertising the prefix alone has no tag: %q", mastago.LabelFor("MASTOGO"))
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

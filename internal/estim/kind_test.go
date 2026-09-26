package estim_test

import (
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312"
)

func TestKindsAreKnownAndStable(t *testing.T) {
	want := []estim.Kind{"mk312bt", "estim-2b", "dglabs-coyote", "tens"}
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
	for _, k := range []estim.Kind{"", "MK312BT", "mk312", "coyote"} {
		if k.Known() {
			t.Fatalf("%q should not be known", k)
		}
	}
}

func TestDriverKindIsKnown(t *testing.T) {
	// The one driver this program carries reports a kind from the list the
	// service validates against.
	if k := estim.KindMK312BT; !k.Known() {
		t.Fatalf("driver kind %q is not in Kinds", k)
	}
	if mk312.Label == "" {
		t.Fatal("driver label is empty")
	}
}

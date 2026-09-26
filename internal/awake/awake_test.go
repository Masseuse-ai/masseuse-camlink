package awake

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

const (
	testName   = "masseuse-camlink test hold"
	testReason = "a test is checking that the computer can be held awake"
)

// TestTakeAndRelease holds the computer awake through the running
// system's own mechanism and lets go. On macOS `pmset -g assertions` must
// list the hold by name while it is held and not after; on Linux without
// logind (a container, a CI runner) Take fails with a reason and the test
// stops there, since that is what the connector then reports.
func TestTakeAndRelease(t *testing.T) {
	h, err := Take(testName, testReason)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skipf("no hold on %s: %v", runtime.GOOS, err)
		}
		if runtime.GOOS == "linux" {
			if !strings.Contains(err.Error(), "awake: ") {
				t.Fatalf("the error does not say what failed: %v", err)
			}
			t.Skipf("no logind here: %v", err)
		}
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		if got := pmsetAssertions(t); !strings.Contains(got, testName) || !strings.Contains(got, "PreventUserIdleSystemSleep") {
			t.Fatalf("pmset does not list the hold:\n%s", got)
		}
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		if got := pmsetAssertions(t); strings.Contains(got, testName) {
			t.Fatalf("pmset still lists the hold after Release:\n%s", got)
		}
	}
	// Releasing again, or releasing nothing, is harmless.
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if err := (*Hold)(nil).Release(); err != nil {
		t.Fatal(err)
	}
}

// TestTwoHolds: two holds live side by side and each is let go on its own.
func TestTwoHolds(t *testing.T) {
	a, err := Take(testName+" A", testReason)
	if err != nil {
		t.Skipf("no hold on %s: %v", runtime.GOOS, err)
	}
	b, err := Take(testName+" B", testReason)
	if err != nil {
		_ = a.Release()
		t.Fatal(err)
	}
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		got := pmsetAssertions(t)
		if strings.Contains(got, testName+" A") || !strings.Contains(got, testName+" B") {
			t.Fatalf("after releasing A, pmset lists:\n%s", got)
		}
	}
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
}

func pmsetAssertions(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("pmset", "-g", "assertions").CombinedOutput()
	if err != nil {
		t.Fatalf("pmset -g assertions: %v\n%s", err, out)
	}
	return string(out)
}

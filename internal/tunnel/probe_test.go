package tunnel

import (
	"net/http"
	"testing"
	"time"
)

func TestBacklogFromProbeState(t *testing.T) {
	tun := &Tunnel{}
	if b := tun.Backlog(); b != 0 {
		t.Fatalf("fresh tunnel backlog %s", b)
	}
	// The last probe took 40 ms and none is outstanding.
	tun.probeRTT = 40 * time.Millisecond
	if b := tun.Backlog(); b != 40*time.Millisecond {
		t.Fatalf("backlog %s, want the last round trip", b)
	}
	// One has been outstanding for two seconds: that is the backlog now.
	tun.probeStart = time.Now().Add(-2 * time.Second)
	if b := tun.Backlog(); b < 2*time.Second || b > 3*time.Second {
		t.Fatalf("backlog %s with a probe outstanding for 2 s", b)
	}
	// A probe the library could not even write counts as five seconds.
	tun.probeStart = time.Time{}
	tun.probeFailed = true
	if b := tun.Backlog(); b != 5*time.Second {
		t.Fatalf("backlog %s after a failed probe", b)
	}
	tun.probeFailed = false
	if b := tun.Backlog(); b != 40*time.Millisecond {
		t.Fatalf("backlog %s once a probe succeeds again", b)
	}
}

func TestStatusErrorRefused(t *testing.T) {
	for status, want := range map[int]bool{401: true, 403: true, 404: false, 500: false, 503: false} {
		e := &StatusError{Host: "slot.example", Status: status}
		if e.Refused() != want {
			t.Errorf("%d refused=%v, want %v", status, e.Refused(), want)
		}
	}
	e := &StatusError{Host: "slot.example", Status: http.StatusUnauthorized}
	if e.Error() != "tunnel: slot.example answered 401" {
		t.Fatalf("message %q", e.Error())
	}
}

func TestSendBufferPerSystem(t *testing.T) {
	if wantSendBuffer("linux") || !wantSendBuffer("darwin") || !wantSendBuffer("windows") {
		t.Fatal("send buffer policy")
	}
}

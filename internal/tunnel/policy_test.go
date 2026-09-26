package tunnel

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []net.IPAddr
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func TestTargetPolicy(t *testing.T) {
	r := fakeResolver{
		"camera.local":   {"192.168.1.108"},
		"nas":            {"10.0.0.5", "fd00::5"},
		"split.example":  {"192.168.1.9", "93.184.216.34"},
		"public.example": {"93.184.216.34"},
	}
	cases := []struct {
		target string
		want   string
		err    string
	}{
		{"192.168.1.108:7441", "192.168.1.108:7441", ""},
		{"10.1.2.3:322", "10.1.2.3:322", ""},
		{"172.16.0.9:554", "172.16.0.9:554", ""},
		{"127.0.0.1:8554", "127.0.0.1:8554", ""},
		{"169.254.7.7:554", "169.254.7.7:554", ""},
		{"[fe80::1]:554", "[fe80::1]:554", ""},
		{"[fd12::9]:554", "[fd12::9]:554", ""},
		{"camera.local:7441", "192.168.1.108:7441", ""},
		{"nas:322", "10.0.0.5:322", ""},
		{"8.8.8.8:53", "", "not a private-network"},
		{"[2001:db8::1]:554", "", "not a private-network"},
		{"public.example:554", "", "outside the private network"},
		{"split.example:554", "", "outside the private network"},
		{"nowhere.example:554", "", "does not resolve"},
		{"192.168.1.108", "", "host:port"},
		{"192.168.1.108:0", "", "port out of range"},
		{"192.168.1.108:70000", "", "port out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			var p targetPolicy
			got, err := p.allow(context.Background(), tc.target, r)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
				if p.locked != "" {
					t.Fatal("refused target was locked in")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("dial %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTargetPolicyLocksToOneTarget(t *testing.T) {
	var p targetPolicy
	if _, err := p.allow(context.Background(), "192.168.1.108:7441", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.allow(context.Background(), "192.168.1.108:7441", nil); err != nil {
		t.Fatalf("same target again: %v", err)
	}
	_, err := p.allow(context.Background(), "192.168.1.109:7441", nil)
	if err == nil || !strings.Contains(err.Error(), "not the session target") {
		t.Fatalf("second target: %v", err)
	}
	_, err = p.allow(context.Background(), "192.168.1.108:554", nil)
	if err == nil {
		t.Fatal("same host, other port accepted")
	}
}

func TestReservedTargetsStandOutsideTheLock(t *testing.T) {
	var p targetPolicy
	// The connector's own endpoint is admitted before, beside and after the
	// one network target, and never takes the lock itself.
	if err := p.allowReserved("127.0.0.1:7443"); err != nil {
		t.Fatal(err)
	}
	if p.locked != "" {
		t.Fatal("a reserved target took the lock")
	}
	if _, err := p.allow(context.Background(), "192.168.1.108:7441", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.allowReserved("127.0.0.1:7443"); err != nil {
		t.Fatalf("reserved beside the network target: %v", err)
	}
	if _, err := p.allow(context.Background(), "192.168.1.109:7441", nil); err == nil {
		t.Fatal("a second network target was admitted")
	}
	// Only loopback literals with a port are reserved-shaped.
	for _, bad := range []string{"192.168.1.108:7443", "localhost:7443", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:70000", "[::1]"} {
		if err := p.allowReserved(bad); err == nil {
			t.Fatalf("%s admitted as reserved", bad)
		}
	}
	if err := p.allowReserved("[::1]:7443"); err != nil {
		t.Fatalf("IPv6 loopback: %v", err)
	}
}

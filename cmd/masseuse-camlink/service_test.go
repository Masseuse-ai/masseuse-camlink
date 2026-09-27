package main

import "testing"

func TestServiceURLAllowed(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://masseuse.ai", true},
		{"https://staging.example.com:8443/", true},
		{"http://127.0.0.1:53211", true},
		{"http://localhost:8080", true},
		{"http://[::1]:8080", true},
		{"http://masseuse.ai", false},
		{"http://192.168.1.20:8080", false},
		{"http://10.0.0.1", false},
		{"http://127.0.0.1.example.com", false},
		{"ftp://127.0.0.1", false},
		{"masseuse.ai", false},
		{"", false},
		{"http://", false},
	} {
		if got := serviceURLAllowed(tc.url); got != tc.ok {
			t.Errorf("%q: allowed %v, want %v", tc.url, got, tc.ok)
		}
	}
}

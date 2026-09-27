package main

import (
	"net"
	"net/url"
	"strings"
)

// serviceURLAllowed says whether -service may be used: an https:// URL
// anywhere, or an http:// URL to this computer alone (a trainer run
// locally, for development and hands-on tests of a unit), since a plain
// http link off the computer would carry the pairing and the unit's link
// in the clear.
func serviceURLAllowed(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

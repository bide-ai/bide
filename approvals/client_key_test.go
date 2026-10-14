package approvals

import (
	"net/http"
	"testing"
)

// IPv6 clients are limited by their /64, the unit an operator assigns to one site or host, so a
// client with a /64 of addresses is one client; IPv4 clients are limited by address, and an IPv4
// address mapped into IPv6 is the same client as the IPv4 one.
func TestRemoteHost_IPv6By64(t *testing.T) {
	key := func(addr string) string { return remoteHost(&http.Request{RemoteAddr: addr}) }
	if a, b := key("[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:5000"), key("[2001:db8:1:2:1::9]:6000"); a != b {
		t.Errorf("two addresses of one /64 are different clients: %q %q", a, b)
	}
	if a, b := key("[2001:db8:1:2::1]:1"), key("[2001:db8:1:3::1]:1"); a == b {
		t.Errorf("addresses of two /64s are one client: %q", a)
	}
	if a, b := key("192.0.2.1:80"), key("192.0.2.2:80"); a == b {
		t.Errorf("two IPv4 addresses are one client: %q", a)
	}
	if key("192.0.2.1:80") != key("[::ffff:192.0.2.1]:80") {
		t.Error("a mapped IPv4 address is a different client from the IPv4 one")
	}
	if got := key("not-an-address"); got != "not-an-address" {
		t.Errorf("unparsable address: %q", got)
	}
}

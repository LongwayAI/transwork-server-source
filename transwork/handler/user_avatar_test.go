package handler

import (
	"strings"
	"testing"
)

// The `picture` claim is only as trustworthy as the upstream IdP, and whatever
// we store is later fetched by the desktop client. These cases pin the shapes
// that must never be persisted, because each one would turn our own response
// into a blind request from the user's machine.
func TestAvatarURLAllowed(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"google cdn", "https://lh3.googleusercontent.com/a/abc=s96-c", true},
		{"other provider host", "https://cdn.logto.example/avatars/1.png", true},

		{"empty", "", false},
		{"not a url", "not-a-url", false},
		{"http downgrade", "http://cdn.example.com/a.png", false},
		{"file scheme", "file:///etc/passwd", false},
		{"no host", "https://", false},
		{"localhost by name", "https://localhost/a.png", false},
		{"loopback v4", "https://127.0.0.1/a.png", false},
		{"loopback v6", "https://[::1]/a.png", false},
		{"private 10/8", "https://10.0.0.5/a.png", false},
		{"private 192.168/16", "https://192.168.1.1/a.png", false},
		{"private 172.16/12", "https://172.16.0.1/a.png", false},
		{"link-local metadata", "https://169.254.169.254/latest/meta-data", false},
		{"unspecified", "https://0.0.0.0/a.png", false},

		// net.ParseIP accepts only canonical dotted-quad, so these all parse as
		// nil and would look like hostnames — while getaddrinfo still resolves
		// every one of them to 127.0.0.1.
		{"shorthand loopback", "https://127.1/a.png", false},
		{"decimal loopback", "https://2130706433/a.png", false},
		{"octal loopback", "https://0177.0.0.1/a.png", false},
		{"hex loopback", "https://0x7f000001/a.png", false},
		{"numeric tld", "https://example.123/a.png", false},
		{"bare single label", "https://intranet/a.png", false},

		// Reserved name spaces that resolvers map to the local machine or LAN.
		// The whole class is refused, not just the bare `localhost` label.
		{"localhost subdomain", "https://foo.localhost/a.png", false},
		{"nested localhost subdomain", "https://a.b.localhost/a.png", false},
		{"localhost trailing root dot", "https://localhost./a.png", false},
		{"uppercase localhost", "https://LOCALHOST/a.png", false},
		{"mdns local", "https://printer.local/a.png", false},
		{"home arpa", "https://router.home.arpa/a.png", false},
		{"private cloud internal", "https://db.internal/a.png", false},
		{"lan suffix", "https://nas.lan/a.png", false},
		{"corp suffix", "https://files.corp/a.png", false},

		// Must not over-reject: these merely contain a reserved word.
		{"host merely containing localhost", "https://localhost.example.com/a.png", true},
		{"host merely containing local", "https://local.example.com/a.png", true},

		// Non-public ranges Go's IsPrivate does not cover.
		{"rfc6598 shared address space", "https://100.64.0.1/a.png", false},
		{"ietf protocol assignments", "https://192.0.0.1/a.png", false},
		{"test-net-1", "https://192.0.2.1/a.png", false},
		{"test-net-2", "https://198.51.100.1/a.png", false},
		{"test-net-3", "https://203.0.113.1/a.png", false},
		{"benchmarking range", "https://198.18.0.1/a.png", false},
		{"reserved future use", "https://240.0.0.1/a.png", false},
		{"multicast", "https://224.0.0.1/a.png", false},
		{"limited broadcast", "https://255.255.255.255/a.png", false},
		{"this network", "https://0.0.0.1/a.png", false},
		{"ipv4-mapped loopback", "https://[::ffff:127.0.0.1]/a.png", false},
		{"ipv4-mapped private", "https://[::ffff:10.0.0.1]/a.png", false},
		{"ipv6 unique local", "https://[fc00::1]/a.png", false},
		{"ipv6 link-local", "https://[fe80::1]/a.png", false},
		{"ipv6 documentation", "https://[2001:db8::1]/a.png", false},

		{"punycode idn tld still allowed", "https://cdn.example.xn--p1ai/a.png", true},
		{"public ip literal allowed", "https://93.184.216.34/a.png", true},
		{"public ipv6 literal allowed", "https://[2606:2800:220:1:248:1893:25c8:1946]/a.png", true},
		{"ipv4-mapped public allowed", "https://[::ffff:93.184.216.34]/a.png", true},
		{"over length", "https://cdn.example.com/" + strings.Repeat("a", maxAvatarURLLen), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := avatarURLAllowed(tc.url); got != tc.want {
				t.Fatalf("avatarURLAllowed(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

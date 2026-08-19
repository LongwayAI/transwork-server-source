package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// clientIPFor spins up an engine configured by setTrustedProxies and reports what
// c.ClientIP() resolves to for a request arriving from remoteAddr carrying xff.
func clientIPFor(t *testing.T, remoteAddr, xff string) string {
	t.Helper()

	headers := map[string]string{}
	if xff != "" {
		headers["X-Forwarded-For"] = xff
	}
	return clientIPForHeaders(t, remoteAddr, headers)
}

// clientIPForHeaders is the general form, for cases that need headers other than XFF.
func clientIPForHeaders(t *testing.T, remoteAddr string, headers map[string]string) string {
	t.Helper()

	gin.SetMode(gin.TestMode)
	server := gin.New()
	if err := setTrustedProxies(server); err != nil {
		t.Fatalf("setTrustedProxies: %v", err)
	}

	var got string
	server.GET("/", func(c *gin.Context) {
		got = c.ClientIP()
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	server.ServeHTTP(httptest.NewRecorder(), req)

	return got
}

// Gin's default RemoteIPHeaders falls back to X-Real-IP when X-Forwarded-For is absent.
// Our nginx overwrites X-Real-IP today, but that config is hand-maintained and lives in
// no repo; if the line were ever dropped, a caller-supplied X-Real-IP arriving via the
// trusted bridge would set ClientIP() and re-open every IP-keyed control. Only
// X-Forwarded-For is consulted, so the header must be ignored regardless.
func TestXRealIPIsIgnoredEvenFromATrustedPeer(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "172.16.0.0/12")

	got := clientIPForHeaders(t, "172.18.0.1:44444", map[string]string{"X-Real-IP": "1.2.3.4"})
	if got != "172.18.0.1" {
		t.Fatalf("X-Real-IP was honoured: ClientIP() = %q, want the peer %q", got, "172.18.0.1")
	}
}

// A forged X-Forwarded-For from a peer that is not a trusted proxy must be ignored.
// This is the regression guard for the bug where SetTrustedProxies was never called:
// Gin then trusted every peer, so any caller could spoof its own IP and walk past
// every rate limiter, the token IP allowlist, and Turnstile.
func TestClientIPIgnoresForgedForwardedForFromUntrustedPeer(t *testing.T) {
	got := clientIPFor(t, "203.0.113.5:54321", "1.2.3.4")
	if got != "203.0.113.5" {
		t.Fatalf("forged X-Forwarded-For was honoured: ClientIP() = %q, want %q", got, "203.0.113.5")
	}
}

// The default must NOT trust arbitrary private peers. The base compose publishes on
// 0.0.0.0 and GCP's default-allow-internal rule exposes every port to 10.128.0.0/9, so a
// default of "all RFC1918" would let any other host in the VPC spoof X-Forwarded-For and
// walk past the rate limiters again. Failing closed here means a deployment that forgets
// TRUSTED_PROXIES gets loud 429s rather than a silent hole.
func TestDefaultDoesNotTrustArbitraryPrivatePeers(t *testing.T) {
	got := clientIPFor(t, "10.128.0.7:44444", "1.2.3.4")
	if got != "10.128.0.7" {
		t.Fatalf("default trusted a private peer: ClientIP() = %q, want %q", got, "10.128.0.7")
	}
}

// The flip side: when the deployment names the bridge CIDR (as the transwork compose
// overlay does), the header the proxy sets must be honoured. Getting this wrong
// collapses every user into a single rate-limit bucket keyed on the bridge gateway.
func TestClientIPTrustsForwardedForFromConfiguredProxy(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "127.0.0.1/32,::1/128,172.16.0.0/12")

	got := clientIPFor(t, "172.18.0.1:44444", "1.2.3.4")
	if got != "1.2.3.4" {
		t.Fatalf("proxy-supplied X-Forwarded-For was dropped: ClientIP() = %q, want %q", got, "1.2.3.4")
	}
}

// Multi-hop: client -> CDN -> host nginx -> container. The immediate peer is still the
// bridge gateway, so the bridge must stay trusted ALONGSIDE the CDN ranges; Gin then
// walks the chain right-to-left and returns the first untrusted entry, i.e. the real
// client. Documented in transwork.env.example — listing only the provider ranges would
// resolve every request to the gateway.
func TestClientIPWalksChainWhenBridgeAndCDNAreBothTrusted(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "172.16.0.0/12,198.51.100.0/24")

	// nginx appends the peer it saw (the CDN egress) to the client-supplied header.
	got := clientIPFor(t, "172.18.0.1:44444", "203.0.113.9, 198.51.100.7")
	if got != "203.0.113.9" {
		t.Fatalf("chain walk returned %q, want the real client %q", got, "203.0.113.9")
	}
}

// The failure mode the doc warns about: trusting only the CDN ranges and dropping the
// bridge means the immediate peer is untrusted, so the header is never consulted and
// every request resolves to the gateway.
func TestDroppingBridgeCollapsesClientIPToGateway(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "198.51.100.0/24")

	got := clientIPFor(t, "172.18.0.1:44444", "203.0.113.9, 198.51.100.7")
	if got != "172.18.0.1" {
		t.Fatalf("expected collapse to the gateway %q, got %q", "172.18.0.1", got)
	}
}

// An explicitly empty TRUSTED_PROXIES trusts nothing and keys off RemoteAddr, which is
// the correct setting when the app is exposed directly with no proxy in front.
func TestEmptyTrustedProxiesIgnoresForwardedFor(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")

	got := clientIPFor(t, "172.18.0.1:44444", "1.2.3.4")
	if got != "172.18.0.1" {
		t.Fatalf("TRUSTED_PROXIES=\"\" still honoured the header: ClientIP() = %q, want %q", got, "172.18.0.1")
	}
}

// Whitespace around entries is tolerated so a multi-line env value does not silently
// produce an invalid CIDR and take the process down at boot.
func TestTrustedProxiesToleratesWhitespace(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", " 172.18.0.0/16 , 10.0.0.0/8 ")

	got := clientIPFor(t, "172.18.0.1:44444", "1.2.3.4")
	if got != "1.2.3.4" {
		t.Fatalf("whitespace-padded CIDR list was mishandled: ClientIP() = %q, want %q", got, "1.2.3.4")
	}
}

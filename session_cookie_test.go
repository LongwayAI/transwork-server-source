package main

import (
	"os"
	"testing"
)

// unsetSessionCookieSecure removes SESSION_COOKIE_SECURE for the duration of the test.
// t.Setenv cannot unset, only blank, so it is called purely to register the restore of
// whatever the caller's environment had — CI or a developer shell may legitimately
// export this variable for a plain-HTTP deployment, and the default-behaviour test must
// not inherit it and fail spuriously.
func unsetSessionCookieSecure(t *testing.T) {
	t.Helper()
	t.Setenv("SESSION_COOKIE_SECURE", "")
	if err := os.Unsetenv("SESSION_COOKIE_SECURE"); err != nil {
		t.Fatalf("unset SESSION_COOKIE_SECURE: %v", err)
	}
}

// mustSessionCookieSecure fails the test on the error path, for the cases where a value
// is expected to parse.
func mustSessionCookieSecure(t *testing.T) bool {
	t.Helper()
	secure, err := sessionCookieSecure()
	if err != nil {
		t.Fatalf("sessionCookieSecure() returned an unexpected error: %v", err)
	}
	return secure
}

// Unset means upstream behaviour. new-api is deployable over plain HTTP, and a Secure
// cookie on such a host breaks login: the browser accepts the Set-Cookie and then never
// sends it back, so password login, OAuth state validation and the passkey begin/finish
// pair all fail to persist a session. Gressio's HTTPS-only assumption lives in the
// compose overlay instead (transwork/docker-compose.transwork.yml), per Rule 4.
func TestSessionCookieSecureDefaultsToUpstreamBehaviour(t *testing.T) {
	unsetSessionCookieSecure(t)

	if mustSessionCookieSecure(t) {
		t.Fatal("session cookie Secure defaulted to true; the application default must " +
			"stay false so upstream's plain-HTTP deployment keeps working")
	}
}

// An explicitly empty value means the same as unset: fall back to the default. That is
// what `SESSION_COOKIE_SECURE=` in transwork.env produces, and the overlay interpolates
// with ${VAR-default} rather than ${VAR:-default} precisely so it stays distinguishable
// from unset rather than being silently forced back to true.
func TestSessionCookieSecureEmptyMeansDefault(t *testing.T) {
	t.Setenv("SESSION_COOKIE_SECURE", "")

	if mustSessionCookieSecure(t) {
		t.Fatal("empty SESSION_COOKIE_SECURE enabled Secure; it must fall back to the default")
	}
}

// The overlay pins "true", and strconv.ParseBool's other spellings must work too — an
// operator who writes 1 or t should not get a cleartext-capable session cookie.
func TestSessionCookieSecureParsesEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"true", true}, // what the compose overlay sets
		{"TRUE", true},
		{"True", true},
		{"1", true},
		{"t", true},
		{" true ", true}, // transwork.env values are hand-edited; tolerate stray spacing
		{"false", false}, // the documented opt-out for a plain-HTTP, non-loopback host
		{"FALSE", false},
		{"0", false},
	} {
		t.Setenv("SESSION_COOKIE_SECURE", tc.env)
		if got := mustSessionCookieSecure(t); got != tc.want {
			t.Errorf("SESSION_COOKIE_SECURE=%q: sessionCookieSecure() = %v, want %v", tc.env, got, tc.want)
		}
	}
}

// A set-but-unparseable value must be an error, not a fallback to the default. On the
// HTTPS-only deployment, silently falling back would hand a non-Secure cookie to an
// operator who typo'd while trying to enable exactly that protection — a security
// regression from a spelling mistake. main() turns this into a startup abort, the same
// way setTrustedProxies treats an invalid CIDR.
func TestSessionCookieSecureRejectsMalformedValue(t *testing.T) {
	for _, env := range []string{"ture", "yes", "no", "on", "off", "2", "true false"} {
		t.Setenv("SESSION_COOKIE_SECURE", env)
		secure, err := sessionCookieSecure()
		if err == nil {
			t.Errorf("SESSION_COOKIE_SECURE=%q was accepted (returned %v); it must be rejected "+
				"so a typo cannot silently disable the Secure attribute", env, secure)
		}
	}
}

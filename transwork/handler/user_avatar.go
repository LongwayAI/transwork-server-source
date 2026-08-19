package handler

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/model"
	twmodel "github.com/QuantumNous/new-api/transwork/model"

	"gorm.io/gorm/clause"
)

// maxAvatarURLLen bounds what we are willing to store, matching the column.
const maxAvatarURLLen = 1024

// reservedHostSuffixes are name spaces that resolvers commonly map to the local
// machine or the local network rather than the public internet: RFC 6761 /
// RFC 8375 special-use names, plus the suffixes ICANN lists as never-to-be
// delegated and the conventional private ones.
//
// This rejects the whole class rather than the single label a reviewer happened
// to name. Each earlier revision of this validator blocked one spelling of
// "reach the local machine" and was bypassed by the next, so the rule is now
// written to close the family.
var reservedHostSuffixes = []string{
	"localhost", // RFC 6761 — always loopback
	"local",     // mDNS / Bonjour link-local names
	"home.arpa", // RFC 8375 — home networks
	"internal",  // conventional private-cloud suffix
	"intranet",
	"private",
	"corp",
	"lan",
}

// nonPublicBlocks is the IANA special-purpose address registry: every range that
// is not globally routable public unicast.
//
// Go's helpers are not enough on their own — IsPrivate covers only RFC1918 and
// RFC4193, so shared address space (RFC 6598), the TEST-NETs, benchmarking and
// reserved space would all read as public. Enumerating the registry closes the
// whole family at once instead of adding one range per review round.
var nonPublicBlocks = func() []*net.IPNet {
	blocks := []string{
		"0.0.0.0/8",          // "this network"
		"10.0.0.0/8",         // RFC1918 private
		"100.64.0.0/10",      // RFC6598 shared address space (CGNAT)
		"127.0.0.0/8",        // loopback
		"169.254.0.0/16",     // link-local (incl. cloud metadata)
		"172.16.0.0/12",      // RFC1918 private
		"192.0.0.0/24",       // IETF protocol assignments
		"192.0.2.0/24",       // TEST-NET-1
		"192.168.0.0/16",     // RFC1918 private
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // TEST-NET-2
		"203.0.113.0/24",     // TEST-NET-3
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved for future use
		"255.255.255.255/32", // limited broadcast
		"::/128",             // unspecified
		"::1/128",            // loopback
		"64:ff9b::/96",       // NAT64
		"100::/64",           // discard-only
		"2001:db8::/32",      // documentation
		"fc00::/7",           // unique local
		"fe80::/10",          // link-local
		"ff00::/8",           // multicast
	}
	out := make([]*net.IPNet, 0, len(blocks))
	for _, b := range blocks {
		if _, n, err := net.ParseCIDR(b); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// ipIsNonPublic reports whether ip falls outside globally routable public space.
// IPv4-mapped IPv6 forms are normalised first, so ::ffff:127.0.0.1 is judged as
// 127.0.0.1 rather than sneaking through as an unmatched v6 address.
func ipIsNonPublic(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range nonPublicBlocks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostIsReserved reports whether host is, or sits under, a reserved name space.
// host must already be lower-cased with any FQDN root dot removed.
func hostIsReserved(host string) bool {
	for _, s := range reservedHostSuffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

// avatarURLAllowed reports whether an IdP-supplied `picture` claim is safe to
// persist. We hand this value to the desktop client, which fetches it, so a
// claim naming a loopback or private-network host would turn our response into
// a blind request originating from the user's machine. The claim is only as
// trustworthy as the upstream provider — on a federated or self-service IdP it
// can be user-editable — so it is validated here rather than trusted.
//
// Limitation: a public hostname that *resolves* to a private address still
// passes, since that can only be caught at connect time. This rejects the
// literal cases, which is what a claim can actually carry.
func avatarURLAllowed(raw string) bool {
	if raw == "" || len(raw) > maxAvatarURLLen {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	// Normalised once: lower-cased, and the optional FQDN root dot removed so
	// "localhost." cannot slip past a suffix comparison.
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || hostIsReserved(host) {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ipIsNonPublic(ip)
	}
	// Not a canonical IP literal, so it must be a real DNS name. Anything that
	// merely looks numeric is refused — see hostIsDNSName.
	return hostIsDNSName(host)
}

// hostIsDNSName reports whether host is a DNS name rather than some alternative
// encoding of a numeric address.
//
// This exists because net.ParseIP accepts only canonical dotted-quad form, so
// shorthand and alternate-radix literals — "127.1", "2130706433", "0177.0.0.1",
// "0x7f000001" — parse as nil and would otherwise be treated as hostnames, even
// though getaddrinfo resolves every one of them to 127.0.0.1.
//
// Requiring an alphabetic top-level label rejects all of them at once: a numeric
// literal can never end in one. Punycode TLDs are allowed explicitly so
// internationalised domains still pass.
func hostIsDNSName(host string) bool {
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false // bare single label; also covers the all-decimal IPv4 form
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c == '-' || (c >= '0' && c <= '9') ||
				(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	if strings.HasPrefix(strings.ToLower(tld), "xn--") {
		return true
	}
	if len(tld) < 2 {
		return false
	}
	for i := 0; i < len(tld); i++ {
		c := tld[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

// upsertUserAvatar records the avatar URL the IdP reported at sign-in,
// overwriting any previous value so a changed upstream picture propagates on the
// next login.
//
// An empty url is a no-op rather than a write: a provider that omits the claim
// on one sign-in must never blank an avatar captured earlier.
func upsertUserAvatar(userId int, avatarUrl string) error {
	if model.DB == nil || userId == 0 || avatarUrl == "" {
		return nil
	}
	if !avatarURLAllowed(avatarUrl) {
		// Surfaced as an error so the caller logs it; the caller treats avatar
		// persistence as best-effort, so a rejected claim never blocks sign-in.
		return fmt.Errorf("rejected avatar url from IdP (scheme/host not allowed)")
	}
	row := twmodel.UserAvatar{UserId: userId, AvatarUrl: avatarUrl}
	return model.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"avatar_url", "updated_at"}),
	}).Create(&row).Error
}

// getUserAvatar returns the stored avatar URL, or "" when none is recorded.
// Absence is the normal case for accounts created before this table existed, or
// whose IdP does not forward a picture claim, so a miss is not an error.
func getUserAvatar(userId int) string {
	if model.DB == nil || userId == 0 {
		return ""
	}
	var row twmodel.UserAvatar
	if err := model.DB.Where("user_id = ?", userId).First(&row).Error; err != nil {
		return ""
	}
	return row.AvatarUrl
}

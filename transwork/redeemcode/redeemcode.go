// Package redeemcode generates the human-readable redemption/invite codes used
// by Gressio.
//
// Upstream new-api generates redemption keys as 32-char UUIDs (see
// controller.AddRedemption). Gressio prefers short codes that are easy to read
// and type by hand, so the generation logic lives here in the overlay and the
// upstream handler only calls Generate() (Rule 4: upstream-first overlay).
package redeemcode

import (
	"crypto/rand"
	"math/big"
)

// charset is uppercase letters + digits with the visually ambiguous characters
// (O, I, 0, 1) removed, so codes are easy to read and type by hand.
const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// codeLength is the length of a generated redemption code. With a 32-symbol
// alphabet this yields ~1.1e12 combinations, so collisions against the unique
// index on the key column are astronomically unlikely.
const codeLength = 8

// Generate returns a cryptographically secure random redemption code.
//
// crypto/rand (not math/rand) is required because redemption codes are bearer
// tokens for account credit: a predictable generator would let an attacker
// guess unissued codes. Uniqueness is enforced by the redemptions.key unique
// index at insert time; no pre-check query is needed given the huge state space.
func Generate() (string, error) {
	b := make([]byte, codeLength)
	max := big.NewInt(int64(len(charset)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = charset[n.Int64()]
	}
	return string(b), nil
}

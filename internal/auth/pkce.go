package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// randomString is n random bytes, base64url without padding.
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on the platforms this CLI ships for; a
		// predictable verifier would be worse than stopping.
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkce returns an RFC 7636 verifier (64 characters) and its S256 challenge.
// S256 is the only method the server accepts.
func pkce() (verifier, challenge string) {
	verifier = randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

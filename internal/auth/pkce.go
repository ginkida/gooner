package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// generateCodeVerifier generates a cryptographically random code verifier for PKCE.
// The verifier is a 32-byte random string, base64url encoded (43 characters).
func generateCodeVerifier() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Fallback to a simpler approach if crypto/rand fails
		panic("failed to generate random bytes for PKCE verifier: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// generateCodeChallenge generates a code challenge from the code verifier.
// Uses SHA256 hash and base64url encoding as per RFC 7636.
func generateCodeChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// generateState generates a random state parameter for CSRF protection.
func generateState() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("failed to generate random bytes for state: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// PKCE holds a Proof Key for Code Exchange verifier/challenge pair for the
// OAuth 2.0 authorization-code flow (RFC 7636). The verifier is a
// high-entropy random secret held by the client; the challenge is its
// SHA-256 digest, sent in the authorization request. At token exchange the
// client presents the verifier so the server can prove it matches.
type PKCE struct {
	// Verifier is the secret held by the client and replayed at token
	// exchange. Never send it in the authorization request.
	Verifier string
	// Challenge is the base64url-encoded SHA-256 of the verifier, sent as
	// code_challenge with code_challenge_method=S256.
	Challenge string
}

// GeneratePKCE creates a new verifier/challenge pair using a 96-byte random
// verifier and the S256 challenge method. Both values are base64url encoded
// without padding, matching the reference OAuth flows.
func GeneratePKCE() (PKCE, error) {
	verifierBytes := make([]byte, 96)
	if _, err := rand.Read(verifierBytes); err != nil {
		return PKCE{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCE{Verifier: verifier, Challenge: challenge}, nil
}

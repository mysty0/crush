package oauth

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeneratePKCE(t *testing.T) {
	t.Parallel()

	p, err := GeneratePKCE()
	require.NoError(t, err)
	require.NotEmpty(t, p.Verifier)
	require.NotEmpty(t, p.Challenge)

	// Verifier decodes as base64url without padding and carries the full
	// 96 bytes of entropy.
	raw, err := base64.RawURLEncoding.DecodeString(p.Verifier)
	require.NoError(t, err)
	require.Len(t, raw, 96)

	// The challenge is a base64url-encoded SHA-256 digest (32 bytes).
	sum, err := base64.RawURLEncoding.DecodeString(p.Challenge)
	require.NoError(t, err)
	require.Len(t, sum, 32)
}

func TestGeneratePKCEUnique(t *testing.T) {
	t.Parallel()

	a, err := GeneratePKCE()
	require.NoError(t, err)
	b, err := GeneratePKCE()
	require.NoError(t, err)
	require.NotEqual(t, a.Verifier, b.Verifier)
	require.NotEqual(t, a.Challenge, b.Challenge)
}

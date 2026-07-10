package codex

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeJWT builds an unsigned JWT (header.payload.signature) whose payload
// is the base64url-encoded JSON of the given claims. The signature is a
// dummy value since decoding never verifies it.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadJSON, err := json.Marshal(claims)
	require.NoError(t, err)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	return header + "." + payload + ".sig"
}

func TestDecodeAccountID(t *testing.T) {
	t.Parallel()

	token := makeJWT(t, map[string]any{
		accountClaim: map[string]any{
			"chatgpt_account_id": "acct-123",
		},
	})
	require.Equal(t, "acct-123", DecodeAccountID(token))
}

func TestDecodeAccountID_Missing(t *testing.T) {
	t.Parallel()

	token := makeJWT(t, map[string]any{"foo": "bar"})
	require.Equal(t, "", DecodeAccountID(token))
}

func TestDecodeAccountID_Malformed(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", DecodeAccountID("not-a-jwt"))
	require.Equal(t, "", DecodeAccountID(""))
	require.Equal(t, "", DecodeAccountID("a.!!!.c"))
}

func TestDecodeEmail(t *testing.T) {
	t.Parallel()

	token := makeJWT(t, map[string]any{
		profileClaim: map[string]any{
			"email": "user@example.com",
		},
	})
	require.Equal(t, "user@example.com", DecodeEmail(token))
}

func TestDecodeEmail_TopLevelFallback(t *testing.T) {
	t.Parallel()

	token := makeJWT(t, map[string]any{"email": "top@example.com"})
	require.Equal(t, "top@example.com", DecodeEmail(token))
}

func TestDecodeEmail_Missing(t *testing.T) {
	t.Parallel()

	token := makeJWT(t, map[string]any{"foo": "bar"})
	require.Equal(t, "", DecodeEmail(token))
}

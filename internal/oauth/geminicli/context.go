package geminicli

import "context"

// userPromptIDContextKey is the context key carrying the per-turn user
// prompt id. It is an unexported struct type so no other package can
// collide with it, mirroring the pattern used by internal/agent/tools.
type userPromptIDContextKey struct{}

// WithUserPromptID attaches a user prompt id to ctx so every Cloud Code
// Assist request made while handling one logical agent turn reports the
// same value in its request envelope's userPromptId field.
//
// Why this matters: a single user prompt usually fans out into many
// generateContent calls, one per tool-calling round trip. The Cloud Code
// Assist backend groups requests for quota accounting by userPromptId, so
// when the field is absent every round trip looks like a fresh top-level
// prompt and an ordinary agent turn is billed as N prompts instead of one.
// That is the suspected cause of near-immediate rate limiting. The id must
// therefore be generated once at the start of a turn and remain stable for
// all of that turn's requests — never regenerated per request.
func WithUserPromptID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userPromptIDContextKey{}, id)
}

// UserPromptIDFromContext returns the user prompt id attached to ctx, or
// "" when the request was not made inside a tracked agent turn. Callers
// must omit the envelope field entirely on an empty value rather than
// sending a blank string.
func UserPromptIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(userPromptIDContextKey{}).(string)
	return id
}

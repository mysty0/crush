package geminicli

import "charm.land/catwalk/pkg/catwalk"

// DefaultModels returns the Gemini models available through the Cloud Code
// Assist (Gemini CLI) subscription.
//
// Cloud Code Assist does not expose a clean public model-list endpoint, so
// this curated static list mirrors the models shipped by the reference
// implementation.
func DefaultModels() []catwalk.Model {
	return []catwalk.Model{
		{
			ID:               "gemini-2.5-pro",
			Name:             "Gemini 2.5 Pro",
			ContextWindow:    1048576,
			DefaultMaxTokens: 65536,
			CanReason:        true,
			SupportsImages:   true,
		},
		{
			ID:               "gemini-2.5-flash",
			Name:             "Gemini 2.5 Flash",
			ContextWindow:    1048576,
			DefaultMaxTokens: 65536,
			CanReason:        true,
			SupportsImages:   true,
		},
		{
			ID:               "gemini-2.0-flash",
			Name:             "Gemini 2.0 Flash",
			ContextWindow:    1048576,
			DefaultMaxTokens: 8192,
			CanReason:        true,
			SupportsImages:   true,
		},
		{
			ID:               "gemini-2.5-flash-lite",
			Name:             "Gemini 2.5 Flash-Lite",
			ContextWindow:    1048576,
			DefaultMaxTokens: 65536,
			CanReason:        true,
			SupportsImages:   true,
		},
	}
}

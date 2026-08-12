package config

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/claudecode"
)

// ResolveInlineModels resolves the configured inline model patterns into
// concrete models to name in the tool descriptions that accept a "model"
// parameter. It returns the resolved models plus human-readable warnings
// for patterns that matched nothing, so the caller can surface them
// rather than silently advertising a shorter list than the user asked
// for.
//
// The configured large and small models are always included: they are the
// defaults those tools fall back to, so the model must be able to name
// them. Patterns are resolved after those slots and de-duplicated
// against them.
//
// # Ordering contract
//
// Each pattern resolves to the FIRST matching model in provider list
// order, which is what lets "*opus*" keep tracking the current flagship
// as new models ship. That depends on provider model lists being curated
// newest-first -- true for the catwalk provider definitions and for the
// OAuth subscription providers seeded in seedOAuthProviders. If an
// upstream list is ever reordered oldest-first, patterns silently start
// resolving to an older model; nothing here can detect that.
//
// Providers themselves are visited in sorted ID order. The underlying
// provider map has no inherent order, and an unsorted walk would let an
// unscoped pattern resolve to a different provider between runs --
// rewriting the tool descriptions, which sit in the cached prompt
// prefix, and invalidating the cache for the whole request.
func (c *Config) ResolveInlineModels() ([]SelectedModel, []string) {
	var (
		resolved []SelectedModel
		warnings []string
	)

	seen := make(map[string]struct{})
	add := func(m SelectedModel) {
		if m.Model == "" || m.Provider == "" {
			return
		}
		key := m.Provider + "/" + m.Model
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		resolved = append(resolved, m)
	}

	// The configured slots are the defaults these tools fall back to,
	// so they are always advertised.
	for _, slot := range []SelectedModelType{SelectedModelTypeLarge, SelectedModelTypeSmall} {
		if m, ok := c.Models[slot]; ok {
			add(m)
		}
	}

	if c.Options == nil || c.Options.AgentModels == nil {
		return resolved, nil
	}

	providers := slices.SortedFunc(slices.Values(c.EnabledProviders()), func(a, b ProviderConfig) int {
		return strings.Compare(a.ID, b.ID)
	})

	for _, ref := range c.Options.AgentModels.Inline {
		if ref.Match == "" {
			continue
		}
		if m, ok := matchInlineModel(providers, ref); ok {
			add(m)
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"agent_models: pattern %q matched no models%s; the agent, Workflow, and agentic_fetch tools will not advertise it (other models remain reachable via the list_models tool)",
			ref.Match, sourceSuffix(ref.Source),
		))
	}

	return resolved, warnings
}

// matchInlineModel returns the first model matching ref, scanning
// providers in the given order and each provider's models in list order.
func matchInlineModel(providers []ProviderConfig, ref InlineModelRef) (SelectedModel, bool) {
	for _, providerCfg := range providers {
		if !providerMatchesSource(providerCfg.ID, ref.Source) {
			continue
		}
		for _, m := range providerCfg.Models {
			ok, err := path.Match(ref.Match, m.ID)
			if err != nil || !ok {
				continue
			}
			return SelectedModel{
				Provider:        providerCfg.ID,
				Model:           m.ID,
				MaxTokens:       m.DefaultMaxTokens,
				ReasoningEffort: m.DefaultReasoningEffort,
			}, true
		}
	}
	return SelectedModel{}, false
}

// providerMatchesSource reports whether a provider satisfies a ref's
// source restriction. An empty source matches everything. A "claude-code"
// source matches every Claude Code subscription account (claude-code,
// claude-code-work, ...), not just the base provider, so scoping to the
// subscription keeps working once a second account is added.
func providerMatchesSource(providerID, source string) bool {
	switch source {
	case "":
		return true
	case claudecode.ProviderID:
		return claudecode.IsProviderID(providerID)
	default:
		return providerID == source
	}
}

func sourceSuffix(source string) string {
	if source == "" {
		return ""
	}
	return fmt.Sprintf(" in provider %q", source)
}

package tools

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/stretchr/testify/require"
)

func skillCtx(sessionID string) context.Context {
	return context.WithValue(context.Background(), SessionIDContextKey, sessionID)
}

func runSkillTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, name string) fantasy.ToolResponse {
	t.Helper()
	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"name":"` + name + `"}`})
	require.NoError(t, err)
	return resp
}

func TestSkillTool_ActivatesAndReturnsInstructions(t *testing.T) {
	t.Parallel()

	store := skills.NewLoadedStore()
	active := []*skills.Skill{
		{Name: "caveman", Instructions: "Talk terse."},
	}
	tool := NewSkillTool(active, store)

	resp := runSkillTool(t, tool, skillCtx("s1"), "caveman")
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Talk terse.")
	require.Contains(t, resp.Content, "now active")

	// The skill is recorded so its instructions persist across turns.
	require.Equal(t, []string{"caveman"}, store.Names("s1"))
}

func TestSkillTool_UnknownSkill(t *testing.T) {
	t.Parallel()

	store := skills.NewLoadedStore()
	tool := NewSkillTool([]*skills.Skill{{Name: "jq", Instructions: "x"}}, store)

	resp := runSkillTool(t, tool, skillCtx("s1"), "nope")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Unknown skill")
	require.Empty(t, store.Names("s1"))
}

func TestSkillTool_RejectsModelInvocationDisabled(t *testing.T) {
	t.Parallel()

	store := skills.NewLoadedStore()
	active := []*skills.Skill{
		{Name: "secret", Instructions: "x", DisableModelInvocation: true},
	}
	tool := NewSkillTool(active, store)

	resp := runSkillTool(t, tool, skillCtx("s1"), "secret")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "user-invocable only")
	require.Empty(t, store.Names("s1"))
}

func TestSkillTool_RequiresSession(t *testing.T) {
	t.Parallel()

	store := skills.NewLoadedStore()
	tool := NewSkillTool([]*skills.Skill{{Name: "caveman", Instructions: "x"}}, store)

	_, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"name":"caveman"}`})
	require.Error(t, err)
}

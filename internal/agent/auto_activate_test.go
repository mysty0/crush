package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/stretchr/testify/require"
)

// newAutoActivateCoordinator builds the minimal coordinator state that
// autoActivateSkills touches.
func newAutoActivateCoordinator() *coordinator {
	return &coordinator{
		activeSkills: []*skills.Skill{
			{Name: "caveman", Instructions: "terse", AutoActivate: true},
			{Name: "jq", Instructions: "jq stuff"},
		},
		loadedSkills:  skills.NewLoadedStore(),
		autoActivated: csync.NewMap[string, struct{}](),
	}
}

func TestAutoActivateSkills(t *testing.T) {
	t.Parallel()

	t.Run("seeds only auto-activating skills", func(t *testing.T) {
		t.Parallel()
		c := newAutoActivateCoordinator()
		c.autoActivateSkills("s1")
		require.Equal(t, []string{"caveman"}, c.loadedSkills.Names("s1"))
	})

	t.Run("deactivation is not undone on later turns", func(t *testing.T) {
		t.Parallel()
		c := newAutoActivateCoordinator()
		c.autoActivateSkills("s1")
		c.deactivateSkillsFromPrompt("s1", "stop caveman")
		require.Empty(t, c.loadedSkills.Names("s1"))

		c.autoActivateSkills("s1")
		require.Empty(t, c.loadedSkills.Names("s1"), "seeding must happen once per session")
	})

	t.Run("each session is seeded independently", func(t *testing.T) {
		t.Parallel()
		c := newAutoActivateCoordinator()
		c.autoActivateSkills("s1")
		c.deactivateSkillsFromPrompt("s1", "normal mode")
		c.autoActivateSkills("s2")
		require.Equal(t, []string{"caveman"}, c.loadedSkills.Names("s2"))
	})

	t.Run("disabled skills never reach the session", func(t *testing.T) {
		t.Parallel()
		c := newAutoActivateCoordinator()
		// options.disabled_skills filters upstream, leaving activeSkills empty.
		c.activeSkills = nil
		c.autoActivateSkills("s1")
		require.Empty(t, c.loadedSkills.Names("s1"))
	})
}

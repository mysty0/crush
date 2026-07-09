package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/skills"
)

const SkillToolName = "skill"

const skillToolDescription = `Activate a skill for the current conversation.

Skills are specialized instruction sets or workflows listed in
<available_skills>. When a skill's description matches the user's request, call
this tool with the skill's exact name to activate it. The skill's full
instructions are returned and stay in effect for the rest of the conversation
until the user asks to stop it (e.g. "stop <name>", "normal mode").

Prefer this over reading the skill file directly. Do not call it for a skill
that is already active, and do not call it for skills the user has not asked for
unless their request clearly matches the skill's description.`

type SkillParams struct {
	Name string `json:"name" description:"The exact name of the skill to activate, as listed in <available_skills>"`
}

// NewSkillTool returns a tool that activates a named skill for the current
// session: it records the skill in the loaded store (so its instructions
// persist across turns) and returns the instructions so the model applies
// them immediately. The call renders in the transcript, giving the user a
// visible signal that the skill was activated.
func NewSkillTool(activeSkills []*skills.Skill, loadedSkills *skills.LoadedStore) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SkillToolName,
		skillToolDescription,
		func(ctx context.Context, params SkillParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			name := strings.TrimSpace(params.Name)
			if name == "" {
				return fantasy.NewTextErrorResponse("name is required"), nil
			}

			var found *skills.Skill
			for _, s := range activeSkills {
				if s.Name == name {
					found = s
					break
				}
			}
			if found == nil {
				return fantasy.NewTextErrorResponse(
					fmt.Sprintf("Unknown skill %q. Available skills: %s", name, modelInvocableNames(activeSkills)),
				), nil
			}
			if found.DisableModelInvocation {
				return fantasy.NewTextErrorResponse(
					fmt.Sprintf("Skill %q cannot be activated by the model; it is user-invocable only.", name),
				), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required to activate a skill")
			}
			loadedSkills.Add(sessionID, found.Name, found.Instructions)

			var sb strings.Builder
			fmt.Fprintf(&sb,
				"Skill %q is now active for this conversation. Follow its instructions on every response until the user asks to stop it (e.g. \"stop %s\", \"normal mode\").\n\n",
				found.Name, found.Name)
			sb.WriteString(found.Instructions)
			return fantasy.NewTextResponse(sb.String()), nil
		},
	)
}

// modelInvocableNames returns the sorted, comma-separated names of skills
// the model is allowed to activate, for error messages.
func modelInvocableNames(active []*skills.Skill) string {
	names := make([]string, 0, len(active))
	for _, s := range active {
		if s.DisableModelInvocation {
			continue
		}
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

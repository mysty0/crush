package model

import (
	"fmt"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// backgroundTimerDuration is how long the Ctrl+B gesture stays armed
// after the first press before it disarms itself. It mirrors
// cancelTimerDuration so both double-tap gestures feel identical.
const backgroundTimerDuration = 2 * time.Second

// backgroundTimerCmd expires the Ctrl+B arm window.
func backgroundTimerCmd() tea.Cmd {
	return tea.Tick(backgroundTimerDuration, func(time.Time) tea.Msg {
		return backgroundTimerExpiredMsg{}
	})
}

// backgroundNow handles the Ctrl+B key press while the agent is busy. Like
// cancelAgent, it is a two-press gesture: the first press arms the gesture
// and starts a timer; the second press (before the timer expires) actually
// detaches every currently-blocking operation for the session -- foreground
// bash waits and synchronous sub-agent turns -- so they keep running in the
// background and the turn is freed. The double tap disambiguates an
// intentional background from Ctrl+B's ordinary editor binding.
func (m *UI) backgroundNow() tea.Cmd {
	if !m.hasSession() {
		return nil
	}
	if !m.com.Workspace.AgentIsReady() {
		return nil
	}

	if m.backgroundArmed {
		// Second press — actually background whatever is blocking.
		m.backgroundArmed = false
		fired := m.com.Workspace.AgentBackgroundNow(m.session.ID)
		return util.ReportInfo(backgroundSummary(fired))
	}

	// First press — arm the gesture and start the disarm timer.
	m.backgroundArmed = true
	return backgroundTimerCmd()
}

// backgroundSummary renders the breakdown returned by AgentBackgroundNow
// into a human message, e.g. "Backgrounded 1 bash command, 2 sub-agents."
// A nil/empty map means nothing was blocking.
func backgroundSummary(fired map[tools.BackgroundKind]int) string {
	if len(fired) == 0 {
		return "Nothing to background."
	}
	kinds := make([]tools.BackgroundKind, 0, len(fired))
	for k := range fired {
		kinds = append(kinds, k)
	}
	// Stable order so the summary doesn't shuffle between presses.
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })

	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		n := fired[k]
		label := string(k)
		if n != 1 {
			label += "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, label))
	}

	out := "Backgrounded " + parts[0]
	for i := 1; i < len(parts); i++ {
		out += ", " + parts[i]
	}
	return out + "."
}

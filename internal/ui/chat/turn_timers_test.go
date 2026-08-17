package chat

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestAssistantLoaderNamesBothTimers guards the readability of the
// assistant loader, which shows two durations side by side: the label's
// stopwatch for the current step and a suffix for the whole turn. Two
// bare numbers next to each other read as a contradiction (the step
// timer restarts on every model round trip while the turn total keeps
// climbing), so the total must say what it is.
//
// This test cannot run in parallel: the turn timer it drives is process
// wide (see internal/ui/common/timer.go).
func TestAssistantLoaderNamesBothTimers(t *testing.T) {
	sty := styles.CharmtonePantera()
	item := NewAssistantMessageItem(&sty, &message.Message{
		ID:   "m-timers",
		Role: message.Assistant,
	}).(*AssistantMessageItem)

	common.StartTurn()
	t.Cleanup(common.StopTurn)

	// The label fades in over the anim's birth animation; advance past it
	// so the rendered text is settled, as it would be a moment after the
	// spinner appears.
	for range 25 {
		item.Advance()
	}

	// Backdate the step stopwatch past its threshold so both timers are
	// on screen at once -- the case that was ambiguous.
	item.spinStartedAt = time.Now().Add(-12 * time.Second)
	item.clearCache()
	rendered := ansi.Strip(item.Render(80))

	require.Regexp(t, `Working · \d+s`, rendered,
		"the step timer must keep naming the activity it is timing")
	require.Contains(t, rendered, "total",
		"the turn total must be named, not rendered as a second bare duration")
}

// TestTurnTotalIsUnlabeledWhenIdle verifies the suffix stays empty
// between turns, so an idle loader does not render a dangling "total"
// with nothing after it.
func TestTurnTotalIsUnlabeledWhenIdle(t *testing.T) {
	common.StopTurn()

	sty := styles.CharmtonePantera()
	item := NewAssistantMessageItem(&sty, &message.Message{
		ID:   "m-idle",
		Role: message.Assistant,
	}).(*AssistantMessageItem)

	for range 25 {
		item.Advance()
	}
	rendered := ansi.Strip(item.Render(80))

	require.NotContains(t, rendered, "total",
		"no turn is active, so there is no total to show")
}

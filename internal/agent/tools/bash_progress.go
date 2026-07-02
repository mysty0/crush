package tools

import (
	"context"

	"github.com/charmbracelet/crush/internal/pubsub"
)

// BashProgressEvent carries incremental output from a running foreground
// bash command so the UI can stream it live before the tool returns.
type BashProgressEvent struct {
	SessionID  string
	ToolCallID string
	// Output is the full accumulated output so far (not just the latest
	// chunk), so a subscriber that missed earlier lossy updates still
	// renders the correct current state.
	Output string
}

// bashProgressBroker fans out foreground bash progress events to
// subscribers (e.g. the TUI). It is a package-level broker mirroring the
// pattern used by the MCP client package.
var bashProgressBroker = pubsub.NewBroker[BashProgressEvent]()

// SubscribeBashProgress returns a channel that receives incremental output
// events from running foreground bash commands.
func SubscribeBashProgress(ctx context.Context) <-chan pubsub.Event[BashProgressEvent] {
	return bashProgressBroker.Subscribe(ctx)
}

// PublishBashProgress emits an incremental output event. Delivery is
// lossy by design: dropped intermediate updates are harmless because each
// event carries the full accumulated output.
func PublishBashProgress(sessionID, toolCallID, output string) {
	bashProgressBroker.Publish(pubsub.UpdatedEvent, BashProgressEvent{
		SessionID:  sessionID,
		ToolCallID: toolCallID,
		Output:     output,
	})
}

package agent

import (
	"context"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
)

// RetryProgressEvent reports that a provider request for a session is being
// retried after a transient failure (rate limit or 5xx). The UI renders it as
// a live "retrying N/M" indicator on the in-flight assistant message. A Done
// event clears the indicator (the request resumed, or the turn ended).
type RetryProgressEvent struct {
	SessionID string
	// MessageID is the in-flight assistant message so the UI can target the
	// item that should show the indicator.
	MessageID string
	// Attempt is the 1-based retry number and MaxRetries the cap, rendered
	// together as "retrying Attempt/MaxRetries".
	Attempt    int
	MaxRetries int
	// RetryAt is when the next attempt fires, used to render a live countdown.
	RetryAt time.Time
	// Reason is a short human-readable cause, e.g. "Rate limited".
	Reason string
	// Done clears the indicator; set when the request resumes or the turn ends.
	Done bool
}

// retryProgressBroker fans out provider retry events to subscribers (e.g. the
// TUI). It mirrors the package-level broker pattern used for bash and workflow
// progress.
var retryProgressBroker = pubsub.NewBroker[RetryProgressEvent]()

// SubscribeRetryProgress returns a channel that receives provider retry
// progress events.
func SubscribeRetryProgress(ctx context.Context) <-chan pubsub.Event[RetryProgressEvent] {
	return retryProgressBroker.Subscribe(ctx)
}

// PublishRetryProgress emits a retry progress event. Delivery is lossy by
// design: each event carries the full current state, so a dropped intermediate
// update is harmless.
func PublishRetryProgress(e RetryProgressEvent) {
	retryProgressBroker.Publish(pubsub.UpdatedEvent, e)
}

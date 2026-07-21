package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// blockingTool blocks in Run until its release channel is closed, ignoring
// context cancellation entirely — modeling a tool wedged in a syscall it
// cannot be interrupted from. started is closed once Run is entered so a test
// can synchronize before canceling.
type blockingTool struct {
	name     string
	started  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func newBlockingTool(name string) *blockingTool {
	return &blockingTool{
		name:     name,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
}

func (b *blockingTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: b.name} }

func (b *blockingTool) Run(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	close(b.started)
	<-b.release // Deliberately ignores ctx cancellation.
	close(b.returned)
	return fantasy.NewTextResponse("done"), nil
}

func (b *blockingTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (b *blockingTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

func TestCancelEnforcingTool_ReturnsOnCancel(t *testing.T) {
	t.Parallel()

	inner := newBlockingTool("wedged")
	tool := &cancelEnforcingTool{inner: inner}

	ctx, cancel := context.WithCancel(t.Context())

	type result struct {
		resp fantasy.ToolResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: "wedged"})
		done <- result{resp, err}
	}()

	// Wait until the wedged tool is actually running, then cancel.
	select {
	case <-inner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("inner tool never started")
	}
	cancel()

	// The wrapper must return promptly with the context error even though
	// the inner tool is still blocked.
	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelEnforcingTool did not return on cancellation")
	}

	// The inner tool is still blocked (abandoned, not unblocked by cancel).
	select {
	case <-inner.returned:
		t.Fatal("inner tool should still be blocked after cancellation")
	default:
	}

	// Releasing it later must not panic on the buffered channel.
	close(inner.release)
	select {
	case <-inner.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("inner tool never returned after release")
	}
}

func TestCancelEnforcingTool_PassesThroughResult(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "view", resp: fantasy.NewTextResponse("hello")}
	tool := &cancelEnforcingTool{inner: inner}

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-1", Name: "view"})
	require.NoError(t, err)
	require.True(t, inner.called)
	require.Equal(t, "hello", resp.Content)
}

func TestCancelEnforcingTool_PassesThroughError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	inner := &errTool{name: "view", err: wantErr}
	tool := &cancelEnforcingTool{inner: inner}

	_, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-1", Name: "view"})
	require.ErrorIs(t, err, wantErr)
}

// errTool returns a fixed error from Run.
type errTool struct {
	name string
	err  error
}

func (e *errTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: e.name} }
func (e *errTool) Run(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, e.err
}
func (e *errTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (e *errTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

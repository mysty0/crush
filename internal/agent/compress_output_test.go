package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// assistantTextMessage builds a minimal assistant-role fantasy.Message
// with a single text part.
func assistantTextMessage(text string) fantasy.Message {
	return fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}
}

// textToolResultMessage builds a tool-role fantasy.Message with a single
// text tool-result part, for use in compressPriorToolResults tests.
func textToolResultMessage(toolCallID, text string) fantasy.Message {
	return fantasy.Message{
		Role: fantasy.MessageRoleTool,
		Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{
				ToolCallID: toolCallID,
				Output:     fantasy.ToolResultOutputContentText{Text: text},
			},
		},
	}
}

func toolResultText(t *testing.T, msg fantasy.Message) string {
	t.Helper()
	require.Len(t, msg.Content, 1)
	part, ok := msg.Content[0].(fantasy.ToolResultPart)
	require.True(t, ok)
	out, ok := part.Output.(fantasy.ToolResultOutputContentText)
	require.True(t, ok)
	return out.Text
}

func longText(n int) string {
	s := make([]byte, n)
	for i := range s {
		s[i] = 'a'
	}
	return string(s)
}

// TestCompressPriorToolResults_SkipsMostRecentStep verifies the core
// PrepareStep integration rule: only tool-result messages from steps
// before the current one are ever compressed, never the freshest one.
func TestCompressPriorToolResults_SkipsMostRecentStep(t *testing.T) {
	t.Parallel()

	big := longText(compressToolResultThresholdBytes + 100)
	messages := []fantasy.Message{
		fantasy.NewUserMessage("do the thing"),
		textToolResultMessage("call-1", big),
		assistantTextMessage("ok, next"),
		textToolResultMessage("call-2", big), // most recent tool message
	}

	var calls int
	compress := func(ctx context.Context, sessionID, content string) (string, bool) {
		calls++
		return "COMPRESSED:" + content, true
	}

	compressPriorToolResults(t.Context(), "s1", messages, compress)

	require.Equal(t, 1, calls, "only the prior-step tool result should be compressed")
	require.Contains(t, toolResultText(t, messages[1]), "COMPRESSED:")
	require.Equal(t, big, toolResultText(t, messages[3]), "most recent tool result must stay untouched")
}

// TestCompressPriorToolResults_BelowThresholdUntouched verifies small tool
// outputs are never sent to the compressor.
func TestCompressPriorToolResults_BelowThresholdUntouched(t *testing.T) {
	t.Parallel()

	small := "a small tool result"
	messages := []fantasy.Message{
		textToolResultMessage("call-1", small),
		textToolResultMessage("call-2", longText(compressToolResultThresholdBytes+1)), // most recent
	}

	var calls int
	compress := func(ctx context.Context, sessionID, content string) (string, bool) {
		calls++
		return "COMPRESSED", true
	}

	compressPriorToolResults(t.Context(), "s1", messages, compress)

	require.Equal(t, 0, calls)
	require.Equal(t, small, toolResultText(t, messages[0]))
}

// TestCompressPriorToolResults_UnavailableLeavesContentUntouched covers the
// "daemon unavailable / compression disabled" path: compress returning
// ok=false must never alter the message.
func TestCompressPriorToolResults_UnavailableLeavesContentUntouched(t *testing.T) {
	t.Parallel()

	big := longText(compressToolResultThresholdBytes + 1)
	messages := []fantasy.Message{
		textToolResultMessage("call-1", big),
		textToolResultMessage("call-2", big), // most recent
	}

	compress := func(ctx context.Context, sessionID, content string) (string, bool) {
		return "", false
	}

	compressPriorToolResults(t.Context(), "s1", messages, compress)

	require.Equal(t, big, toolResultText(t, messages[0]))
}

// TestCompressPriorToolResults_SkipsAlreadyCompressed verifies a message
// already compressed on an earlier step's pass isn't compressed again.
func TestCompressPriorToolResults_SkipsAlreadyCompressed(t *testing.T) {
	t.Parallel()

	already := compressedOutputMarker + " (kept ~50%)]\n\n" + longText(compressToolResultThresholdBytes)
	messages := []fantasy.Message{
		textToolResultMessage("call-1", already),
		textToolResultMessage("call-2", longText(compressToolResultThresholdBytes+1)), // most recent
	}

	var calls int
	compress := func(ctx context.Context, sessionID, content string) (string, bool) {
		calls++
		return "COMPRESSED", true
	}

	compressPriorToolResults(t.Context(), "s1", messages, compress)

	require.Equal(t, 0, calls)
	require.Equal(t, already, toolResultText(t, messages[0]))
}

// TestCompressPriorToolResults_NoToolMessages is a no-op-safety check: a
// message slice with no tool-result parts at all must not panic.
func TestCompressPriorToolResults_NoToolMessages(t *testing.T) {
	t.Parallel()

	messages := []fantasy.Message{
		fantasy.NewUserMessage("hello"),
		assistantTextMessage("hi"),
	}
	compress := func(ctx context.Context, sessionID, content string) (string, bool) {
		t.Fatal("compress should not be called")
		return "", false
	}
	compressPriorToolResults(t.Context(), "s1", messages, compress)
}

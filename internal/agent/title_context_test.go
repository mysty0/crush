package agent

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func userMsg(text string) message.Message {
	return message.Message{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}}
}

func assistantMsg(text string) message.Message {
	return message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: text}}}
}

func TestConversationText_FlattensUserAndAssistant(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{
		userMsg("fix the login bug"),
		assistantMsg("looking into it"),
		userMsg("it's on mobile"),
	}
	got := conversationText(msgs)
	require.Equal(t, "fix the login bug\nlooking into it\nit's on mobile", got)
}

func TestConversationText_SkipsToolSystemAndEmpty(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{
		{Role: message.System, Parts: []message.ContentPart{message.TextContent{Text: "system prompt"}}},
		userMsg("hello"),
		{Role: message.Tool, Parts: []message.ContentPart{message.TextContent{Text: "tool output"}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: ""}}},
		assistantMsg("hi there"),
	}
	got := conversationText(msgs)
	require.Equal(t, "hello\nhi there", got)
}

func TestConversationText_TailSlicesLongConversations(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", titleContextMaxChars)
	msgs := []message.Message{
		userMsg("OLD-START" + long),
		assistantMsg("RECENT-END"),
	}
	got := conversationText(msgs)
	require.LessOrEqual(t, len(got), titleContextMaxChars)
	// The most recent content must survive the tail slice.
	require.True(t, strings.HasSuffix(got, "RECENT-END"))
	// The oldest content is dropped.
	require.False(t, strings.Contains(got, "OLD-START"))
}

func TestConversationText_EmptyWhenNoContent(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{
		{Role: message.Tool, Parts: []message.ContentPart{message.TextContent{Text: "tool only"}}},
	}
	require.Empty(t, conversationText(msgs))
}

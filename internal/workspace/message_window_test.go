package workspace

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// newWindowTestWorkspace builds an AppWorkspace backed by a real SQLite
// database (no debounce, so writes are immediately observable) with just
// the Sessions and Messages services ListMessagesWindow /
// ListOlderMessages touch.
func newWindowTestWorkspace(t *testing.T) (*AppWorkspace, session.Service, message.Service) {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q, conn, message.WithDebounce(0))

	return NewAppWorkspace(&app.App{Sessions: sessions, Messages: messages}, nil), sessions, messages
}

// addMsg creates a message with an explicit, strictly increasing
// created_at (via Copy, which preserves the timestamp given rather than
// stamping "now"). created_at has only second-level precision in this
// schema, so a tight test loop using Create would collide within the
// same wall-clock second and make ordering/pagination assertions flaky;
// explicit timestamps make the tests deterministic.
func addMsg(t *testing.T, messages message.Service, sessionID string, createdAt int64, text string) message.Message {
	t.Helper()
	m, err := messages.Copy(context.Background(), sessionID, message.Message{
		Role:      message.User,
		Parts:     []message.ContentPart{message.TextContent{Text: text}},
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	})
	require.NoError(t, err)
	return m
}

// TestListMessagesWindowSmallSessionLoadsEverything pins the common
// case: a session shorter than the window returns all its messages with
// hasMore=false, so ordinary-sized sessions behave exactly as ListMessages
// did before windowing existed.
func TestListMessagesWindowSmallSessionLoadsEverything(t *testing.T) {
	ws, sessions, messages := newWindowTestWorkspace(t)
	ctx := context.Background()

	sess, err := sessions.Create(ctx, "s")
	require.NoError(t, err)
	addMsg(t, messages, sess.ID, 1, "one")
	addMsg(t, messages, sess.ID, 2, "two")
	addMsg(t, messages, sess.ID, 3, "three")

	msgs, hasMore, err := ws.ListMessagesWindow(ctx, sess.ID, 10)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Len(t, msgs, 3)
	require.Equal(t, []string{"one", "two", "three"}, texts(msgs))
}

// TestListMessagesWindowLargeSessionTruncatesAndReportsMore pins the
// actual fix: a session larger than the window returns only the most
// recent `limit` messages, in chronological order, with hasMore=true so
// the caller knows to page in the rest instead of assuming it saw
// everything.
func TestListMessagesWindowLargeSessionTruncatesAndReportsMore(t *testing.T) {
	ws, sessions, messages := newWindowTestWorkspace(t)
	ctx := context.Background()

	sess, err := sessions.Create(ctx, "s")
	require.NoError(t, err)
	for i := range 10 {
		addMsg(t, messages, sess.ID, int64(i+1), string(rune('a'+i)))
	}

	msgs, hasMore, err := ws.ListMessagesWindow(ctx, sess.ID, 4)
	require.NoError(t, err)
	require.True(t, hasMore)
	require.Equal(t, []string{"g", "h", "i", "j"}, texts(msgs),
		"the window must hold the most recent messages in chronological order")
}

// TestListMessagesWindowAlwaysIncludesSinceSummary pins the compaction
// behavior the UI needs: even when the recent-message window would cut a
// session off before its last compaction point, the window is extended
// to include everything from SummaryMessageID onward -- the same tail an
// agent turn would still send to the model. Losing part of that tail
// from the initial load would show a shorter (and misleading) active
// context than the agent actually has.
func TestListMessagesWindowAlwaysIncludesSinceSummary(t *testing.T) {
	ws, sessions, messages := newWindowTestWorkspace(t)
	ctx := context.Background()

	sess, err := sessions.Create(ctx, "s")
	require.NoError(t, err)
	var ts int64
	for i := range 5 {
		ts++
		addMsg(t, messages, sess.ID, ts, "pre-"+string(rune('a'+i)))
	}
	ts++
	summary := addMsg(t, messages, sess.ID, ts, "summary")
	for i := range 5 {
		ts++
		addMsg(t, messages, sess.ID, ts, "post-"+string(rune('a'+i)))
	}

	sess.SummaryMessageID = summary.ID
	_, err = sessions.Save(ctx, sess)
	require.NoError(t, err)

	// A window smaller than the post-summary tail must still cover the
	// whole tail (6 messages: the summary itself plus 5 after it), not
	// just the trailing `limit` of them.
	msgs, hasMore, err := ws.ListMessagesWindow(ctx, sess.ID, 3)
	require.NoError(t, err)
	require.True(t, hasMore, "the pre-summary history is still there and unloaded")
	require.Len(t, msgs, 6)
	require.Equal(t, "summary", msgs[0].Content().Text)
}

// TestListOlderMessagesPagesInChronologicalOrder verifies the paging
// query used to lazily load history above the initial window: it
// returns the page immediately before the cursor, oldest first, and an
// empty result once nothing older is left -- the signal the UI uses to
// stop trying to load more.
func TestListOlderMessagesPagesInChronologicalOrder(t *testing.T) {
	ws, sessions, messages := newWindowTestWorkspace(t)
	ctx := context.Background()

	sess, err := sessions.Create(ctx, "s")
	require.NoError(t, err)
	for i := range 6 {
		addMsg(t, messages, sess.ID, int64(i+1), string(rune('a'+i)))
	}

	window, hasMore, err := ws.ListMessagesWindow(ctx, sess.ID, 2)
	require.NoError(t, err)
	require.True(t, hasMore)
	require.Equal(t, []string{"e", "f"}, texts(window))

	older, err := ws.ListOlderMessages(ctx, sess.ID, window[0].CreatedAt, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"c", "d"}, texts(older))

	older, err = ws.ListOlderMessages(ctx, sess.ID, older[0].CreatedAt, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, texts(older))

	older, err = ws.ListOlderMessages(ctx, sess.ID, older[0].CreatedAt, 2)
	require.NoError(t, err)
	require.Empty(t, older, "nothing left before the first message")
}

func texts(msgs []message.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Content().Text
	}
	return out
}

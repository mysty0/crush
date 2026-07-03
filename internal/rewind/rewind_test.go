package rewind

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	rewind   Service
	sessions session.Service
	messages message.Service
	history  history.Service
	ctx      context.Context
}

func newTestEnv(t *testing.T) testEnv {
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
	// Disable debounce so message writes are synchronous and observable.
	messages := message.NewService(q, message.WithDebounce(0))
	hist := history.NewService(q, conn)

	return testEnv{
		rewind:   NewService(sessions, messages, hist),
		sessions: sessions,
		messages: messages,
		history:  hist,
		ctx:      t.Context(),
	}
}

// addUser creates a user message with the given text and returns it.
func (e testEnv) addUser(t *testing.T, sessionID, text string) message.Message {
	t.Helper()
	m, err := e.messages.Create(e.ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	return m
}

// addAssistant creates an assistant message with the given text.
func (e testEnv) addAssistant(t *testing.T, sessionID, text string) message.Message {
	t.Helper()
	m, err := e.messages.Create(e.ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	return m
}

func TestListRewindPoints(t *testing.T) {
	e := newTestEnv(t)

	sess, err := e.sessions.Create(e.ctx, "s")
	require.NoError(t, err)

	e.addUser(t, sess.ID, "first")
	e.addAssistant(t, sess.ID, "reply one")
	e.addUser(t, sess.ID, "second")
	e.addAssistant(t, sess.ID, "reply two")

	points, err := e.rewind.ListRewindPoints(e.ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, points, 2, "only user messages are rewind points")

	// Newest first (ListUserMessages orders by created_at DESC).
	previews := []string{points[0].Preview, points[1].Preview}
	require.Contains(t, previews, "first")
	require.Contains(t, previews, "second")
}

func TestRewindConversationForksWithoutTouchingOrigin(t *testing.T) {
	e := newTestEnv(t)

	sess, err := e.sessions.Create(e.ctx, "Origin")
	require.NoError(t, err)

	e.addUser(t, sess.ID, "first turn")
	e.addAssistant(t, sess.ID, "first reply")
	target := e.addUser(t, sess.ID, "keep me")
	e.addAssistant(t, sess.ID, "dropped reply")
	e.addUser(t, sess.ID, "drop me")

	res, err := e.rewind.Rewind(e.ctx, sess.ID, target.ID, ModeConversation)
	require.NoError(t, err)
	require.NotEmpty(t, res.ForkedSessionID)
	require.Zero(t, res.FilesRestored)

	// Origin is untouched: still has all 5 messages.
	originMsgs, err := e.messages.List(e.ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, originMsgs, 5)

	// Fork keeps everything BEFORE the target user message; the target
	// and everything after it are dropped. The target's text is returned
	// so the UI can put it back in the prompt for editing/resending.
	forkMsgs, err := e.messages.List(e.ctx, res.ForkedSessionID)
	require.NoError(t, err)
	require.Len(t, forkMsgs, 2)
	require.Equal(t, "first turn", forkMsgs[0].Content().Text)
	require.Equal(t, "first reply", forkMsgs[1].Content().Text)
	require.Equal(t, "keep me", res.PrefillText,
		"the rewound message text must be returned for the prompt")

	// Fork records its provenance.
	fork, err := e.sessions.Get(e.ctx, res.ForkedSessionID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, fork.ForkedFromSessionID)
	require.Equal(t, target.ID, fork.ForkedAtMessageID)
	require.Equal(t, "Origin (rewind)", fork.Title)
}

// TestRewindForkPreservesOrderAndTimestamps guards against a regression
// where the fork copied messages with fresh created_at timestamps. Since
// messages are ordered by created_at (second resolution), copying many
// messages within the same second collapsed them to one or two timestamp
// values and scrambled the fork's order — the UI then showed a random
// message as the latest output. The fork must preserve each source
// message's created_at so ordering is identical to the origin.
func TestRewindForkPreservesOrderAndTimestamps(t *testing.T) {
	e := newTestEnv(t)

	sess, err := e.sessions.Create(e.ctx, "Origin")
	require.NoError(t, err)

	// Seed the origin with messages carrying distinct, fixed timestamps
	// in the past. This mirrors a real conversation whose messages span
	// specific seconds, and makes the assertion below catch a fork that
	// stamps fresh "now" timestamps (which would collapse ordering).
	const n = 30
	const base = int64(1_700_000_000)
	const targetIdx = 20
	var target message.Message
	for i := 0; i < n; i++ {
		src := message.Message{
			Role:      message.User,
			Parts:     []message.ContentPart{message.TextContent{Text: "msg " + strconv.Itoa(i)}},
			CreatedAt: base + int64(i),
			UpdatedAt: base + int64(i),
		}
		m, err := e.messages.Copy(e.ctx, sess.ID, src)
		require.NoError(t, err)
		if i == targetIdx {
			target = m
		}
	}

	res, err := e.rewind.Rewind(e.ctx, sess.ID, target.ID, ModeConversation)
	require.NoError(t, err)

	forkMsgs, err := e.messages.List(e.ctx, res.ForkedSessionID)
	require.NoError(t, err)
	// The fork contains everything before the target (indices 0..19);
	// the target and everything after it are dropped.
	require.Len(t, forkMsgs, targetIdx)
	require.Equal(t, "msg "+strconv.Itoa(targetIdx), res.PrefillText,
		"the rewound message text must be returned for the prompt")

	for i := range forkMsgs {
		require.Equal(t, "msg "+strconv.Itoa(i), forkMsgs[i].Content().Text,
			"fork message %d out of order", i)
		require.Equal(t, base+int64(i), forkMsgs[i].CreatedAt,
			"fork message %d must preserve created_at", i)
	}
}

func TestRewindCodeRestoresFilesOnDisk(t *testing.T) {
	e := newTestEnv(t)

	sess, err := e.sessions.Create(e.ctx, "s")
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	// Message 1 captures the file at "v1".
	m1 := e.addUser(t, sess.ID, "edit one")
	_, err = e.history.Create(e.ctx, sess.ID, m1.ID, path, "v1")
	require.NoError(t, err)

	// Later the file becomes "v2" (a newer version tied to a later msg).
	m2 := e.addUser(t, sess.ID, "edit two")
	_, err = e.history.CreateVersion(e.ctx, sess.ID, m2.ID, path, "v2")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o644))

	// Rewind code to m1: the file on disk must go back to "v1".
	res, err := e.rewind.Rewind(e.ctx, sess.ID, m1.ID, ModeCode)
	require.NoError(t, err)
	require.Empty(t, res.ForkedSessionID, "code mode does not fork")
	require.Equal(t, 1, res.FilesRestored)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "v1", string(got))
}

func TestRewindBothForksAndRestores(t *testing.T) {
	e := newTestEnv(t)

	sess, err := e.sessions.Create(e.ctx, "s")
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "f.txt")
	m1 := e.addUser(t, sess.ID, "one")
	_, err = e.history.Create(e.ctx, sess.ID, m1.ID, path, "old")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	m2 := e.addUser(t, sess.ID, "two")
	_, err = e.history.CreateVersion(e.ctx, sess.ID, m2.ID, path, "new")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("new"), 0o644))

	res, err := e.rewind.Rewind(e.ctx, sess.ID, m1.ID, ModeBoth)
	require.NoError(t, err)
	require.NotEmpty(t, res.ForkedSessionID)
	require.Equal(t, 1, res.FilesRestored)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "old", string(got))
}

func TestRewindRejectsForeignMessage(t *testing.T) {
	e := newTestEnv(t)

	a, err := e.sessions.Create(e.ctx, "a")
	require.NoError(t, err)
	b, err := e.sessions.Create(e.ctx, "b")
	require.NoError(t, err)

	msgInB := e.addUser(t, b.ID, "in b")

	_, err = e.rewind.Rewind(e.ctx, a.ID, msgInB.ID, ModeConversation)
	require.Error(t, err)
}

-- name: GetMessage :one
SELECT *
FROM messages
WHERE id = ? LIMIT 1;

-- name: ListMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ?
ORDER BY created_at ASC;

-- name: ListMessagesBySessionRecent :many
-- Returns the most recent `limit` messages for a session, newest first.
-- Callers that want chronological order (as ListMessagesBySession
-- returns) must reverse the result. Used for the initial windowed load
-- of a session so opening a long-running, heavily-compacted session
-- doesn't pull its entire history into memory (see
-- ListMessagesBySessionBefore for paging further back).
SELECT *
FROM messages
WHERE session_id = ?
ORDER BY created_at DESC
LIMIT ?;

-- name: ListMessagesBySessionBefore :many
-- Returns up to `limit` messages older than beforeCreatedAt, newest
-- first (reverse to get chronological order). Used to lazily page in
-- earlier history as the user scrolls up past the initially loaded
-- window. An empty result means there is nothing older left to load.
SELECT *
FROM messages
WHERE session_id = ? AND created_at < ?
ORDER BY created_at DESC
LIMIT ?;

-- name: ListMessagesBySessionSince :many
-- Returns every message for a session at or after sinceCreatedAt, in
-- chronological order. Used to make sure the initial windowed load
-- always includes the full post-compaction tail (from the session's
-- SummaryMessageID onward) even when that tail is longer than the
-- default recent-message window.
SELECT *
FROM messages
WHERE session_id = ? AND created_at >= ?
ORDER BY created_at ASC;


-- name: CreateMessage :one
INSERT INTO messages (
    id,
    session_id,
    role,
    parts,
    model,
    provider,
    is_summary_message,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: CreateMessageWithTimestamp :one
-- Like CreateMessage but preserves explicit created_at/updated_at values.
-- Used when copying messages (e.g. rewind forks) so the original ordering
-- is retained; ListMessagesBySession orders by created_at.
INSERT INTO messages (
    id,
    session_id,
    role,
    parts,
    model,
    provider,
    is_summary_message,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: UpdateMessage :exec
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;


-- name: DeleteMessage :exec
DELETE FROM messages
WHERE id = ?;

-- name: DeleteSessionMessages :exec
DELETE FROM messages
WHERE session_id = ?;

-- name: ListUserMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ? AND role = 'user'
ORDER BY created_at DESC;

-- name: ListAllUserMessages :many
SELECT *
FROM messages
WHERE role = 'user'
ORDER BY created_at DESC;

-- name: GetLastAssistantMessageBySession :one
SELECT *
FROM messages
WHERE session_id = ? AND role = 'assistant' AND is_summary_message = 0
ORDER BY created_at DESC
LIMIT 1;

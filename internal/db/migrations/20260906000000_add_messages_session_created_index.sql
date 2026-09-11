-- +goose Up
-- +goose StatementBegin
-- Composite index for windowed/paginated message loading: queries filter
-- by session_id and order/cursor on created_at together (see
-- ListMessagesBySessionRecent, ListMessagesBySessionBefore,
-- ListMessagesBySessionSince), which the separate single-column indexes
-- on session_id and created_at cannot satisfy without a scan for large
-- sessions.
CREATE INDEX IF NOT EXISTS idx_messages_session_id_created_at ON messages (session_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_messages_session_id_created_at;
-- +goose StatementEnd

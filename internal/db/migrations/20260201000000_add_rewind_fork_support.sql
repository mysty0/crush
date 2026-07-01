-- +goose Up
-- +goose StatementBegin
-- Link each file version to the message whose turn produced it, so a
-- rewind can reconstruct the on-disk state as of a given message.
ALTER TABLE files ADD COLUMN message_id TEXT;

-- Record fork provenance: when a session is created by rewinding another
-- session, forked_from_session_id points at the origin and
-- forked_at_message_id is the message the fork was truncated to. Forks
-- are top-level sessions (parent_session_id stays NULL) so they appear
-- in the normal session list.
ALTER TABLE sessions ADD COLUMN forked_from_session_id TEXT;
ALTER TABLE sessions ADD COLUMN forked_at_message_id TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN forked_at_message_id;
ALTER TABLE sessions DROP COLUMN forked_from_session_id;
ALTER TABLE files DROP COLUMN message_id;
-- +goose StatementEnd

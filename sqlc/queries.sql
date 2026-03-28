-- name: UpsertOutbox :exec
INSERT INTO sent_messages (conversation_id, message_id, text)
VALUES (?, ?, ?)
ON CONFLICT(conversation_id, message_id) DO UPDATE SET text = excluded.text;

-- name: GetOutbox :one
SELECT text FROM sent_messages
WHERE conversation_id = ? AND message_id = ?;

-- name: CountOutbox :one
SELECT count(*) FROM sent_messages WHERE conversation_id = ?;

-- name: DeleteOldestOutbox :exec
DELETE FROM sent_messages
WHERE rowid IN (
    SELECT sm.rowid FROM sent_messages sm
    WHERE sm.conversation_id = ?
    ORDER BY sm.rowid ASC
    LIMIT ?
);

-- name: EnqueueInbox :exec
INSERT INTO inbox (priority, source, content, reply_to)
VALUES (?, ?, ?, ?);

-- name: DequeueInbox :one
DELETE FROM inbox
WHERE id = (
    SELECT id FROM inbox
    ORDER BY priority ASC, id ASC
    LIMIT 1
)
RETURNING id, priority, source, content, reply_to, created_at;

-- name: PeekInbox :one
SELECT id, priority, source, content, reply_to, created_at
FROM inbox
ORDER BY priority ASC, id ASC
LIMIT 1;

-- name: DeleteInboxItem :exec
DELETE FROM inbox WHERE id = ?;

-- name: DeleteStaleItems :exec
DELETE FROM inbox WHERE source IN ('heartbeat', 'compact');

-- name: DequeueUserItems :many
DELETE FROM inbox
WHERE source = 'user'
RETURNING id, priority, source, content, reply_to, created_at;

-- name: CountInbox :one
SELECT count(*) FROM inbox;

-- name: EnqueueHeartbeatIfEmpty :execresult
INSERT INTO inbox (priority, source, content, reply_to)
SELECT ?, 'heartbeat', '', ''
WHERE NOT EXISTS (SELECT 1 FROM inbox WHERE source = 'heartbeat');

-- name: DueReminders :many
-- datetime() normalizes ISO 8601 variants (Z vs +00:00, T vs space) so
-- lexicographic comparison doesn't break on agent-formatted timestamps.
DELETE FROM reminders
WHERE datetime(fire_at) <= datetime(?)
RETURNING id, fire_at, prompt;

-- name: InsertReminder :exec
INSERT INTO reminders (fire_at, prompt) VALUES (?, ?);

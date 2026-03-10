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

-- name: DeleteHeartbeatItems :exec
DELETE FROM inbox WHERE source = 'heartbeat';

-- name: CountInbox :one
SELECT count(*) FROM inbox;

-- name: EnqueueHeartbeatIfEmpty :execresult
INSERT INTO inbox (priority, source, content, reply_to)
SELECT ?, 'heartbeat', '', ''
WHERE NOT EXISTS (SELECT 1 FROM inbox WHERE source = 'heartbeat');

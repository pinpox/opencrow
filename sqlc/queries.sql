-- name: UpsertSentMessage :exec
INSERT INTO sent_messages (conversation_id, message_id, text)
VALUES (?, ?, ?)
ON CONFLICT(conversation_id, message_id) DO UPDATE SET text = excluded.text;

-- name: GetSentMessage :one
SELECT text FROM sent_messages
WHERE conversation_id = ? AND message_id = ?;

-- name: CountByConversation :one
SELECT count(*) FROM sent_messages WHERE conversation_id = ?;

-- name: DeleteOldestMessages :exec
DELETE FROM sent_messages
WHERE rowid IN (
    SELECT sm.rowid FROM sent_messages sm
    WHERE sm.conversation_id = ?
    ORDER BY sm.rowid ASC
    LIMIT ?
);

-- name: PruneSeenRumors :exec
DELETE FROM seen_rumors WHERE seen < ?;

-- name: NewestSeenRumor :one
SELECT CAST(COALESCE(max(seen), 0) AS INTEGER) FROM seen_rumors;

-- name: CountSeenRumors :one
SELECT count(*) FROM seen_rumors;

-- name: UpsertSeenRumor :exec
INSERT INTO seen_rumors (id, seen) VALUES (?, ?)
ON CONFLICT(id) DO UPDATE SET seen = excluded.seen;

-- name: InsertSeenRumorIfNew :execrows
INSERT OR IGNORE INTO seen_rumors (id, seen) VALUES (?, ?);

-- name: ListPublishQueue :many
SELECT id, event_json, relays, created_at FROM publish_queue ORDER BY created_at, id;

-- name: InsertPublishItem :execlastid
INSERT INTO publish_queue (event_json, relays, created_at) VALUES (?, ?, ?);

-- name: DeletePublishItem :exec
DELETE FROM publish_queue WHERE id = ?;

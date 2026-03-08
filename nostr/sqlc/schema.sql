CREATE TABLE IF NOT EXISTS seen_rumors (
    id   TEXT PRIMARY KEY,
    seen INTEGER NOT NULL  -- unix timestamp
);

CREATE INDEX IF NOT EXISTS idx_seen_rumors_seen ON seen_rumors (seen);

CREATE TABLE IF NOT EXISTS publish_queue (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_json TEXT    NOT NULL, -- signed Nostr event as JSON
    relays     TEXT    NOT NULL, -- newline-separated relay URLs
    created_at INTEGER NOT NULL  -- unix timestamp for ordering
);

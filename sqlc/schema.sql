CREATE TABLE IF NOT EXISTS sent_messages (
    conversation_id TEXT NOT NULL,
    message_id      TEXT NOT NULL,
    text            TEXT NOT NULL,
    PRIMARY KEY (conversation_id, message_id)
);

CREATE TABLE IF NOT EXISTS inbox (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    priority   INTEGER NOT NULL DEFAULT 2,  -- 0=user, 1=trigger, 2=heartbeat
    source     TEXT    NOT NULL,             -- "user", "trigger", "heartbeat"
    content    TEXT    NOT NULL DEFAULT '',
    reply_to   TEXT    NOT NULL DEFAULT '',  -- backend message ID to reply to
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

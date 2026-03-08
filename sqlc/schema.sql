CREATE TABLE IF NOT EXISTS sent_messages (
    conversation_id TEXT NOT NULL,
    message_id      TEXT NOT NULL,
    text            TEXT NOT NULL,
    PRIMARY KEY (conversation_id, message_id)
);

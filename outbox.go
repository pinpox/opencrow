package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
)

const maxOutboxPerConversation = 500

// outboxStore tracks outgoing message text keyed by backend-specific
// message ID, so we can resolve reply-to references from users. Entries are
// per-conversation and bounded to avoid unbounded growth. The store is
// persisted to SQLite so reply context survives bot restarts.
type outboxStore struct {
	queries *Queries
	db      *sql.DB
}

// newOutboxStore wraps an existing database connection. The caller owns
// the DB lifecycle — outboxStore never closes it.
func newOutboxStore(db *sql.DB) *outboxStore {
	return &outboxStore{
		db:      db,
		queries: New(db),
	}
}

// Put records an outgoing message. If the conversation exceeds the max,
// the oldest entries (by rowid insertion order) are evicted. The upsert,
// count, and eviction run in a single transaction to avoid concurrent
// Put calls over-deleting entries.
func (s *outboxStore) Put(ctx context.Context, conversationID, messageID, text string) {
	if messageID == "" {
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("failed to begin outbox tx", "error", err)

		return
	}

	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	q := s.queries.WithTx(tx)

	if err := q.UpsertOutbox(ctx, UpsertOutboxParams{
		ConversationID: conversationID,
		MessageID:      messageID,
		Text:           text,
	}); err != nil {
		slog.Warn("failed to upsert outbox", "error", err)

		return
	}

	count, err := q.CountOutbox(ctx, conversationID)
	if err != nil {
		slog.Warn("failed to count outbox", "error", err)

		return
	}

	if count > maxOutboxPerConversation {
		excess := count - maxOutboxPerConversation
		if err := q.DeleteOldestOutbox(ctx, DeleteOldestOutboxParams{
			ConversationID: conversationID,
			Limit:          excess,
		}); err != nil {
			slog.Warn("failed to evict old outbox", "error", err)

			return
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("failed to commit outbox tx", "error", err)
	}
}

// Get returns the text of a previously sent message, or "" if unknown.
func (s *outboxStore) Get(ctx context.Context, conversationID, messageID string) string {
	if messageID == "" {
		return ""
	}

	text, err := s.queries.GetOutbox(ctx, GetOutboxParams{
		ConversationID: conversationID,
		MessageID:      messageID,
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.Error("unexpected error reading outbox",
				"conversation", conversationID,
				"message", messageID,
				"error", err,
			)
		}

		return ""
	}

	return text
}

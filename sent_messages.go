package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	// Register the pure-Go SQLite driver for the sent messages store.
	_ "modernc.org/sqlite"
)

//go:embed sqlc/schema.sql
var sentMessagesSchema string

const (
	maxSentMessagesPerConversation = 500
	sentMessagesDBFile             = "sent_messages.db"
)

// sentMessageStore tracks outgoing message text keyed by backend-specific
// message ID, so we can resolve reply-to references from users. Entries are
// per-conversation and bounded to avoid unbounded growth. The store is
// persisted to SQLite so reply context survives bot restarts.
type sentMessageStore struct {
	queries *Queries
	db      *sql.DB
}

func newSentMessageStore(ctx context.Context, dataDir string) (*sentMessageStore, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("dataDir must not be empty")
	}

	dbPath := filepath.Join(dataDir, sentMessagesDBFile)

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening sent messages db: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()

		return nil, fmt.Errorf("pinging sent messages db: %w", err)
	}

	if err := migrateSentMessages(ctx, db); err != nil {
		db.Close()

		return nil, fmt.Errorf("migrating sent messages db: %w", err)
	}

	return &sentMessageStore{
		db:      db,
		queries: New(db),
	}, nil
}

func migrateSentMessages(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, sentMessagesSchema); err != nil {
		return fmt.Errorf("migrating sent messages db: %w", err)
	}

	return nil
}

// Put records a sent message. If the conversation exceeds the max, the
// oldest entries (by rowid insertion order) are evicted. The upsert,
// count, and eviction run in a single transaction to avoid concurrent
// Put calls over-deleting entries.
func (s *sentMessageStore) Put(ctx context.Context, conversationID, messageID, text string) {
	if messageID == "" {
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("failed to begin sent message tx", "error", err)

		return
	}

	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	q := s.queries.WithTx(tx)

	if err := q.UpsertSentMessage(ctx, UpsertSentMessageParams{
		ConversationID: conversationID,
		MessageID:      messageID,
		Text:           text,
	}); err != nil {
		slog.Warn("failed to upsert sent message", "error", err)

		return
	}

	count, err := q.CountByConversation(ctx, conversationID)
	if err != nil {
		slog.Warn("failed to count sent messages", "error", err)

		return
	}

	if count > maxSentMessagesPerConversation {
		excess := count - maxSentMessagesPerConversation
		if err := q.DeleteOldestMessages(ctx, DeleteOldestMessagesParams{
			ConversationID: conversationID,
			Limit:          excess,
		}); err != nil {
			slog.Warn("failed to evict old sent messages", "error", err)

			return
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("failed to commit sent message tx", "error", err)
	}
}

// Get returns the text of a previously sent message, or "" if unknown.
func (s *sentMessageStore) Get(ctx context.Context, conversationID, messageID string) string {
	if messageID == "" {
		return ""
	}

	text, err := s.queries.GetSentMessage(ctx, GetSentMessageParams{
		ConversationID: conversationID,
		MessageID:      messageID,
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, context.Canceled) {
			slog.Error("unexpected error reading sent message",
				"conversation", conversationID,
				"message", messageID,
				"error", err,
			)
		}

		return ""
	}

	return text
}

// Close closes the underlying database connection.
func (s *sentMessageStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing sent messages db: %w", err)
	}

	return nil
}

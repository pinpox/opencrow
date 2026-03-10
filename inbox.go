package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// Priority levels for inbox items. Lower number = higher priority.
const (
	PriorityUser      = 0
	PriorityTrigger   = 1
	PriorityHeartbeat = 2
)

//go:generate sqlc generate

// InboxStore is a persistent priority queue backed by SQLite.
type InboxStore struct {
	queries *Queries
}

// NewInboxStore wraps an existing database connection. The schema must
// already be applied (openDB handles this). Clears stale heartbeat and
// compact items left over from a previous crash.
func NewInboxStore(ctx context.Context, db *sql.DB) (*InboxStore, error) {
	queries := New(db)

	// Heartbeat and compact items have in-memory state that doesn't
	// survive a restart, so purge any left over from a previous run.
	if err := queries.DeleteStaleItems(ctx); err != nil {
		return nil, fmt.Errorf("clearing stale inbox items: %w", err)
	}

	return &InboxStore{queries: queries}, nil
}

// Enqueue inserts an item into the inbox.
func (s *InboxStore) Enqueue(ctx context.Context, priority int64, source, content, replyTo string) error {
	if err := s.queries.EnqueueInbox(ctx, EnqueueInboxParams{
		Priority: priority,
		Source:   source,
		Content:  content,
		ReplyTo:  replyTo,
	}); err != nil {
		return fmt.Errorf("enqueuing inbox item: %w", err)
	}

	slog.Info("inbox: enqueued", "source", source, "priority", priority)

	return nil
}

// Dequeue removes and returns the highest-priority (lowest number) item.
// Returns sql.ErrNoRows if the inbox is empty.
func (s *InboxStore) Dequeue(ctx context.Context) (Inbox, error) {
	return s.queries.DequeueInbox(ctx)
}

// Requeue re-inserts an item that was interrupted. Heartbeat markers are
// dropped since the timer will re-fire.
func (s *InboxStore) Requeue(ctx context.Context, item Inbox) error {
	if item.Source == sourceHeartbeat {
		return nil
	}

	if err := s.queries.EnqueueInbox(ctx, EnqueueInboxParams{
		Priority: item.Priority,
		Source:   item.Source,
		Content:  item.Content,
		ReplyTo:  item.ReplyTo,
	}); err != nil {
		return fmt.Errorf("requeueing %s item: %w", item.Source, err)
	}

	return nil
}

// Count returns the number of items in the inbox.
func (s *InboxStore) Count(ctx context.Context) (int64, error) {
	return s.queries.CountInbox(ctx)
}

// EnqueueHeartbeat atomically inserts a heartbeat marker only if none
// is already pending. Returns true if a row was inserted.
func (s *InboxStore) EnqueueHeartbeat(ctx context.Context) (bool, error) {
	result, err := s.queries.EnqueueHeartbeatIfEmpty(ctx, PriorityHeartbeat)
	if err != nil {
		return false, fmt.Errorf("enqueuing heartbeat: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking heartbeat insert result: %w", err)
	}

	return n > 0, nil
}

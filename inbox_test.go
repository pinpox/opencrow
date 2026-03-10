package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newTestDB creates an in-memory SQLite DB with the full schema applied.
func newTestDB(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, dbSchema); err != nil {
		db.Close()
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// newTestInboxWithDB creates an InboxStore using an existing DB connection.
func newTestInboxWithDB(ctx context.Context, t *testing.T, db *sql.DB) *InboxStore {
	t.Helper()

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	return inbox
}

// newTestInbox creates an InboxStore backed by an in-memory SQLite DB.
func newTestInbox(ctx context.Context, t *testing.T) *InboxStore {
	t.Helper()

	return newTestInboxWithDB(ctx, t, newTestDB(ctx, t))
}

func TestInbox_PriorityOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inbox := newTestInbox(ctx, t)

	must(t, inbox.Enqueue(ctx, PriorityHeartbeat, sourceHeartbeat, "", ""))
	must(t, inbox.Enqueue(ctx, PriorityTrigger, sourceTrigger, "event data", ""))
	must(t, inbox.Enqueue(ctx, PriorityUser, sourceUser, "urgent msg", ""))

	item1, err := inbox.Dequeue(ctx)
	must(t, err)

	if item1.Source != sourceUser {
		t.Errorf("first dequeue: Source = %q, want %q", item1.Source, sourceUser)
	}

	item2, err := inbox.Dequeue(ctx)
	must(t, err)

	if item2.Source != sourceTrigger {
		t.Errorf("second dequeue: Source = %q, want %q", item2.Source, sourceTrigger)
	}

	item3, err := inbox.Dequeue(ctx)
	must(t, err)

	if item3.Source != sourceHeartbeat {
		t.Errorf("third dequeue: Source = %q, want %q", item3.Source, sourceHeartbeat)
	}
}

func TestWorker_PreemptsLowerPriority(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inbox := newTestInbox(ctx, t)

	worker := NewWorker(inbox, PiConfig{SessionDir: t.TempDir()}, HeartbeatConfig{}, "")

	// Simulate a heartbeat running by setting worker state directly.
	cancelled := make(chan struct{})

	worker.mu.Lock()
	worker.currentPriority = PriorityHeartbeat
	worker.currentCancel = func() { close(cancelled) }
	worker.mu.Unlock()

	// Notify with user priority — should preempt the heartbeat.
	worker.Notify(PriorityUser)

	select {
	case <-cancelled:
		// good
	case <-time.After(1 * time.Second):
		t.Fatal("preemption did not cancel the running operation")
	}
}

func TestWorker_NoPreemptSamePriority(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inbox := newTestInbox(ctx, t)

	worker := NewWorker(inbox, PiConfig{SessionDir: t.TempDir()}, HeartbeatConfig{}, "")

	preempted := false

	worker.mu.Lock()
	worker.currentPriority = PriorityUser
	worker.currentCancel = func() { preempted = true }
	worker.mu.Unlock()

	worker.Notify(PriorityUser)

	if preempted {
		t.Error("same-priority notify should not preempt")
	}
}

// seedInbox inserts rows directly into the inbox table, bypassing
// NewInboxStore cleanup, to simulate items left over from a crash.
func seedInbox(t *testing.T, db *sql.DB, rows []string) {
	t.Helper()

	ctx := context.Background()

	for _, q := range rows {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInbox_ClearsStaleItemsOnInit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDB(ctx, t)

	// Heartbeat and compact items have in-memory state that doesn't
	// survive a restart; both must be purged on init.
	seedInbox(t, db, []string{
		"INSERT INTO inbox (priority, source, content) VALUES (2, 'heartbeat', '')",
		"INSERT INTO inbox (priority, source, content) VALUES (0, 'compact', '')",
		// These should survive.
		"INSERT INTO inbox (priority, source, content) VALUES (0, 'user', 'keep me')",
		"INSERT INTO inbox (priority, source, content) VALUES (1, 'trigger', 'event data')",
	})

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	count, err := inbox.Count(ctx)
	must(t, err)

	if count != 2 {
		t.Fatalf("count = %d, want 2 (heartbeat and compact should be cleared)", count)
	}

	item1, err := inbox.Dequeue(ctx)
	must(t, err)

	if item1.Source != sourceUser {
		t.Errorf("first item source = %q, want %q", item1.Source, sourceUser)
	}

	item2, err := inbox.Dequeue(ctx)
	must(t, err)

	if item2.Source != sourceTrigger {
		t.Errorf("second item source = %q, want %q", item2.Source, sourceTrigger)
	}
}

func TestInbox_Persistence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := dir + "/test.db?_journal_mode=WAL&_busy_timeout=5000"

	db1, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db1.ExecContext(ctx, dbSchema); err != nil {
		db1.Close()
		t.Fatal(err)
	}

	inbox1, err := NewInboxStore(ctx, db1)
	if err != nil {
		t.Fatal(err)
	}

	must(t, inbox1.Enqueue(ctx, PriorityTrigger, sourceTrigger, "survived crash", ""))
	db1.Close()

	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	defer db2.Close()

	if _, err := db2.ExecContext(ctx, dbSchema); err != nil {
		t.Fatal(err)
	}

	inbox2, err := NewInboxStore(ctx, db2)
	if err != nil {
		t.Fatal(err)
	}

	count, err := inbox2.Count(ctx)
	must(t, err)

	if count != 1 {
		t.Fatalf("count after reopen = %d, want 1", count)
	}

	item, err := inbox2.Dequeue(ctx)
	must(t, err)

	if item.Content != "survived crash" {
		t.Errorf("Content = %q, want %q", item.Content, "survived crash")
	}
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

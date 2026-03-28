package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openTestOutbox creates an outboxStore backed by a temp SQLite DB
// and registers cleanup.
func openTestOutbox(t *testing.T) *outboxStore {
	t.Helper()

	return newOutboxStore(newTestDB(context.Background(), t))
}

func TestOutbox_PutAndGet(t *testing.T) {
	t.Parallel()

	s := openTestOutbox(t)
	ctx := context.Background()

	s.Put(ctx, "room1", "msg1", "hello")
	s.Put(ctx, "room1", "msg2", "world")
	s.Put(ctx, "room2", "msg3", "other room")

	if got := s.Get(ctx, "room1", "msg1"); got != "hello" {
		t.Errorf("Get(room1, msg1) = %q, want %q", got, "hello")
	}

	if got := s.Get(ctx, "room1", "msg2"); got != "world" {
		t.Errorf("Get(room1, msg2) = %q, want %q", got, "world")
	}

	if got := s.Get(ctx, "room2", "msg3"); got != "other room" {
		t.Errorf("Get(room2, msg3) = %q, want %q", got, "other room")
	}

	if got := s.Get(ctx, "room1", "nonexistent"); got != "" {
		t.Errorf("Get(room1, nonexistent) = %q, want empty", got)
	}

	if got := s.Get(ctx, "nonexistent", "msg1"); got != "" {
		t.Errorf("Get(nonexistent, msg1) = %q, want empty", got)
	}
}

func TestOutbox_EmptyIDIgnored(t *testing.T) {
	t.Parallel()

	s := openTestOutbox(t)
	ctx := context.Background()

	s.Put(ctx, "room1", "", "should be ignored")

	if got := s.Get(ctx, "room1", ""); got != "" {
		t.Errorf("Get(room1, empty) = %q, want empty", got)
	}
}

func TestOutbox_Eviction(t *testing.T) {
	t.Parallel()

	s := openTestOutbox(t)
	ctx := context.Background()

	// Fill beyond the limit.
	for i := range maxOutboxPerConversation + 10 {
		s.Put(ctx, "room1", fmt.Sprintf("msg%d", i), fmt.Sprintf("text%d", i))
	}

	// The first 10 should have been evicted.
	for i := range 10 {
		if got := s.Get(ctx, "room1", fmt.Sprintf("msg%d", i)); got != "" {
			t.Errorf("msg%d should have been evicted, got %q", i, got)
		}
	}

	// The rest should still be there.
	for i := 10; i < maxOutboxPerConversation+10; i++ {
		want := fmt.Sprintf("text%d", i)
		if got := s.Get(ctx, "room1", fmt.Sprintf("msg%d", i)); got != want {
			t.Errorf("msg%d = %q, want %q", i, got, want)
		}
	}
}

func TestOutbox_DuplicatePutNoCorruption(t *testing.T) {
	t.Parallel()

	s := openTestOutbox(t)
	ctx := context.Background()

	// Insert max entries, then overwrite the first one. This must not
	// create a duplicate in Order, which would break eviction.
	for i := range maxOutboxPerConversation {
		s.Put(ctx, "room1", fmt.Sprintf("msg%d", i), fmt.Sprintf("text%d", i))
	}

	// Overwrite msg0 with new text — should update value, not grow Order.
	s.Put(ctx, "room1", "msg0", "updated")

	if got := s.Get(ctx, "room1", "msg0"); got != "updated" {
		t.Errorf("Get(room1, msg0) = %q, want %q", got, "updated")
	}

	// Add one more entry. If Order had a duplicate msg0, two entries
	// would be evicted and msg1 would disappear.
	s.Put(ctx, "room1", "new", "new-text")

	if got := s.Get(ctx, "room1", "msg1"); got != "text1" {
		t.Errorf("msg1 should survive, got %q", got)
	}
}

func TestOutbox_GetCancelledContext(t *testing.T) {
	t.Parallel()

	s := openTestOutbox(t)
	bg := context.Background()

	s.Put(bg, "room1", "msg1", "hello")

	ctx, cancel := context.WithCancel(bg)
	cancel()

	if got := s.Get(ctx, "room1", "msg1"); got != "" {
		t.Errorf("Get with cancelled ctx = %q, want empty", got)
	}
}

func TestOutbox_GetAfterClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDBAt(ctx, t, filepath.Join(t.TempDir(), "test.db"))
	s := newOutboxStore(db)

	s.Put(ctx, "room1", "msg1", "hello")

	// Close the DB to provoke a real (non-ErrNoRows) error on the next Get.
	db.Close()

	if got := s.Get(ctx, "room1", "msg1"); got != "" {
		t.Errorf("Get after close = %q, want empty", got)
	}
}

func TestOutbox_Persistence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// First connection: write some messages.
	db1 := newTestDBAt(ctx, t, dbPath)
	s1 := newOutboxStore(db1)
	s1.Put(ctx, "room1", "msg1", "hello")
	s1.Put(ctx, "room1", "msg2", "world")
	db1.Close()

	// Second connection: reads from the same file.
	s2 := newOutboxStore(newTestDBAt(ctx, t, dbPath))

	if got := s2.Get(ctx, "room1", "msg1"); got != "hello" {
		t.Errorf("after reload, Get(room1, msg1) = %q, want %q", got, "hello")
	}

	if got := s2.Get(ctx, "room1", "msg2"); got != "world" {
		t.Errorf("after reload, Get(room1, msg2) = %q, want %q", got, "world")
	}
}

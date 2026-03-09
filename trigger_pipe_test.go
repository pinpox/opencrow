package main

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestTriggerPipeReader_EnqueuesLines(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	dbPath := t.TempDir() + "/test.db"

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}

	defer db.Close()

	if _, err := db.ExecContext(ctx, dbSchema); err != nil {
		t.Fatal(err)
	}

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	worker := NewWorker(inbox, PiConfig{SessionDir: dir}, HeartbeatConfig{}, "")
	startTriggerPipe(ctx, worker, dir)

	pipePath := TriggerPipePath(dir)
	waitForFIFO(t, pipePath)
	writeToPipe(t, pipePath, "first trigger\nsecond trigger\n")
	waitForInboxCount(ctx, t, inbox, 2)

	item1, err := inbox.Dequeue(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if item1.Source != sourceTrigger {
		t.Errorf("item1.Source = %q, want %q", item1.Source, sourceTrigger)
	}

	if item1.Priority != PriorityTrigger {
		t.Errorf("item1.Priority = %d, want %d", item1.Priority, PriorityTrigger)
	}

	if item1.Content != "first trigger" {
		t.Errorf("item1.Content = %q, want %q", item1.Content, "first trigger")
	}

	item2, err := inbox.Dequeue(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if item2.Content != "second trigger" {
		t.Errorf("item2.Content = %q, want %q", item2.Content, "second trigger")
	}
}

func waitForFIFO(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		info, err := os.Lstat(path)
		if err == nil && info.Mode()&os.ModeNamedPipe != 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("FIFO not created at %s within timeout", path)
}

func writeToPipe(t *testing.T, path, content string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func waitForInboxCount(ctx context.Context, t *testing.T, inbox *InboxStore, want int64) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		count, _ := inbox.Count(ctx)

		if count >= want {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("inbox count did not reach %d within timeout", want)
}

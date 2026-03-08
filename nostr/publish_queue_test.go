package nostr

import (
	"context"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/pinpox/opencrow/testutil"
)

const unreachableRelay = "ws://127.0.0.1:1"

func TestPublishQueue_EnqueueAndDrain(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	ctx := context.Background()

	q := mustNewPublishQueue(t, t.TempDir())
	defer q.db.Close()

	q.setPool(pool)

	evt := signTestEvent(t)

	q.enqueue(ctx, evt, []string{wsURL}, "test")

	if q.Len() != 1 {
		t.Fatalf("queue length = %d, want 1", q.Len())
	}

	// Items are due immediately, so drain should publish them.
	q.drainOnce(ctx)

	if q.Len() != 0 {
		t.Errorf("queue length after drain = %d, want 0", q.Len())
	}

	// Verify the event actually landed on the relay.
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	events := pool.FetchMany(fetchCtx, []string{wsURL}, gonostr.Filter{
		Kinds:   []gonostr.Kind{0},
		Authors: []gonostr.PubKey{evt.PubKey},
	}, gonostr.SubscriptionOptions{})

	found := false

	for ie := range events {
		if ie.ID == evt.ID {
			found = true
		}
	}

	if !found {
		t.Error("event not found on relay after drain")
	}
}

func TestPublishQueue_PartialSuccess_DeliveredNotPersisted(t *testing.T) {
	t.Parallel()

	goodURL, cleanupGood := testutil.StartTestRelay(t)
	defer cleanupGood()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	ctx := context.Background()

	dir := t.TempDir()

	q := mustNewPublishQueue(t, dir)
	defer q.db.Close()

	q.setPool(pool)

	evt := signTestEvent(t)

	// Enqueue to one good relay and one unreachable relay.
	q.enqueue(ctx, evt, []string{goodURL, unreachableRelay}, "test")

	q.drainOnce(ctx)

	// Good relay succeeded → item is delivered. Bad relay still pending.
	if q.Len() != 1 {
		t.Fatalf("queue length = %d, want 1 (bad relay still pending)", q.Len())
	}

	q.mu.Lock()
	if !q.items[0].Delivered {
		t.Error("item should be marked as delivered (one relay succeeded)")
	}
	q.mu.Unlock()

	// Delivered items should NOT be persisted in the DB.
	rows, err := q.db.queries.ListPublishQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 0 {
		t.Errorf("persisted %d items, want 0 (delivered items should not be persisted)", len(rows))
	}
}

func TestPublishQueue_PersistenceAcrossRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	evt := signTestEvent(t)

	// First "run": enqueue to an unreachable relay, drain fails, persists.
	pool1 := gonostr.NewPool(gonostr.PoolOptions{})

	q1 := mustNewPublishQueue(t, dir)
	q1.setPool(pool1)
	q1.enqueue(context.Background(), evt, []string{unreachableRelay}, "test")
	q1.drainOnce(context.Background())

	if q1.Len() != 1 {
		t.Fatalf("q1 length = %d, want 1", q1.Len())
	}

	pool1.Close("first run done")
	q1.db.Close()

	// Second "run": load from DB, verify the item survived.
	q2 := mustNewPublishQueue(t, dir)

	if q2.Len() != 1 {
		t.Fatalf("q2 length after reload = %d, want 1", q2.Len())
	}

	q2.mu.Lock()
	loaded := q2.items[0]
	q2.mu.Unlock()

	if loaded.Event.ID != evt.ID {
		t.Errorf("loaded event ID = %s, want %s", loaded.Event.ID.Hex(), evt.ID.Hex())
	}

	if loaded.Delivered {
		t.Error("loaded item should not be marked as delivered")
	}

	q2.db.Close()
}

func TestPublishQueue_BackoffCapsAtMax(t *testing.T) {
	t.Parallel()

	prev := calcBackoff(1)

	for attempt := 2; attempt <= 20; attempt++ {
		cur := calcBackoff(attempt)
		if cur < prev && cur != maxBackoff {
			t.Errorf("calcBackoff(%d) = %v < calcBackoff(%d) = %v", attempt, cur, attempt-1, prev)
		}

		if cur > maxBackoff {
			t.Errorf("calcBackoff(%d) = %v > maxBackoff %v", attempt, cur, maxBackoff)
		}

		prev = cur
	}

	if calcBackoff(100) != maxBackoff {
		t.Errorf("calcBackoff(100) = %v, want %v (should be capped)", calcBackoff(100), maxBackoff)
	}
}

// mustNewPublishQueue opens a DB in dataDir and creates a publish queue.
func mustNewPublishQueue(t *testing.T, dataDir string) *publishQueue {
	t.Helper()

	db, err := OpenDB(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}

	q, err := newPublishQueue(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}

	return q
}

func signTestEvent(t *testing.T) gonostr.Event {
	t.Helper()

	sk := gonostr.Generate()

	evt := gonostr.Event{
		Kind:      0,
		CreatedAt: gonostr.Now(),
		Content:   "test",
		PubKey:    sk.Public(),
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatal(err)
	}

	return evt
}

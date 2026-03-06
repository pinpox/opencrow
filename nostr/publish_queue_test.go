package nostr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/pinpox/opencrow/testutil"
)

const unreachableRelay = "ws://127.0.0.1:1"

func TestPublishQueue_EnqueueAndRetry(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	q := newPublishQueue(t.TempDir(), pool)
	evt := signTestEvent(t)

	// Enqueue with the relay as "failed" — the relay is actually up,
	// so retry should succeed.
	q.enqueue(evt, []string{wsURL}, []string{wsURL})

	if q.Len() != 1 {
		t.Fatalf("queue length = %d, want 1", q.Len())
	}

	// Force immediate retry by setting NextRetry to the past.
	q.mu.Lock()
	q.items[0].NextRetry = time.Now().Add(-time.Second)
	q.mu.Unlock()

	ctx := context.Background()
	retried := q.retryOnce(ctx)

	if retried != 1 {
		t.Fatalf("retried = %d, want 1", retried)
	}

	if q.Len() != 0 {
		t.Errorf("queue length after retry = %d, want 0", q.Len())
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
		t.Error("event not found on relay after retry")
	}
}

func TestPublishQueue_PerRelayTracking(t *testing.T) {
	t.Parallel()

	goodURL, cleanupGood := testutil.StartTestRelay(t)
	defer cleanupGood()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	dir := t.TempDir()
	q := newPublishQueue(dir, pool)

	evt := signTestEvent(t)

	// Good relay succeeded initially, bad relay failed.
	q.enqueue(evt, []string{goodURL, unreachableRelay}, []string{unreachableRelay})

	if q.Len() != 1 {
		t.Fatalf("queue length = %d, want 1", q.Len())
	}

	// Item is already delivered (good relay worked), so it should be
	// in-memory only for best-effort retries.
	q.mu.Lock()
	if !q.items[0].Delivered {
		t.Error("item should be marked as delivered (one relay succeeded)")
	}

	q.saveLocked()
	q.mu.Unlock()

	// Verify persistence: delivered items should NOT be written to disk.
	data, err := os.ReadFile(filepath.Join(dir, publishQueueFile))
	if err != nil {
		t.Fatal(err)
	}

	var persisted []*publishItem
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}

	if len(persisted) != 0 {
		t.Errorf("persisted %d items, want 0 (delivered items should not be persisted)", len(persisted))
	}
}

func TestPublishQueue_PersistenceAcrossRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	evt := signTestEvent(t)

	// First "run": enqueue with an unreachable relay so item stays undelivered.
	pool1 := gonostr.NewPool(gonostr.PoolOptions{})
	q1 := newPublishQueue(dir, pool1)
	q1.enqueue(evt, []string{unreachableRelay}, []string{unreachableRelay})

	if q1.Len() != 1 {
		t.Fatalf("q1 length = %d, want 1", q1.Len())
	}

	pool1.Close("first run done")

	// Second "run": load from disk, verify the item survived.
	pool2 := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool2.Close("second run done")

	q2 := newPublishQueue(dir, pool2)

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

	if len(loaded.FailedRelays) != 1 || loaded.FailedRelays[0] != unreachableRelay {
		t.Errorf("loaded failed relays = %v, want [%s]", loaded.FailedRelays, unreachableRelay)
	}
}

func TestPublishQueue_BackoffCapsAtMax(t *testing.T) {
	t.Parallel()

	// Verify backoff grows and is capped at maxBackoff.
	prev := calcBackoff(1)

	for attempt := 2; attempt <= 20; attempt++ {
		cur := calcBackoff(attempt)
		if cur < prev && cur != maxBackoff {
			t.Errorf("calcBackoff(%d) = %v < calcBackoff(%d) = %v (should be monotonically increasing)", attempt, cur, attempt-1, prev)
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

// signTestEvent creates and signs a minimal kind 0 event for testing.
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

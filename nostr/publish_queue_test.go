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

	// One good relay, one unreachable.
	q.enqueue(ctx, evt, []string{goodURL, unreachableRelay}, "test")

	q.drainOnce(ctx)

	// Good relay succeeded → delivered. Bad relay still pending in memory.
	if q.Len() != 1 {
		t.Fatalf("queue length = %d, want 1 (bad relay still pending)", q.Len())
	}

	q.mu.Lock()
	item := q.items[0]

	if !item.Delivered {
		t.Error("item should be marked as delivered (one relay succeeded)")
	}

	if _, ok := item.Pending[unreachableRelay]; !ok {
		t.Error("unreachable relay should still be in Pending set")
	}

	if _, ok := item.Pending[goodURL]; ok {
		t.Error("good relay should have been removed from Pending set")
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

	if _, ok := loaded.Pending[unreachableRelay]; !ok {
		t.Error("loaded item should have relay in Pending set")
	}

	q2.db.Close()
}

func TestPublishQueue_DeliveredTTL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	q := mustNewPublishQueue(t, t.TempDir())
	defer q.db.Close()

	// Fabricate an item that's delivered but has a straggler relay,
	// and is older than ttlDelivered.
	q.mu.Lock()
	q.items = append(q.items, &publishItem{
		Event:     signTestEvent(t),
		Pending:   map[string]struct{}{unreachableRelay: {}},
		Delivered: true,
		CreatedAt: time.Now().Add(-ttlDelivered - time.Minute),
	})
	q.evictLocked(ctx, time.Now())
	n := len(q.items)
	q.mu.Unlock()

	if n != 0 {
		t.Errorf("delivered item past TTL not evicted: %d items remain", n)
	}
}

func TestPublishQueue_UndeliveredTTL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()

	q := mustNewPublishQueue(t, dir)
	defer q.db.Close()

	evt := signTestEvent(t)

	// Enqueue normally so it's persisted.
	q.enqueue(ctx, evt, []string{unreachableRelay}, "test")

	// Age it past the undelivered TTL.
	q.mu.Lock()
	q.items[0].CreatedAt = time.Now().Add(-ttlUndelivered - time.Hour)
	q.evictLocked(ctx, time.Now())
	n := len(q.items)
	q.mu.Unlock()

	if n != 0 {
		t.Errorf("undelivered item past TTL not evicted: %d items remain", n)
	}

	// DB row should also be gone.
	rows, err := q.db.queries.ListPublishQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 0 {
		t.Errorf("expired item still in DB: %d rows", len(rows))
	}
}

func TestRelayBreaker_TripsAfterThreshold(t *testing.T) {
	t.Parallel()

	b := &relayBreaker{state: closed}
	now := time.Now()

	for i := range breakerThreshold - 1 {
		b.onFailure(now)

		if b.state != closed {
			t.Fatalf("breaker opened after %d failures, want %d", i+1, breakerThreshold)
		}
	}

	b.onFailure(now)

	if b.state != open {
		t.Errorf("breaker state = %v, want open after %d failures", b.state, breakerThreshold)
	}

	if b.allow(now) != deny {
		t.Error("open breaker should deny before cooldown expires")
	}

	// Regression: first trip must start at base cooldown, not 2×base.
	if b.cooldown != breakerBaseCooldown {
		t.Errorf("first-trip cooldown = %v, want %v", b.cooldown, breakerBaseCooldown)
	}
}

func TestRelayBreaker_HalfOpenProbe(t *testing.T) {
	t.Parallel()

	b := &relayBreaker{state: closed}
	now := time.Now()

	for range breakerThreshold {
		b.onFailure(now)
	}

	// Advance past cooldown.
	later := b.nextProbe.Add(time.Second)

	if got := b.allow(later); got != allowProbe {
		t.Fatalf("allow after cooldown = %v, want allowProbe", got)
	}

	if b.state != halfOpen {
		t.Errorf("state after probe = %v, want halfOpen", b.state)
	}

	// Second call while half-open should deny (one probe only).
	if got := b.allow(later); got != deny {
		t.Errorf("second allow while halfOpen = %v, want deny", got)
	}

	// Probe succeeds → breaker closes and resets.
	b.onSuccess()

	if b.state != closed {
		t.Errorf("state after success = %v, want closed", b.state)
	}

	if b.failures != 0 {
		t.Errorf("failures after success = %d, want 0", b.failures)
	}
}

func TestRelayBreaker_CooldownDoublesAndCaps(t *testing.T) {
	t.Parallel()

	b := &relayBreaker{state: closed}
	now := time.Now()

	// Trip initially.
	for range breakerThreshold {
		b.onFailure(now)
	}

	if b.cooldown != breakerBaseCooldown {
		t.Fatalf("initial cooldown = %v, want %v", b.cooldown, breakerBaseCooldown)
	}

	prev := b.cooldown

	for range 20 {
		// Simulate: cooldown expires, probe fails, re-opens.
		now = b.nextProbe.Add(time.Second)
		b.allow(now) // transitions to halfOpen
		b.onFailure(now)

		if b.cooldown < prev && b.cooldown != breakerMaxCooldown {
			t.Errorf("cooldown shrank: %v < %v", b.cooldown, prev)
		}

		if b.cooldown > breakerMaxCooldown {
			t.Errorf("cooldown %v exceeds max %v", b.cooldown, breakerMaxCooldown)
		}

		prev = b.cooldown
	}

	if b.cooldown != breakerMaxCooldown {
		t.Errorf("cooldown = %v, want capped at %v", b.cooldown, breakerMaxCooldown)
	}
}

func TestPublishQueue_BreakerGC(t *testing.T) {
	t.Parallel()

	q := mustNewPublishQueue(t, t.TempDir())
	defer q.db.Close()

	q.mu.Lock()
	// Closed breaker with no referencing item → should be GC'd.
	q.breakers["wss://gone.example"] = &relayBreaker{state: closed}
	// Open breaker → should be KEPT even without a referencing item,
	// so a future enqueue inherits the cooldown.
	q.breakers["wss://dead.example"] = &relayBreaker{state: open}
	q.gcBreakersLocked()
	q.mu.Unlock()

	if _, ok := q.breakers["wss://gone.example"]; ok {
		t.Error("closed unreferenced breaker not GC'd")
	}

	if _, ok := q.breakers["wss://dead.example"]; !ok {
		t.Error("open breaker was GC'd but should persist for future enqueues")
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

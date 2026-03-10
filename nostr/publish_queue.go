package nostr

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"
)

const (
	initialBackoff    = 5 * time.Second
	maxBackoff        = 5 * time.Minute
	backoffMultiplier = 2.0
	publishTimeout    = 15 * time.Second
)

// publishItem is a single event that needs to be published to a set of relays.
// Once at least one relay accepts the event, the item is removed from the
// persistent queue (best-effort retries continue in-memory for remaining relays).
type publishItem struct {
	// dbID is the database row ID (0 for in-memory-only items).
	dbID int64
	// Event is the signed Nostr event to publish.
	Event gonostr.Event
	// Relays is the full set of target relay URLs for this item.
	Relays []string
	// FailedRelays tracks which relays still need delivery.
	FailedRelays []string
	// Delivered is true once at least one relay accepted the event.
	Delivered bool
	// Attempts counts total publish rounds.
	Attempts int
	// NextRetry is when this item should next be attempted.
	NextRetry time.Time
	// CreatedAt records when the item was first enqueued.
	CreatedAt time.Time
}

// publishQueue is an outbox that all publishes go through. A background
// goroutine drains it, retrying with exponential backoff. Once at least
// one relay per item accepts the event, the item is removed from the
// persistent store but best-effort retries continue in-memory for
// remaining relays.
type publishQueue struct {
	mu           sync.Mutex
	items        []*publishItem
	flushWaiters []flushWaiter // released once attemptGen advances past their snapshot
	attemptGen   uint64        // incremented on every drain pass that tries items
	db           *DB
	pool         *gonostr.Pool // set via setPool before calling run
	wake         chan struct{} // signals the drain loop that new work arrived
}

// flushWaiter records a waiter and the generation it observed at Flush time.
type flushWaiter struct {
	gen uint64
	ch  chan struct{}
}

// Len returns the number of items in the queue.
func (q *publishQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.items)
}

// Flush blocks until at least one publish attempt pass has occurred since
// the call to Flush. Intended for tests.
func (q *publishQueue) Flush(ctx context.Context) {
	done := make(chan struct{})

	q.mu.Lock()
	q.flushWaiters = append(q.flushWaiters, flushWaiter{gen: q.attemptGen, ch: done})
	q.mu.Unlock()

	q.signal()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func newPublishQueue(ctx context.Context, db *DB) (*publishQueue, error) {
	q := &publishQueue{
		db:   db,
		wake: make(chan struct{}, 1),
	}

	if err := q.load(ctx); err != nil {
		return nil, err
	}

	return q, nil
}

// setPool sets the relay pool used for publishing. Must be called before run.
func (q *publishQueue) setPool(pool *gonostr.Pool) {
	q.mu.Lock()
	q.pool = pool
	q.mu.Unlock()

	// Wake the drain loop in case items were loaded from disk.
	if q.Len() > 0 {
		q.signal()
	}
}

// enqueue adds an event to be published to the given relays. The drain
// goroutine will handle the actual publishing.
func (q *publishQueue) enqueue(ctx context.Context, evt gonostr.Event, relays []string, label string) {
	if len(relays) == 0 {
		return
	}

	item := &publishItem{
		Event:        evt,
		Relays:       relays,
		FailedRelays: relays,
		Delivered:    false,
		Attempts:     0,
		NextRetry:    time.Now(), // due immediately
		CreatedAt:    time.Now(),
	}

	q.mu.Lock()
	q.items = append(q.items, item)

	if err := q.insertItemLocked(ctx, item); err != nil {
		// Item stays in memory for delivery this process lifetime,
		// but won't survive a crash.
		slog.Warn("nostr: failed to persist publish queue item",
			"label", label, "error", err)
	}

	q.mu.Unlock()

	slog.Debug("nostr: enqueued for publish",
		"label", label,
		"relays", len(relays),
		"event_kind", evt.Kind,
	)

	q.signal()
}

// signal wakes the drain loop (non-blocking).
func (q *publishQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// drainOnce processes all items that are due. Returns the time until the
// next item is due, or 0 if nothing is pending.
func (q *publishQueue) drainOnce(ctx context.Context) time.Duration {
	q.mu.Lock()
	due, nextWake := q.collectDueLocked()
	q.mu.Unlock()

	if len(due) == 0 {
		return nextWake
	}

	for _, item := range due {
		q.tryItem(ctx, item)
	}

	q.mu.Lock()
	q.attemptGen++
	q.cleanupLocked(ctx)
	q.mu.Unlock()

	q.notifyFlush()

	// Re-check for the next wake time after cleanup.
	q.mu.Lock()
	_, nextWake = q.collectDueLocked()
	q.mu.Unlock()

	return nextWake
}

// notifyFlush releases flush waiters whose snapshot generation has been
// surpassed by the current attemptGen.
func (q *publishQueue) notifyFlush() {
	q.mu.Lock()
	gen := q.attemptGen

	var remaining []flushWaiter

	for _, w := range q.flushWaiters {
		if gen > w.gen {
			close(w.ch)
		} else {
			remaining = append(remaining, w)
		}
	}

	q.flushWaiters = remaining
	q.mu.Unlock()
}

// collectDueLocked returns items whose NextRetry has passed, and the
// duration until the next earliest retry (0 if nothing pending).
// Must be called with q.mu held.
func (q *publishQueue) collectDueLocked() ([]*publishItem, time.Duration) {
	now := time.Now()

	var due []*publishItem

	var earliest time.Time

	for _, item := range q.items {
		if !item.NextRetry.After(now) {
			due = append(due, item)
		} else if earliest.IsZero() || item.NextRetry.Before(earliest) {
			earliest = item.NextRetry
		}
	}

	if len(due) > 0 || earliest.IsZero() {
		return due, 0
	}

	return nil, time.Until(earliest)
}

// tryItem attempts to publish the event to its remaining failed relays.
func (q *publishQueue) tryItem(ctx context.Context, item *publishItem) {
	var (
		stillFailed []string
		delivered   bool
	)

	for _, relayURL := range item.FailedRelays {
		if err := q.publishToRelay(ctx, relayURL, item.Event); err != nil {
			logLevel := slog.LevelWarn
			if item.Attempts == 0 {
				// First attempt failures are common (e.g. relay down),
				// don't spam warnings for the initial try.
				logLevel = slog.LevelDebug
			}

			slog.Log(ctx, logLevel, "nostr: publish failed",
				"relay", relayURL,
				"event_id", item.Event.ID.Hex(),
				"event_kind", item.Event.Kind,
				"attempt", item.Attempts+1,
				"error", err,
			)

			stillFailed = append(stillFailed, relayURL)
		} else {
			slog.Info("nostr: published",
				"relay", relayURL,
				"event_id", item.Event.ID.Hex(),
				"event_kind", item.Event.Kind,
				"attempt", item.Attempts+1,
			)

			delivered = true
		}
	}

	q.mu.Lock()
	item.Delivered = item.Delivered || delivered
	item.FailedRelays = stillFailed
	item.Attempts++
	item.NextRetry = time.Now().Add(calcBackoff(item.Attempts))

	// Once delivered, remove from persistent storage but keep in memory
	// for best-effort retries to remaining relays.
	if item.Delivered && item.dbID != 0 {
		if err := q.deleteItemLocked(ctx, item.dbID); err != nil {
			slog.Warn("nostr: failed to remove delivered item from db, will retry",
				"event_id", item.Event.ID.Hex(), "error", err)
		} else {
			item.dbID = 0
		}
	}

	q.mu.Unlock()
}

// publishToRelay connects to a relay and publishes an event with a timeout.
func (q *publishQueue) publishToRelay(ctx context.Context, relayURL string, evt gonostr.Event) error {
	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	relay, err := q.pool.EnsureRelay(relayURL)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}

	if err := relay.Publish(pubCtx, evt); err != nil {
		return fmt.Errorf("publishing event: %w", err)
	}

	return nil
}

// cleanupLocked removes items that have no remaining failed relays.
// Must be called with q.mu held.
func (q *publishQueue) cleanupLocked(ctx context.Context) {
	q.items = slices.DeleteFunc(q.items, func(item *publishItem) bool {
		if len(item.FailedRelays) == 0 {
			if item.dbID != 0 {
				if err := q.deleteItemLocked(ctx, item.dbID); err != nil {
					slog.Warn("nostr: failed to remove completed item from db",
						"event_id", item.Event.ID.Hex(), "error", err)

					return false // keep in memory so we retry the delete
				}
			}

			return true
		}

		return false
	})
}

// run drains the queue until ctx is cancelled. It sleeps when idle and
// wakes on new enqueues or when the next retry is due.
func (q *publishQueue) run(ctx context.Context) {
	for {
		nextWake := q.drainOnce(ctx)

		if ctx.Err() != nil {
			return
		}

		if nextWake > 0 {
			// Sleep until the next retry is due or new work arrives.
			timer := time.NewTimer(nextWake)

			select {
			case <-ctx.Done():
				timer.Stop()

				return
			case <-q.wake:
				timer.Stop()
			case <-timer.C:
			}
		} else if q.Len() == 0 {
			// Nothing pending — sleep until woken.
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
			}
		}
		// else: items are due right now, loop immediately.
	}
}

// --- persistence ---

func (q *publishQueue) load(ctx context.Context) error {
	rows, err := q.db.queries.ListPublishQueue(ctx)
	if err != nil {
		return fmt.Errorf("reading publish queue: %w", err)
	}

	for _, row := range rows {
		var evt gonostr.Event
		if err := evt.UnmarshalJSON([]byte(row.EventJson)); err != nil {
			slog.Warn("nostr: skipping corrupt publish queue event", "id", row.ID, "error", err)

			continue
		}

		relays := strings.Split(row.Relays, "\n")

		relays = slices.DeleteFunc(relays, func(s string) bool {
			return strings.TrimSpace(s) == ""
		})
		if len(relays) == 0 {
			continue
		}

		q.items = append(q.items, &publishItem{
			dbID:         row.ID,
			Event:        evt,
			Relays:       relays,
			FailedRelays: relays,
			CreatedAt:    time.Unix(row.CreatedAt, 0),
		})
	}

	if len(q.items) > 0 {
		slog.Info("nostr: loaded publish queue from db", "items", len(q.items))
	}

	return nil
}

// insertItemLocked persists a new item to the database.
// Must be called with q.mu held.
func (q *publishQueue) insertItemLocked(ctx context.Context, item *publishItem) error {
	eventJSON, err := item.Event.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshalling event: %w", err)
	}

	id, err := q.db.queries.InsertPublishItem(ctx, InsertPublishItemParams{
		EventJson: string(eventJSON),
		Relays:    strings.Join(item.Relays, "\n"),
		CreatedAt: item.CreatedAt.Unix(),
	})
	if err != nil {
		return fmt.Errorf("inserting publish queue item: %w", err)
	}

	item.dbID = id

	return nil
}

// deleteItemLocked removes a single item from the database.
// Returns an error if the deletion fails so callers can retain
// the in-memory state for retry.
func (q *publishQueue) deleteItemLocked(ctx context.Context, dbID int64) error {
	if err := q.db.queries.DeletePublishItem(ctx, dbID); err != nil {
		return fmt.Errorf("deleting publish queue item %d: %w", dbID, err)
	}

	return nil
}

// calcBackoff returns the backoff duration for the given attempt number.
func calcBackoff(attempts int) time.Duration {
	d := float64(initialBackoff) * math.Pow(backoffMultiplier, float64(attempts-1))
	if d > float64(maxBackoff) {
		return maxBackoff
	}

	return time.Duration(d)
}

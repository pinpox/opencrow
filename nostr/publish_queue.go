package nostr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/pinpox/opencrow/atomicfile"
)

const (
	publishQueueFile  = ".nostr_publish_queue"
	initialBackoff    = 5 * time.Second
	maxBackoff        = 5 * time.Minute
	backoffMultiplier = 2.0
	publishTimeout    = 15 * time.Second
)

// publishItem is a single event that needs to be published to a set of relays.
// Once at least one relay accepts the event, the item is removed from the
// persistent queue (best-effort retries continue in-memory for remaining relays).
type publishItem struct {
	// Event is the signed Nostr event to publish.
	Event gonostr.Event `json:"event"`
	// Relays is the full set of target relay URLs for this item.
	Relays []string `json:"relays"`
	// FailedRelays tracks which relays still need delivery.
	FailedRelays []string `json:"failed_relays"`
	// Delivered is true once at least one relay accepted the event.
	Delivered bool `json:"delivered"`
	// Attempts counts total publish rounds.
	Attempts int `json:"attempts"`
	// NextRetry is when this item should next be attempted.
	NextRetry time.Time `json:"next_retry"`
	// CreatedAt records when the item was first enqueued.
	CreatedAt time.Time `json:"created_at"`
}

// publishQueue is an outbox that all publishes go through. A background
// goroutine drains it, retrying with exponential backoff. Once at least
// one relay per item accepts the event, the item is removed from the
// persistent store but best-effort retries continue in-memory for
// remaining relays.
type publishQueue struct {
	mu           sync.Mutex
	items        []*publishItem
	flushWaiters []chan struct{} // closed after a drain pass completes
	dataDir      string
	pool         *gonostr.Pool // set via setPool before calling run
	wake         chan struct{} // signals the drain loop that new work arrived
}

// Len returns the number of items in the queue.
func (q *publishQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.items)
}

// Flush blocks until all currently-enqueued items have had at least one
// publish attempt. Intended for tests.
func (q *publishQueue) Flush(ctx context.Context) {
	done := make(chan struct{})

	q.mu.Lock()
	q.flushWaiters = append(q.flushWaiters, done)
	q.mu.Unlock()

	q.signal()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func newPublishQueue(dataDir string) *publishQueue {
	q := &publishQueue{
		dataDir: dataDir,
		wake:    make(chan struct{}, 1),
	}
	q.load()

	return q
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
func (q *publishQueue) enqueue(evt gonostr.Event, relays []string, label string) {
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
	q.saveLocked()
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
		q.notifyFlush()

		return nextWake
	}

	for _, item := range due {
		q.tryItem(ctx, item)
	}

	q.mu.Lock()
	q.cleanupLocked()
	q.saveLocked()
	q.mu.Unlock()

	q.notifyFlush()

	// Re-check for the next wake time after cleanup.
	q.mu.Lock()
	_, nextWake = q.collectDueLocked()
	q.mu.Unlock()

	return nextWake
}

// notifyFlush closes all pending flush waiter channels.
func (q *publishQueue) notifyFlush() {
	q.mu.Lock()
	waiters := q.flushWaiters
	q.flushWaiters = nil
	q.mu.Unlock()

	for _, ch := range waiters {
		close(ch)
	}
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
	var stillFailed []string

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

			item.Delivered = true
		}
	}

	q.mu.Lock()
	item.FailedRelays = stillFailed
	item.Attempts++
	item.NextRetry = time.Now().Add(calcBackoff(item.Attempts))
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
func (q *publishQueue) cleanupLocked() {
	kept := make([]*publishItem, 0, len(q.items))

	for _, item := range q.items {
		if len(item.FailedRelays) == 0 {
			continue
		}

		kept = append(kept, item)
	}

	q.items = kept
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

func (q *publishQueue) load() {
	if q.dataDir == "" {
		return
	}

	data, err := os.ReadFile(filepath.Join(q.dataDir, publishQueueFile))
	if err != nil {
		return
	}

	var items []*publishItem
	if err := json.Unmarshal(data, &items); err != nil {
		slog.Warn("nostr: failed to parse publish queue", "error", err)

		return
	}

	// Only load items that are not yet delivered (need persistent retry).
	for _, item := range items {
		if !item.Delivered && len(item.FailedRelays) > 0 {
			q.items = append(q.items, item)
		}
	}

	if len(q.items) > 0 {
		slog.Info("nostr: loaded publish queue from disk", "items", len(q.items))
	}
}

// saveLocked persists only undelivered items to disk. Items that have been
// delivered to at least one relay are kept in memory only.
// Must be called with q.mu held.
func (q *publishQueue) saveLocked() {
	if q.dataDir == "" {
		return
	}

	// Only persist items where no relay has accepted yet.
	var persistent []*publishItem

	for _, item := range q.items {
		if !item.Delivered {
			persistent = append(persistent, item)
		}
	}

	data, err := json.Marshal(persistent)
	if err != nil {
		slog.Warn("nostr: failed to marshal publish queue", "error", err)

		return
	}

	destPath := filepath.Join(q.dataDir, publishQueueFile)
	if err := atomicfile.Write(destPath, data); err != nil {
		slog.Warn("nostr: failed to save publish queue", "error", err)
	}
}

// calcBackoff returns the backoff duration for the given attempt number.
func calcBackoff(attempts int) time.Duration {
	d := time.Duration(float64(initialBackoff) * math.Pow(backoffMultiplier, float64(attempts-1)))

	return min(d, maxBackoff)
}

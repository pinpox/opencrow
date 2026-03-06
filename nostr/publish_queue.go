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
)

const (
	publishQueueFile    = ".nostr_publish_queue"
	initialBackoff      = 5 * time.Second
	maxBackoff          = 5 * time.Minute
	backoffMultiplier   = 2.0
	publishTimeout      = 15 * time.Second
	retryTickerInterval = 1 * time.Second
)

// publishItem is a single event that needs to be published to a set of relays.
// Items are grouped: once at least one relay in the group accepts the event,
// the item is removed from the persistent queue (best-effort retries continue
// in-memory for the remaining relays).
type publishItem struct {
	// Event is the signed Nostr event to publish.
	Event gonostr.Event `json:"event"`
	// Relays is the full set of target relay URLs for this item.
	Relays []string `json:"relays"`
	// FailedRelays tracks which relays still need delivery. Once empty
	// or at least one relay has succeeded, the item graduates from
	// persistent to best-effort.
	FailedRelays []string `json:"failed_relays"`
	// Delivered is true once at least one relay accepted the event.
	Delivered bool `json:"delivered"`
	// Attempts counts total retry rounds (across all relays).
	Attempts int `json:"attempts"`
	// NextRetry is when this item should next be retried.
	NextRetry time.Time `json:"next_retry"`
	// CreatedAt records when the item was first enqueued.
	CreatedAt time.Time `json:"created_at"`
}

// publishQueue persists outgoing events that failed to publish to their
// target relays. A background goroutine retries them with exponential
// backoff. Once at least one relay per item accepts the event, the item
// is removed from the persistent store but retries continue in-memory
// for remaining relays.
type publishQueue struct {
	mu      sync.Mutex
	items   []*publishItem
	dataDir string
	pool    *gonostr.Pool
}

// Len returns the number of items in the queue.
func (q *publishQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.items)
}

// PendingRelays returns the total number of relay deliveries still pending.
func (q *publishQueue) PendingRelays() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	n := 0

	for _, item := range q.items {
		n += len(item.FailedRelays)
	}

	return n
}

func newPublishQueue(dataDir string, pool *gonostr.Pool) *publishQueue {
	q := &publishQueue{
		dataDir: dataDir,
		pool:    pool,
	}
	q.load()

	return q
}

// enqueue adds an event that failed to publish on one or more relays.
// successRelays are relays that already accepted the event (if any).
func (q *publishQueue) enqueue(evt gonostr.Event, allRelays []string, failedRelays []string) {
	if len(failedRelays) == 0 {
		return
	}

	delivered := len(failedRelays) < len(allRelays)

	item := &publishItem{
		Event:        evt,
		Relays:       allRelays,
		FailedRelays: failedRelays,
		Delivered:    delivered,
		Attempts:     1,
		NextRetry:    time.Now().Add(initialBackoff),
		CreatedAt:    time.Now(),
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.items = append(q.items, item)
	q.saveLocked()
}

// retryOnce processes all items that are due for retry. Returns the number of
// items that were retried.
func (q *publishQueue) retryOnce(ctx context.Context) int {
	q.mu.Lock()
	due := q.collectDueItemsLocked()
	q.mu.Unlock()

	if len(due) == 0 {
		return 0
	}

	retried := 0

	for _, item := range due {
		q.retryItem(ctx, item)

		retried++
	}

	q.mu.Lock()
	q.cleanupLocked()
	q.saveLocked()
	q.mu.Unlock()

	return retried
}

// collectDueItemsLocked returns items whose NextRetry has passed.
// Must be called with q.mu held.
func (q *publishQueue) collectDueItemsLocked() []*publishItem {
	now := time.Now()

	var due []*publishItem

	for _, item := range q.items {
		if !item.NextRetry.After(now) {
			due = append(due, item)
		}
	}

	return due
}

// retryItem attempts to publish the event to its remaining failed relays.
func (q *publishQueue) retryItem(ctx context.Context, item *publishItem) {
	var stillFailed []string

	for _, relayURL := range item.FailedRelays {
		if err := q.publishToRelay(ctx, relayURL, item.Event); err != nil {
			slog.Warn("nostr: queue retry failed",
				"relay", relayURL,
				"event_id", item.Event.ID.Hex(),
				"event_kind", item.Event.Kind,
				"attempts", item.Attempts,
				"error", err,
			)

			stillFailed = append(stillFailed, relayURL)
		} else {
			slog.Info("nostr: queue retry succeeded",
				"relay", relayURL,
				"event_id", item.Event.ID.Hex(),
				"event_kind", item.Event.Kind,
				"attempts", item.Attempts,
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
			// Fully done — all relays accepted or no relays left.
			continue
		}

		kept = append(kept, item)
	}

	q.items = kept
}

// runRetryLoop runs the retry loop until ctx is cancelled.
func (q *publishQueue) runRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(retryTickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.retryOnce(ctx)
		}
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

	if err := os.MkdirAll(q.dataDir, 0o755); err != nil {
		slog.Warn("nostr: failed to create dir for publish queue", "error", err)

		return
	}

	data, err := json.Marshal(persistent)
	if err != nil {
		slog.Warn("nostr: failed to marshal publish queue", "error", err)

		return
	}

	tmpFile, err := os.CreateTemp(q.dataDir, ".publish_queue_*.tmp")
	if err != nil {
		slog.Warn("nostr: failed to create temp file for publish queue", "error", err)

		return
	}

	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		slog.Warn("nostr: failed to write publish queue temp file", "error", err)

		return
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		slog.Warn("nostr: failed to close publish queue temp file", "error", err)

		return
	}

	finalPath := filepath.Join(q.dataDir, publishQueueFile)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		slog.Warn("nostr: failed to rename publish queue into place", "error", err)
	}
}

// calcBackoff returns the backoff duration for the given attempt number.
func calcBackoff(attempts int) time.Duration {
	d := time.Duration(float64(initialBackoff) * math.Pow(backoffMultiplier, float64(attempts-1)))

	return min(d, maxBackoff)
}

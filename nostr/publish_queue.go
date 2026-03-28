package nostr

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"
)

// Timeouts and TTLs.
const (
	publishTimeout = 15 * time.Second

	// ttlUndelivered is the hard deadline for an event that no relay has
	// accepted yet. Past this we log WARN and drop it — the message is
	// lost. 24h gives plenty of slack for transient outages while still
	// bounding DB growth.
	ttlUndelivered = 24 * time.Hour

	// ttlDelivered is the best-effort window for publishing to remaining
	// relays after at least one has accepted the event. The event is
	// already on the Nostr network and will propagate via gossip, so
	// extra relays are just redundancy.
	ttlDelivered = 10 * time.Minute
)

// Circuit-breaker tuning.
const (
	// breakerThreshold is how many consecutive connect failures trip the breaker.
	breakerThreshold = 3

	// rejectThreshold is how many times a relay may reject a specific
	// event before we stop offering it. Prevents one poison event from
	// being retried forever against a relay that will never accept it.
	rejectThreshold = 3

	// breakerBaseCooldown is the initial open duration. Doubles on each
	// subsequent trip up to breakerMaxCooldown.
	breakerBaseCooldown = 30 * time.Second
	breakerMaxCooldown  = 30 * time.Minute
)

// publishItem is a single event that needs to be published to a set of relays.
// It carries no retry state of its own — retry timing is owned by the per-relay
// circuit breaker. An item is persisted to SQLite only while !Delivered; once
// any relay accepts it the row is deleted and remaining relays are best-effort
// in memory.
type publishItem struct {
	// dbID is the database row ID (0 once deleted / never persisted).
	dbID int64
	// Event is the signed Nostr event to publish.
	Event gonostr.Event
	// Pending is the set of relay URLs that have not yet confirmed the event.
	Pending map[string]struct{}
	// Delivered is true once at least one relay accepted the event.
	Delivered bool
	// CreatedAt records when the item was first enqueued. Persisted, so
	// age-based TTL works correctly across restarts.
	CreatedAt time.Time
	// rejects counts per-relay publish rejections for this event. Once a
	// relay hits rejectThreshold we drop it from Pending without tripping
	// the shared breaker — the relay is healthy, it just won't take this
	// event (content filter, auth policy, etc.).
	rejects map[string]int
}

// publishQueue is an outbox that all publishes go through. A background
// goroutine drains it, grouping work by relay so a single dead relay
// trips a shared circuit breaker instead of N independent per-item
// backoff timers.
type publishQueue struct {
	mu       sync.Mutex
	items    []*publishItem
	breakers map[string]*relayBreaker

	// flushWaiters is a test hook: released after a drain pass.
	flushWaiters []flushWaiter
	attemptGen   uint64

	db   *DB
	pool *gonostr.Pool // set via setPool before calling run
	wake chan struct{} // signals the drain loop that new work arrived
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
// the call. Intended for tests.
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
		db:       db,
		breakers: make(map[string]*relayBreaker),
		wake:     make(chan struct{}, 1),
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

	if q.Len() > 0 {
		q.signal()
	}
}

// enqueue adds an event to be published to the given relays.
func (q *publishQueue) enqueue(ctx context.Context, evt gonostr.Event, relays []string, label string) {
	if len(relays) == 0 {
		return
	}

	pending := make(map[string]struct{}, len(relays))
	for _, r := range relays {
		pending[r] = struct{}{}
	}

	item := &publishItem{
		Event:     evt,
		Pending:   pending,
		CreatedAt: time.Now(),
	}

	q.mu.Lock()
	q.items = append(q.items, item)

	if err := q.insertItemLocked(ctx, item, relays); err != nil {
		// Stays in memory for this process lifetime but won't survive a crash.
		slog.Warn("nostr: failed to persist publish queue item",
			"label", label, "error", err)
	}
	q.mu.Unlock()

	slog.Debug("nostr: enqueued for publish",
		"label", label, "relays", len(relays), "event_kind", evt.Kind)

	q.signal()
}

// signal wakes the drain loop (non-blocking).
func (q *publishQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// run drains the queue until ctx is cancelled. Sleeps when idle, wakes on
// new enqueues or when a breaker cooldown expires.
func (q *publishQueue) run(ctx context.Context) {
	for {
		nextWake := q.drainOnce(ctx)

		if ctx.Err() != nil {
			return
		}

		if nextWake > 0 {
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
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
			}
		}
		// else: work remains and no sleep needed, loop immediately.
	}
}

// drainOnce processes all items against relays whose breaker permits
// traffic. Returns how long to sleep before the next useful wakeup.
func (q *publishQueue) drainOnce(ctx context.Context) time.Duration {
	now := time.Now()

	q.mu.Lock()
	q.evictLocked(ctx, now)
	work, probes := q.collectWorkLocked(now)
	q.mu.Unlock()

	for relay, items := range work {
		_, isProbe := probes[relay]

		if ctx.Err() != nil || !q.publishBatch(ctx, relay, items, isProbe) {
			// Aborted before resolving this relay's breaker. Roll back
			// any halfOpen transition so the relay isn't stuck in a
			// state where allow() always returns deny.
			if isProbe {
				q.mu.Lock()
				if b := q.breakers[relay]; b != nil && b.state == halfOpen {
					b.state = open
				}
				q.mu.Unlock()
			}

			if ctx.Err() != nil {
				break
			}
		}
	}

	q.mu.Lock()
	q.attemptGen++
	q.evictLocked(ctx, time.Now())
	q.gcBreakersLocked()
	next := q.nextWakeLocked(time.Now())
	q.mu.Unlock()

	q.notifyFlush()

	return next
}

// collectWorkLocked groups pending items by relay, consulting each relay's
// circuit breaker. Half-open breakers get at most one item (the probe).
// Must be called with q.mu held.
func (q *publishQueue) collectWorkLocked(now time.Time) (map[string][]*publishItem, map[string]struct{}) {
	work := make(map[string][]*publishItem)
	probes := make(map[string]struct{})

	for _, item := range q.items {
		for relay := range item.Pending {
			b := q.breakerLocked(relay)

			switch b.allow(now) {
			case allowAll:
				work[relay] = append(work[relay], item)
			case allowProbe:
				if _, already := probes[relay]; !already {
					work[relay] = append(work[relay], item)
					probes[relay] = struct{}{}
				}
			case deny:
				// skip
			}
		}
	}

	return work, probes
}

// publishBatch sends all items to one relay over a single connection.
// Returns false if it bailed out without resolving the breaker (context
// cancelled mid-batch), so the caller can roll back a halfOpen transition.
func (q *publishQueue) publishBatch(ctx context.Context, relayURL string, items []*publishItem, probe bool) bool {
	conn, err := q.pool.EnsureRelay(relayURL)
	if err != nil {
		slog.Log(ctx, probeLevel(probe), "nostr: relay connect failed",
			"relay", relayURL, "error", err)

		q.mu.Lock()
		q.breakerLocked(relayURL).onFailure(time.Now())
		q.mu.Unlock()

		return true
	}

	var anyOK bool

	for _, item := range items {
		if ctx.Err() != nil {
			return false // shutdown; breaker left unresolved
		}

		// Per-event timeout so large batches don't starve the tail.
		pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		err := conn.Publish(pubCtx, item.Event)

		cancel()

		if err != nil {
			q.recordReject(ctx, item, relayURL, err, probe)

			continue
		}

		anyOK = true

		slog.Info("nostr: published",
			"relay", relayURL,
			"event_id", item.Event.ID.Hex(),
			"event_kind", item.Event.Kind,
		)

		q.mu.Lock()
		delete(item.Pending, relayURL)

		if !item.Delivered {
			item.Delivered = true

			if item.dbID != 0 {
				if err := q.deleteItemLocked(ctx, item.dbID); err != nil {
					slog.Warn("nostr: failed to remove delivered item from db",
						"event_id", item.Event.ID.Hex(), "error", err)
				} else {
					item.dbID = 0
				}
			}
		}
		q.mu.Unlock()
	}

	// Breaker tracks relay connectivity only. We reached the relay, so
	// mark it healthy regardless of per-event outcomes — otherwise one
	// event the relay refuses (content policy, wrong kind) would open
	// the breaker and block every other event.
	q.mu.Lock()
	q.breakerLocked(relayURL).onSuccess()
	q.mu.Unlock()

	_ = anyOK // kept for potential future metrics

	return true
}

// recordReject bumps the per-(item,relay) rejection counter and drops the
// relay from the item once it's clear the relay will never accept this event.
func (q *publishQueue) recordReject(ctx context.Context, item *publishItem, relayURL string, err error, probe bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if item.rejects == nil {
		item.rejects = make(map[string]int)
	}

	item.rejects[relayURL]++
	n := item.rejects[relayURL]

	slog.Log(ctx, probeLevel(probe), "nostr: publish rejected",
		"relay", relayURL,
		"event_id", item.Event.ID.Hex(),
		"event_kind", item.Event.Kind,
		"rejects", n,
		"error", err,
	)

	if n >= rejectThreshold {
		delete(item.Pending, relayURL)
		delete(item.rejects, relayURL)

		slog.Warn("nostr: relay keeps rejecting event, dropping from target set",
			"relay", relayURL,
			"event_id", item.Event.ID.Hex(),
			"rejects", n,
		)
	}
}

// evictLocked drops items that have completed or exceeded their TTL.
// Must be called with q.mu held.
func (q *publishQueue) evictLocked(ctx context.Context, now time.Time) {
	q.items = slices.DeleteFunc(q.items, func(item *publishItem) bool {
		age := now.Sub(item.CreatedAt)

		switch {
		case len(item.Pending) == 0:
			// Fully delivered. dbID should already be 0 but be defensive.
			if item.dbID != 0 {
				_ = q.deleteItemLocked(ctx, item.dbID)
			}

			return true

		case item.Delivered && age > ttlDelivered:
			// On the network already; give up on stragglers.
			slog.Debug("nostr: dropping best-effort retries",
				"event_id", item.Event.ID.Hex(),
				"age", age.Round(time.Second),
				"pending_relays", slices.Collect(maps.Keys(item.Pending)),
			)

			return true

		case !item.Delivered && age > ttlUndelivered:
			// Data loss. Shout about it.
			slog.Warn("nostr: DROPPING undelivered event past TTL",
				"event_id", item.Event.ID.Hex(),
				"age", age.Round(time.Second),
				"pending_relays", slices.Collect(maps.Keys(item.Pending)),
			)

			if item.dbID != 0 {
				if err := q.deleteItemLocked(ctx, item.dbID); err != nil {
					slog.Warn("nostr: failed to delete expired item from db",
						"event_id", item.Event.ID.Hex(), "error", err)

					return false // keep so we retry the delete
				}
			}

			return true
		}

		return false
	})
}

// nextWakeLocked returns how long until the next breaker re-opens for
// probing, or 0 if nothing is pending. Must be called with q.mu held.
func (q *publishQueue) nextWakeLocked(now time.Time) time.Duration {
	if len(q.items) == 0 {
		return 0
	}

	// Which relays do we still care about?
	wanted := make(map[string]struct{})

	for _, item := range q.items {
		for r := range item.Pending {
			wanted[r] = struct{}{}
		}
	}

	var earliest time.Time

	for relay := range wanted {
		b := q.breakerLocked(relay)
		if b.state != open {
			// Closed or half-open means work is possible now — but we
			// just drained, so if it's still pending it means half-open
			// probe is in flight or we're mid-loop. Let the caller
			// decide via Len(); return 0 means "don't sleep long".
			return breakerBaseCooldown // conservative short nap
		}

		if earliest.IsZero() || b.nextProbe.Before(earliest) {
			earliest = b.nextProbe
		}
	}

	if earliest.IsZero() {
		return 0
	}

	d := earliest.Sub(now)
	if d < 0 {
		return 0
	}

	return d
}

// notifyFlush releases test waiters whose generation has been surpassed.
func (q *publishQueue) notifyFlush() {
	q.mu.Lock()
	defer q.mu.Unlock()

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
}

// --- circuit breaker ---

type breakerState int

const (
	closed breakerState = iota
	open
	halfOpen
)

type allowResult int

const (
	deny allowResult = iota
	allowProbe
	allowAll
)

// relayBreaker is a classic three-state circuit breaker tracking one relay's
// health. It owns all retry timing — items have none. Not persisted: on
// restart we re-probe everything, which is cheap and self-healing.
type relayBreaker struct {
	state     breakerState
	failures  int
	cooldown  time.Duration
	nextProbe time.Time
}

func (b *relayBreaker) allow(now time.Time) allowResult {
	switch b.state {
	case closed:
		return allowAll
	case open:
		if !now.Before(b.nextProbe) {
			b.state = halfOpen

			return allowProbe
		}

		return deny
	case halfOpen:
		return deny // a probe is already scheduled this pass
	}

	return deny
}

func (b *relayBreaker) onSuccess() {
	if b.state != closed {
		slog.Info("nostr: relay recovered", "cooldown_was", b.cooldown)
	}

	b.state = closed
	b.failures = 0
	b.cooldown = breakerBaseCooldown
}

func (b *relayBreaker) onFailure(now time.Time) {
	b.failures++

	wasOpen := b.state == open || b.state == halfOpen

	if b.state == halfOpen || b.failures >= breakerThreshold {
		if wasOpen {
			// Re-trip after a failed probe: escalate.
			b.cooldown = min(b.cooldown*2, breakerMaxCooldown)
		} else {
			// First trip from closed: start at base.
			b.cooldown = breakerBaseCooldown
		}

		b.state = open
		b.nextProbe = now.Add(b.cooldown)

		slog.Warn("nostr: relay breaker open",
			"failures", b.failures, "cooldown", b.cooldown)
	}
}

// breakerLocked returns the breaker for a relay, creating it if needed.
// Must be called with q.mu held.
func (q *publishQueue) breakerLocked(relay string) *relayBreaker {
	b, ok := q.breakers[relay]
	if !ok {
		b = &relayBreaker{state: closed}
		q.breakers[relay] = b
	}

	return b
}

// gcBreakersLocked drops breaker entries for relays no item references.
// Prevents unbounded map growth when relay sets change over time.
// Must be called with q.mu held.
func (q *publishQueue) gcBreakersLocked() {
	wanted := make(map[string]struct{})

	for _, item := range q.items {
		for r := range item.Pending {
			wanted[r] = struct{}{}
		}
	}

	for r, b := range q.breakers {
		// Keep open breakers even if no item wants them right now —
		// a new enqueue to the same dead relay should inherit the
		// cooldown, not start fresh.
		if _, ok := wanted[r]; !ok && b.state == closed {
			delete(q.breakers, r)
		}
	}
}

// probeLevel returns Debug for half-open probes (expected to fail often)
// and Warn otherwise.
func probeLevel(probe bool) slog.Level {
	if probe {
		return slog.LevelDebug
	}

	return slog.LevelWarn
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
			slog.Warn("nostr: deleting corrupt publish queue row", "id", row.ID, "error", err)

			if derr := q.db.queries.DeletePublishItem(ctx, row.ID); derr != nil {
				slog.Warn("nostr: failed to delete corrupt row", "id", row.ID, "error", derr)
			}

			continue
		}

		relays := strings.Split(row.Relays, "\n")
		pending := make(map[string]struct{}, len(relays))

		for _, r := range relays {
			if r = strings.TrimSpace(r); r != "" {
				pending[r] = struct{}{}
			}
		}

		if len(pending) == 0 {
			// Row with no relays is useless; clean it up.
			_ = q.db.queries.DeletePublishItem(ctx, row.ID)

			continue
		}

		q.items = append(q.items, &publishItem{
			dbID:      row.ID,
			Event:     evt,
			Pending:   pending,
			CreatedAt: time.Unix(row.CreatedAt, 0),
		})
	}

	if len(q.items) > 0 {
		slog.Info("nostr: loaded publish queue from db", "items", len(q.items))
	}

	return nil
}

// insertItemLocked persists a new item. Must be called with q.mu held.
func (q *publishQueue) insertItemLocked(ctx context.Context, item *publishItem, relays []string) error {
	eventJSON, err := item.Event.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshalling event: %w", err)
	}

	id, err := q.db.queries.InsertPublishItem(ctx, InsertPublishItemParams{
		EventJson: string(eventJSON),
		Relays:    strings.Join(relays, "\n"),
		CreatedAt: item.CreatedAt.Unix(),
	})
	if err != nil {
		return fmt.Errorf("inserting publish queue item: %w", err)
	}

	item.dbID = id

	return nil
}

// deleteItemLocked removes one row. Must be called with q.mu held.
func (q *publishQueue) deleteItemLocked(ctx context.Context, dbID int64) error {
	if err := q.db.queries.DeletePublishItem(ctx, dbID); err != nil {
		return fmt.Errorf("deleting publish queue item %d: %w", dbID, err)
	}

	return nil
}

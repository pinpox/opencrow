package testutil

import (
	"context"
	"iter"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
)

// StartTestRelay spins up an in-process Nostr relay backed by an in-memory
// slice store. It returns the WebSocket URL (ws://...) and a cleanup function.
func StartTestRelay(t *testing.T) (string, func()) {
	t.Helper()

	relay := khatru.NewRelay()

	store := &slicestore.SliceStore{}
	if err := store.Init(); err != nil {
		t.Fatalf("initializing slice store: %v", err)
	}

	// Wrap the store with a mutex because slicestore.QueryEvents and
	// CountEvents are not goroutine-safe (upstream bug). We set the
	// relay hooks manually instead of calling UseEventstore so every
	// store access goes through the lock.
	var mu sync.Mutex

	relay.QueryStored = func(_ context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		mu.Lock()
		// Collect results under the lock, then yield without holding it.
		events := make([]nostr.Event, 0, 500)
		for evt := range store.QueryEvents(filter, 500) {
			events = append(events, evt)
		}
		mu.Unlock()

		return func(yield func(nostr.Event) bool) {
			for _, evt := range events {
				if !yield(evt) {
					return
				}
			}
		}
	}
	relay.Count = func(_ context.Context, filter nostr.Filter) (uint32, error) {
		mu.Lock()
		defer mu.Unlock()

		return store.CountEvents(filter)
	}
	relay.StoreEvent = func(_ context.Context, event nostr.Event) error {
		mu.Lock()
		defer mu.Unlock()

		return store.SaveEvent(event)
	}
	relay.ReplaceEvent = func(_ context.Context, event nostr.Event) error {
		mu.Lock()
		defer mu.Unlock()

		return store.ReplaceEvent(event)
	}
	relay.DeleteEvent = func(_ context.Context, id nostr.ID) error {
		mu.Lock()
		defer mu.Unlock()

		return store.DeleteEvent(id)
	}

	srv := httptest.NewServer(relay)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	return wsURL, srv.Close
}

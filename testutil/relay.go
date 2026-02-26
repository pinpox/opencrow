package testutil

import (
	"net/http/httptest"
	"strings"
	"testing"

	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
)

// StartTestRelay spins up an in-process Nostr relay backed by an in-memory
// slice store. It returns the WebSocket URL (ws://...) and a cleanup function.
func StartTestRelay(t *testing.T) (url string, cleanup func()) {
	t.Helper()

	relay := khatru.NewRelay()

	store := &slicestore.SliceStore{}
	store.Init()

	relay.UseEventstore(store, 500)

	srv := httptest.NewServer(relay)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	return wsURL, srv.Close
}

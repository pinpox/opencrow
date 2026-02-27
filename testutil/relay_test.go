package testutil

import (
	"context"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestStartTestRelay_SmokeTest(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := StartTestRelay(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect to the relay
	relay, err := nostr.RelayConnect(ctx, wsURL, nostr.RelayOptions{})
	if err != nil {
		t.Fatalf("connecting to test relay: %v", err)
	}
	defer relay.Close()

	// Generate a test keypair
	sk := nostr.Generate()
	pk := sk.Public()

	// Publish a kind-1 event
	evt := nostr.Event{
		Kind:      1,
		CreatedAt: nostr.Now(),
		Content:   "hello from test",
		PubKey:    pk,
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("signing event: %v", err)
	}

	if err := relay.Publish(ctx, evt); err != nil {
		t.Fatalf("publishing event: %v", err)
	}

	// Subscribe and verify receipt
	sub, err := relay.Subscribe(ctx, nostr.Filter{
		Kinds:   []nostr.Kind{1},
		Authors: []nostr.PubKey{pk},
	}, nostr.SubscriptionOptions{})
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer sub.Unsub()

	select {
	case received := <-sub.Events:
		if received.Content != "hello from test" {
			t.Errorf("got content %q, want %q", received.Content, "hello from test")
		}

		if received.PubKey != pk {
			t.Errorf("got pubkey %s, want %s", received.PubKey, pk)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

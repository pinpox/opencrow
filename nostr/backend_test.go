package nostr

import (
	"context"
	"sync"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip17"
	"fiatjaf.com/nostr/nip59"
	"github.com/pinpox/opencrow/backend"
	"github.com/pinpox/opencrow/testutil"
)

func TestSendMessage_PublishesGiftWrap(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	// Bot keys
	botSK := gonostr.Generate()
	botPK := botSK.Public()

	// Recipient keys
	recipientSK := gonostr.Generate()
	recipientPK := recipientSK.Public()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	b := newTestBackend(t, botSK, []string{wsURL}, nil)

	// Set up pool and keyer so SendMessage works without Run
	pool := gonostr.NewPool(gonostr.PoolOptions{})
	kr := keyer.NewPlainKeySigner(botSK)
	b.pool = pool
	b.kr = kr

	// Send the message
	b.SendMessage(ctx, recipientPK.Hex(), "hello from bot")

	// Now query the relay for the gift wrap
	events := pool.FetchMany(ctx, []string{wsURL}, gonostr.Filter{
		Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
	}, gonostr.SubscriptionOptions{})

	var found bool

	for ie := range events {
		evt := ie.Event
		if evt.Kind != gonostr.KindGiftWrap {
			continue
		}

		recipientKr := keyer.NewPlainKeySigner(recipientSK)

		rumor, err := nip59.GiftUnwrap(evt,
			func(otherpubkey gonostr.PubKey, ciphertext string) (string, error) {
				return recipientKr.Decrypt(ctx, ciphertext, otherpubkey)
			},
		)
		if err != nil {
			// This might be the "toUs" copy we can't decrypt — skip
			continue
		}

		if rumor.Kind != gonostr.KindDirectMessage {
			t.Errorf("rumor kind = %d, want %d", rumor.Kind, gonostr.KindDirectMessage)
		}

		if rumor.Content != "hello from bot" {
			t.Errorf("rumor content = %q, want %q", rumor.Content, "hello from bot")
		}

		if rumor.PubKey != botPK {
			t.Errorf("rumor pubkey = %s, want %s", rumor.PubKey, botPK)
		}

		found = true

		break
	}

	if !found {
		t.Fatal("no gift wrap found on relay")
	}
}

func TestRun_ReceivesDM(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()

	senderSK := gonostr.Generate()
	senderPK := senderSK.Public()

	var (
		received []backend.Message
		mu       sync.Mutex
	)

	handler := func(_ context.Context, msg backend.Message) {
		mu.Lock()

		received = append(received, msg)
		mu.Unlock()
	}

	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, nil, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := make(chan error, 1)

	go func() { runErr <- b.Run(ctx) }()

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, senderSK, b.keys.PK, "hello bot")

	deadline := time.After(3 * time.Second)

	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()

		if n > 0 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for handler to be called")
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-runErr

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("received %d messages, want 1", len(received))
	}

	if received[0].ConversationID != senderPK.Hex() {
		t.Errorf("ConversationID = %q, want %q", received[0].ConversationID, senderPK.Hex())
	}

	if received[0].Text != "hello bot" {
		t.Errorf("Text = %q, want %q", received[0].Text, "hello bot")
	}
}

func TestRun_DropsDisallowedUser(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()

	allowedSK := gonostr.Generate()
	allowedPK := allowedSK.Public()

	disallowedSK := gonostr.Generate()

	var (
		received []backend.Message
		mu       sync.Mutex
	)

	handler := func(_ context.Context, msg backend.Message) {
		mu.Lock()

		received = append(received, msg)
		mu.Unlock()
	}

	allowedUsers := map[string]struct{}{allowedPK.Hex(): {}}
	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, allowedUsers, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := make(chan error, 1)

	go func() { runErr <- b.Run(ctx) }()

	time.Sleep(300 * time.Millisecond)

	// Send from disallowed user first
	sendTestDM(ctx, t, wsURL, disallowedSK, b.keys.PK, "should be dropped")
	time.Sleep(200 * time.Millisecond)

	// Then from allowed user
	sendTestDM(ctx, t, wsURL, allowedSK, b.keys.PK, "should be received")

	deadline := time.After(3 * time.Second)

	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()

		if n > 0 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for allowed message")
		case <-time.After(50 * time.Millisecond):
		}
	}

	time.Sleep(200 * time.Millisecond)

	cancel()
	<-runErr

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("received %d messages, want 1", len(received))
	}

	if received[0].Text != "should be received" {
		t.Errorf("Text = %q, want %q", received[0].Text, "should be received")
	}
}

func TestSingleActiveConversation(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()

	userASK := gonostr.Generate()
	userAPK := userASK.Public()

	userBSK := gonostr.Generate()

	var (
		received []backend.Message
		mu       sync.Mutex
	)

	handler := func(_ context.Context, msg backend.Message) {
		mu.Lock()

		received = append(received, msg)
		mu.Unlock()
	}

	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, nil, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := make(chan error, 1)

	go func() { runErr <- b.Run(ctx) }()

	time.Sleep(300 * time.Millisecond)

	// A sends first — becomes active
	sendTestDM(ctx, t, wsURL, userASK, b.keys.PK, "from A")
	time.Sleep(300 * time.Millisecond)

	// B sends — should be dropped
	sendTestDM(ctx, t, wsURL, userBSK, b.keys.PK, "from B")
	time.Sleep(300 * time.Millisecond)

	cancel()
	<-runErr

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("received %d messages, want 1", len(received))
	}

	if received[0].ConversationID != userAPK.Hex() {
		t.Errorf("ConversationID = %q, want %q", received[0].ConversationID, userAPK.Hex())
	}
}

func TestResetConversation(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()

	userASK := gonostr.Generate()
	userAPK := userASK.Public()

	userBSK := gonostr.Generate()
	userBPK := userBSK.Public()

	var (
		received []backend.Message
		mu       sync.Mutex
	)

	handler := func(_ context.Context, msg backend.Message) {
		mu.Lock()

		received = append(received, msg)
		mu.Unlock()
	}

	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, nil, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := make(chan error, 1)

	go func() { runErr <- b.Run(ctx) }()

	time.Sleep(300 * time.Millisecond)

	// A sends — becomes active
	sendTestDM(ctx, t, wsURL, userASK, b.keys.PK, "from A")
	time.Sleep(300 * time.Millisecond)

	// Reset A's conversation
	b.ResetConversation(ctx, userAPK.Hex())
	time.Sleep(100 * time.Millisecond)

	// B sends — should be accepted now
	sendTestDM(ctx, t, wsURL, userBSK, b.keys.PK, "from B")

	deadline := time.After(3 * time.Second)

	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()

		if n >= 2 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for second message")
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-runErr

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 2 {
		t.Fatalf("received %d messages, want 2", len(received))
	}

	if received[1].ConversationID != userBPK.Hex() {
		t.Errorf("second message ConversationID = %q, want %q", received[1].ConversationID, userBPK.Hex())
	}
}

func TestSeenRumorsPersistence(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	senderSK := gonostr.Generate()
	sessionDir := t.TempDir()

	var (
		mu       sync.Mutex
		received []backend.Message
	)

	handler := func(_ context.Context, msg backend.Message) {
		mu.Lock()

		received = append(received, msg)
		mu.Unlock()
	}

	b, err := NewBackend(Config{
		PrivateKey:     botSK.Hex(),
		Relays:         []string{wsURL},
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: sessionDir,
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := make(chan error, 1)

	go func() { runErr <- b.Run(ctx) }()

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, senderSK, b.keys.PK, "first message")

	deadline := time.After(3 * time.Second)

	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()

		if n > 0 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out")
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-runErr

	// Verify seen rumors were persisted
	seen := loadSeenRumors(sessionDir, 7*24*time.Hour)
	if len(seen) == 0 {
		t.Fatal("no seen rumors persisted to disk")
	}
}

func TestRestartDropsStaleMessages(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	senderSK := gonostr.Generate()
	sessionDir := t.TempDir()

	var (
		mu       sync.Mutex
		received []backend.Message
	)

	handler := func(_ context.Context, msg backend.Message) {
		mu.Lock()

		received = append(received, msg)
		mu.Unlock()
	}

	// --- First run: receive a message, persist seen rumor IDs ---
	b1, err := NewBackend(Config{
		PrivateKey:     botSK.Hex(),
		Relays:         []string{wsURL},
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: sessionDir,
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()

	runErr1 := make(chan error, 1)

	go func() { runErr1 <- b1.Run(ctx1) }()

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx1, t, wsURL, senderSK, b1.keys.PK, "old message")

	deadline := time.After(3 * time.Second)

	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()

		if n > 0 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for first message")
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel1()
	<-runErr1

	mu.Lock()
	if len(received) != 1 || received[0].Text != "old message" {
		t.Fatalf("first run: got %d messages, want 1 with 'old message'", len(received))
	}

	received = nil
	mu.Unlock()

	// --- Second run: same sessionDir, "old message" is still on the relay ---
	// It must NOT be delivered again.
	b2, err := NewBackend(Config{
		PrivateKey:     botSK.Hex(),
		Relays:         []string{wsURL},
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: sessionDir,
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	runErr2 := make(chan error, 1)

	go func() { runErr2 <- b2.Run(ctx2) }()

	time.Sleep(300 * time.Millisecond)

	// Send a new message — this one should be delivered
	sendTestDM(ctx2, t, wsURL, senderSK, b2.keys.PK, "new message")

	deadline = time.After(3 * time.Second)

	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()

		if n > 0 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for new message")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Give extra time for any stale duplicates to arrive
	time.Sleep(500 * time.Millisecond)

	cancel2()
	<-runErr2

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("second run: got %d messages, want 1", len(received))
	}

	if received[0].Text != "new message" {
		t.Errorf("second run: got %q, want %q", received[0].Text, "new message")
	}
}

// --- test helpers ---

func newTestBackend(t *testing.T, sk gonostr.SecretKey, relays []string, allowedUsers map[string]struct{}) *Backend {
	t.Helper()

	return newTestBackendWithHandler(t, sk, relays, allowedUsers, func(context.Context, backend.Message) {})
}

func newTestBackendWithHandler(t *testing.T, sk gonostr.SecretKey, relays []string, allowedUsers map[string]struct{}, handler backend.MessageHandler) *Backend {
	t.Helper()

	if allowedUsers == nil {
		allowedUsers = make(map[string]struct{})
	}

	b, err := NewBackend(Config{
		PrivateKey:     sk.Hex(),
		Relays:         relays,
		AllowedUsers:   allowedUsers,
		SessionBaseDir: t.TempDir(),
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// sendTestDM sends a NIP-17 gift-wrapped DM from sender to recipient via the relay.
func sendTestDM(ctx context.Context, t *testing.T, wsURL string, senderSK gonostr.SecretKey, recipientPK gonostr.PubKey, content string) {
	t.Helper()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	kr := keyer.NewPlainKeySigner(senderSK)

	_, toThem, err := nip17.PrepareMessage(ctx, content, nil, kr, recipientPK, nil)
	if err != nil {
		t.Fatalf("preparing DM: %v", err)
	}

	relay, err := pool.EnsureRelay(wsURL)
	if err != nil {
		t.Fatalf("connecting to relay: %v", err)
	}

	if err := relay.Publish(ctx, toThem); err != nil {
		t.Fatalf("publishing gift wrap: %v", err)
	}
}

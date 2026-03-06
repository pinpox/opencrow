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

	botSK := gonostr.Generate()
	recipientSK := gonostr.Generate()
	recipientPK := recipientSK.Public()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	b := newTestBackend(t, botSK, []string{wsURL}, nil)

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	kr := keyer.NewPlainKeySigner(botSK)
	b.pool = pool
	b.kr = kr

	b.SendMessage(ctx, recipientPK.Hex(), "hello from bot")

	events := pool.FetchMany(ctx, []string{wsURL}, gonostr.Filter{
		Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
	}, gonostr.SubscriptionOptions{})

	recipientKr := keyer.NewPlainKeySigner(recipientSK)
	rumor := findGiftWrapRumor(ctx, t, events, recipientKr)

	if rumor.Kind != gonostr.KindDirectMessage {
		t.Errorf("rumor kind = %d, want %d", rumor.Kind, gonostr.KindDirectMessage)
	}

	if rumor.Content != "hello from bot" {
		t.Errorf("rumor content = %q, want %q", rumor.Content, "hello from bot")
	}

	if rumor.PubKey != botSK.Public() {
		t.Errorf("rumor pubkey = %s, want %s", rumor.PubKey, botSK.Public())
	}
}

// findGiftWrapRumor unwraps the first decryptable gift wrap from the event stream.
func findGiftWrapRumor(ctx context.Context, t *testing.T, events <-chan gonostr.RelayEvent, kr gonostr.Keyer) gonostr.Event {
	t.Helper()

	for ie := range events {
		if ie.Kind != gonostr.KindGiftWrap {
			continue
		}

		rumor, err := nip59.GiftUnwrap(ie.Event,
			func(otherpubkey gonostr.PubKey, ciphertext string) (string, error) {
				return kr.Decrypt(ctx, ciphertext, otherpubkey)
			},
		)
		if err != nil {
			continue
		}

		return rumor
	}

	t.Fatal("no decryptable gift wrap found on relay")

	return gonostr.Event{}
}

func TestRun_ReceivesDM(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	senderSK := gonostr.Generate()

	c := &messageCollector{}
	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, nil, c.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, senderSK, b.keys.PK, "hello bot")
	waitForMessages(t, c, 1)

	cancel()
	<-runErr

	msgs := c.get()
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1", len(msgs))
	}

	if msgs[0].ConversationID != senderSK.Public().Hex() {
		t.Errorf("ConversationID = %q, want %q", msgs[0].ConversationID, senderSK.Public().Hex())
	}

	if msgs[0].Text != "hello bot" {
		t.Errorf("Text = %q, want %q", msgs[0].Text, "hello bot")
	}
}

func TestRun_DropsDisallowedUser(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	allowedSK := gonostr.Generate()
	disallowedSK := gonostr.Generate()

	c := &messageCollector{}
	allowedUsers := map[string]struct{}{allowedSK.Public().Hex(): {}}
	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, allowedUsers, c.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, disallowedSK, b.keys.PK, "should be dropped")
	time.Sleep(200 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, allowedSK, b.keys.PK, "should be received")
	waitForMessages(t, c, 1)
	time.Sleep(200 * time.Millisecond)

	cancel()
	<-runErr

	msgs := c.get()
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1", len(msgs))
	}

	if msgs[0].Text != "should be received" {
		t.Errorf("Text = %q, want %q", msgs[0].Text, "should be received")
	}
}

func TestSingleActiveConversation(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	userASK := gonostr.Generate()
	userBSK := gonostr.Generate()

	c := &messageCollector{}
	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, nil, c.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, userASK, b.keys.PK, "from A")
	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, userBSK, b.keys.PK, "from B")
	time.Sleep(300 * time.Millisecond)

	cancel()
	<-runErr

	msgs := c.get()
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1", len(msgs))
	}

	if msgs[0].ConversationID != userASK.Public().Hex() {
		t.Errorf("ConversationID = %q, want %q", msgs[0].ConversationID, userASK.Public().Hex())
	}
}

func TestResetConversation(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	userASK := gonostr.Generate()
	userBSK := gonostr.Generate()

	c := &messageCollector{}
	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, nil, c.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, userASK, b.keys.PK, "from A")
	waitForMessages(t, c, 1)

	b.ResetConversation(ctx, userASK.Public().Hex())
	time.Sleep(100 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, userBSK, b.keys.PK, "from B")
	waitForMessages(t, c, 2)

	cancel()
	<-runErr

	msgs := c.get()
	if len(msgs) != 2 {
		t.Fatalf("received %d messages, want 2", len(msgs))
	}

	if msgs[1].ConversationID != userBSK.Public().Hex() {
		t.Errorf("second message ConversationID = %q, want %q", msgs[1].ConversationID, userBSK.Public().Hex())
	}
}

func TestSeenRumorsPersistence(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	senderSK := gonostr.Generate()
	sessionDir := t.TempDir()

	c := &messageCollector{}

	b, err := NewBackend(Config{
		PrivateKey:     botSK.Hex(),
		Relays:         []string{wsURL},
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: sessionDir,
	}, c.handler)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, senderSK, b.keys.PK, "first message")
	waitForMessages(t, c, 1)

	cancel()
	<-runErr

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

	c := &messageCollector{}

	// --- First run: receive a message, persist seen rumor IDs ---
	b1 := newTestBackendWithSessionDir(t, botSK, []string{wsURL}, sessionDir, c.handler)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()

	runErr1 := runBackendAsync(ctx1, b1)

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx1, t, wsURL, senderSK, b1.keys.PK, "old message")
	waitForMessages(t, c, 1)

	cancel1()
	<-runErr1

	msgs := c.get()
	if len(msgs) != 1 || msgs[0].Text != "old message" {
		t.Fatalf("first run: got %d messages, want 1 with 'old message'", len(msgs))
	}

	c.reset()

	// --- Second run: same sessionDir, "old message" is still on the relay ---
	b2 := newTestBackendWithSessionDir(t, botSK, []string{wsURL}, sessionDir, c.handler)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	runErr2 := runBackendAsync(ctx2, b2)

	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx2, t, wsURL, senderSK, b2.keys.PK, "new message")
	waitForMessages(t, c, 1)
	time.Sleep(500 * time.Millisecond)

	cancel2()
	<-runErr2

	msgs = c.get()
	if len(msgs) != 1 {
		t.Fatalf("second run: got %d messages, want 1", len(msgs))
	}

	if msgs[0].Text != "new message" {
		t.Errorf("second run: got %q, want %q", msgs[0].Text, "new message")
	}
}

func TestRun_SubscribesAllowedUserDMRelays(t *testing.T) {
	t.Parallel()

	// Two separate relays: bot is configured with relayA only.
	// Sender publishes their kind 10050 listing relayB, then sends a DM
	// to relayB. Bot should discover relayB and receive the message.
	relayA, cleanupA := testutil.StartTestRelay(t)
	defer cleanupA()

	relayB, cleanupB := testutil.StartTestRelay(t)
	defer cleanupB()

	botSK := gonostr.Generate()
	senderSK := gonostr.Generate()
	senderPK := senderSK.Public()

	// Publish sender's DM relay list (kind 10050) on relayA so the bot can discover it.
	publishDMRelayListEvent(t, relayA, senderSK, []string{relayB})

	allowed := map[string]struct{}{senderPK.Hex(): {}}

	mc := &messageCollector{}
	b := newTestBackendWithHandler(t, botSK, []string{relayA}, allowed, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)
	time.Sleep(500 * time.Millisecond)

	// Send DM to relayB only — bot must have discovered it from the 10050.
	sendTestDM(ctx, t, relayB, senderSK, b.keys.PK, "hello from relayB")

	waitForMessages(t, mc, 1)
	cancel()
	<-runErr

	msgs := mc.get()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Text != "hello from relayB" {
		t.Errorf("text = %q, want %q", msgs[0].Text, "hello from relayB")
	}
}

func TestSendReaction_PublishesGiftWrappedReaction(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	senderSK := gonostr.Generate()

	c := &messageCollector{}
	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, nil, c.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)
	time.Sleep(300 * time.Millisecond)

	sendTestDM(ctx, t, wsURL, senderSK, b.keys.PK, "hello bot")
	waitForMessages(t, c, 1)

	// Give the async reaction goroutine time to publish.
	time.Sleep(500 * time.Millisecond)

	cancel()
	<-runErr

	// Fetch all gift wraps from the relay and find the reaction.
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fetchCancel()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	events := pool.FetchMany(fetchCtx, []string{wsURL}, gonostr.Filter{
		Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
	}, gonostr.SubscriptionOptions{})

	senderKr := keyer.NewPlainKeySigner(senderSK)

	var foundReaction bool
	for ie := range events {
		rumor, err := nip59.GiftUnwrap(ie.Event,
			func(otherpubkey gonostr.PubKey, ciphertext string) (string, error) {
				return senderKr.Decrypt(fetchCtx, ciphertext, otherpubkey)
			},
		)
		if err != nil {
			continue
		}
		if rumor.Kind != gonostr.KindReaction {
			continue
		}

		foundReaction = true

		if rumor.Content != "👍" {
			t.Errorf("reaction content = %q, want %q", rumor.Content, "👍")
		}
		if rumor.PubKey != botSK.Public() {
			t.Errorf("reaction pubkey = %s, want %s", rumor.PubKey, botSK.Public())
		}

		// Verify e tag references the original DM rumor.
		var hasETag, hasPTag, hasKTag bool
		for _, tag := range rumor.Tags {
			if len(tag) >= 2 {
				switch tag[0] {
				case "e":
					hasETag = true
				case "p":
					if tag[1] == senderSK.Public().Hex() {
						hasPTag = true
					} else {
						t.Errorf("p tag = %q, want %q", tag[1], senderSK.Public().Hex())
					}
				case "k":
					if tag[1] == "14" {
						hasKTag = true
					} else {
						t.Errorf("k tag = %q, want %q", tag[1], "14")
					}
				}
			}
		}
		if !hasETag {
			t.Error("reaction missing e tag")
		}
		if !hasPTag {
			t.Error("reaction missing p tag")
		}
		if !hasKTag {
			t.Error("reaction missing k tag")
		}

		break
	}

	if !foundReaction {
		t.Fatal("no reaction gift wrap found on relay")
	}
}

func TestSendReaction_DisallowedUserNoReaction(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	allowedSK := gonostr.Generate()
	disallowedSK := gonostr.Generate()

	c := &messageCollector{}
	allowedUsers := map[string]struct{}{allowedSK.Public().Hex(): {}}
	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, allowedUsers, c.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)
	time.Sleep(300 * time.Millisecond)

	// Send DM from disallowed user — should not trigger a reaction.
	sendTestDM(ctx, t, wsURL, disallowedSK, b.keys.PK, "should be dropped")
	time.Sleep(500 * time.Millisecond)

	cancel()
	<-runErr

	// Fetch all gift wraps and try to unwrap as the disallowed user.
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fetchCancel()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	events := pool.FetchMany(fetchCtx, []string{wsURL}, gonostr.Filter{
		Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
	}, gonostr.SubscriptionOptions{})

	disallowedKr := keyer.NewPlainKeySigner(disallowedSK)

	for ie := range events {
		rumor, err := nip59.GiftUnwrap(ie.Event,
			func(otherpubkey gonostr.PubKey, ciphertext string) (string, error) {
				return disallowedKr.Decrypt(fetchCtx, ciphertext, otherpubkey)
			},
		)
		if err != nil {
			continue
		}
		if rumor.Kind == gonostr.KindReaction {
			t.Fatal("reaction was sent for disallowed user, expected none")
		}
	}
}

func TestRun_ReplyThreadingPrependsMarker(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	senderSK := gonostr.Generate()

	c := &messageCollector{}
	b := newTestBackendWithHandler(t, botSK, []string{wsURL}, nil, c.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)
	time.Sleep(300 * time.Millisecond)

	// Send a DM with an "e" tag (simulating a reply).
	sendTestDMWithTags(ctx, t, wsURL, senderSK, b.keys.PK, "this is a reply",
		gonostr.Tags{{"e", "original123"}})
	waitForMessages(t, c, 1)

	cancel()
	<-runErr

	msgs := c.get()
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1", len(msgs))
	}

	want := "[nostr:reply-to:original123]\nthis is a reply"
	if msgs[0].Text != want {
		t.Errorf("Text = %q, want %q", msgs[0].Text, want)
	}
}

// --- test helpers ---

// messageCollector collects backend messages in a thread-safe way.
type messageCollector struct {
	mu       sync.Mutex
	messages []backend.Message
}

func (mc *messageCollector) handler(_ context.Context, msg backend.Message) {
	mc.mu.Lock()
	mc.messages = append(mc.messages, msg)
	mc.mu.Unlock()
}

func (mc *messageCollector) count() int {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	return len(mc.messages)
}

func (mc *messageCollector) get() []backend.Message {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	result := make([]backend.Message, len(mc.messages))
	copy(result, mc.messages)

	return result
}

func (mc *messageCollector) reset() {
	mc.mu.Lock()
	mc.messages = nil
	mc.mu.Unlock()
}

// waitForMessages polls until at least n messages are collected or the timeout expires.
func waitForMessages(t *testing.T, c *messageCollector, n int) {
	t.Helper()

	deadline := time.After(3 * time.Second)

	for {
		if c.count() >= n {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d message(s), got %d", n, c.count())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// runBackendAsync starts the backend in a goroutine and returns a channel for the error.
func runBackendAsync(ctx context.Context, b *Backend) <-chan error {
	ch := make(chan error, 1)

	go func() { ch <- b.Run(ctx) }()

	return ch
}

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

func newTestBackendWithSessionDir(t *testing.T, sk gonostr.SecretKey, relays []string, sessionDir string, handler backend.MessageHandler) *Backend {
	t.Helper()

	b, err := NewBackend(Config{
		PrivateKey:     sk.Hex(),
		Relays:         relays,
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: sessionDir,
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// publishDMRelayListEvent publishes a kind 10050 event listing the given relays.
func publishDMRelayListEvent(t *testing.T, publishRelay string, sk gonostr.SecretKey, dmRelays []string) {
	t.Helper()

	tags := make(gonostr.Tags, 0, len(dmRelays))
	for _, r := range dmRelays {
		tags = append(tags, gonostr.Tag{"relay", r})
	}

	evt := gonostr.Event{
		Kind:      gonostr.KindDMRelayList,
		CreatedAt: gonostr.Now(),
		Tags:      tags,
		PubKey:    sk.Public(),
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("signing DM relay list: %v", err)
	}

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := pool.EnsureRelay(publishRelay)
	if err != nil {
		t.Fatalf("connecting to relay: %v", err)
	}
	if err := r.Publish(ctx, evt); err != nil {
		t.Fatalf("publishing DM relay list: %v", err)
	}
}

// sendTestDMWithTags sends a NIP-17 gift-wrapped DM with extra tags on the rumor.
func sendTestDMWithTags(ctx context.Context, t *testing.T, wsURL string, senderSK gonostr.SecretKey, recipientPK gonostr.PubKey, content string, extraTags gonostr.Tags) {
	t.Helper()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	kr := keyer.NewPlainKeySigner(senderSK)

	_, toThem, err := nip17.PrepareMessage(ctx, content, extraTags, kr, recipientPK, nil)
	if err != nil {
		t.Fatalf("preparing DM with tags: %v", err)
	}

	relay, err := pool.EnsureRelay(wsURL)
	if err != nil {
		t.Fatalf("connecting to relay: %v", err)
	}

	if err := relay.Publish(ctx, toThem); err != nil {
		t.Fatalf("publishing gift wrap: %v", err)
	}
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

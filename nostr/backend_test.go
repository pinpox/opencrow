package nostr

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
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

	b := newTestBackend(t, botSK, []string{wsURL}, nil)

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	kr := keyer.NewPlainKeySigner(botSK)
	b.pool = pool
	b.kr = kr
	b.pubQueue.setPool(pool)

	done := make(chan struct{})

	go func() {
		b.pubQueue.run(ctx)
		close(done)
	}()

	// Cancel context and wait for the drain goroutine to finish before
	// the test returns, otherwise it may still write to t.TempDir().
	defer func() {
		cancel()
		<-done
	}()

	b.SendMessage(ctx, recipientPK.Hex(), "hello from bot", "")
	b.pubQueue.Flush(ctx)

	recipientKr := keyer.NewPlainKeySigner(recipientSK)
	rumor := fetchGiftWrapRumor(ctx, t, []string{wsURL}, recipientKr)

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

// fetchGiftWrapRumor polls the relay until a decryptable gift wrap is found.
// FetchMany is a one-shot query that returns only events already indexed at
// subscription time, so a single attempt can miss an event that was just
// published. Retrying avoids this race.
func fetchGiftWrapRumor(ctx context.Context, t *testing.T, relays []string, kr gonostr.Keyer) gonostr.Event {
	t.Helper()

	for {
		pool := gonostr.NewPool(gonostr.PoolOptions{})

		events := pool.FetchMany(ctx, relays, gonostr.Filter{
			Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
		}, gonostr.SubscriptionOptions{})

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

			pool.Close("found")

			return rumor
		}

		pool.Close("retry")

		select {
		case <-ctx.Done():
			t.Fatal("no decryptable gift wrap found on relay")

			return gonostr.Event{}
		case <-time.After(50 * time.Millisecond):
		}
	}
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

	if msgs[0].MessageID == "" {
		t.Error("MessageID is empty, want non-empty rumor ID")
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

	rumor := fetchReactionRumor(t, wsURL, senderSK)

	if rumor.Content != "👍" {
		t.Errorf("reaction content = %q, want %q", rumor.Content, "👍")
	}

	if rumor.PubKey != botSK.Public() {
		t.Errorf("reaction pubkey = %s, want %s", rumor.PubKey, botSK.Public())
	}

	verifyReactionTags(t, rumor, senderSK.Public().Hex())
}

// fetchReactionRumor unwraps gift wraps from the relay and returns the first
// reaction rumor addressed to senderSK. Fails the test if none is found.
func fetchReactionRumor(t *testing.T, wsURL string, senderSK gonostr.SecretKey) gonostr.Event {
	t.Helper()

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fetchCancel()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	events := pool.FetchMany(fetchCtx, []string{wsURL}, gonostr.Filter{
		Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
	}, gonostr.SubscriptionOptions{})

	senderKr := keyer.NewPlainKeySigner(senderSK)

	for ie := range events {
		rumor, err := nip59.GiftUnwrap(ie.Event,
			func(otherpubkey gonostr.PubKey, ciphertext string) (string, error) {
				return senderKr.Decrypt(fetchCtx, ciphertext, otherpubkey)
			},
		)
		if err != nil {
			continue
		}

		if rumor.Kind == gonostr.KindReaction {
			return rumor
		}
	}

	t.Fatal("no reaction gift wrap found on relay")

	return gonostr.Event{} // unreachable
}

// verifyReactionTags checks that a reaction rumor contains the expected e, p, and k tags.
func verifyReactionTags(t *testing.T, rumor gonostr.Event, expectedPubkeyHex string) {
	t.Helper()

	requireTag(t, rumor.Tags, "e", "")
	requireTag(t, rumor.Tags, "p", expectedPubkeyHex)
	requireTag(t, rumor.Tags, "k", "14")
}

// requireTag asserts that tags contain a tag with the given key.
// If wantValue is non-empty the tag's value must also match.
func requireTag(t *testing.T, tags gonostr.Tags, key, wantValue string) {
	t.Helper()

	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == key {
			if wantValue != "" && tag[1] != wantValue {
				t.Errorf("%s tag = %q, want %q", key, tag[1], wantValue)
			}

			return
		}
	}

	t.Errorf("reaction missing %s tag", key)
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

func TestRun_ReplyThreadingSetsReplyToID(t *testing.T) {
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

	if msgs[0].Text != "this is a reply" {
		t.Errorf("Text = %q, want %q", msgs[0].Text, "this is a reply")
	}

	if msgs[0].MessageID == "" {
		t.Error("MessageID is empty, want non-empty rumor ID")
	}

	if msgs[0].ReplyToID != "original123" {
		t.Errorf("ReplyToID = %q, want %q", msgs[0].ReplyToID, "original123")
	}
}

func TestRun_ReceivesFileMessage(t *testing.T) {
	t.Parallel()

	// Serve a fake image so the download succeeds.
	imgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0fake jpeg"))
	}))
	defer imgServer.Close()

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

	fileURL := imgServer.URL + "/QmHash123"
	sendTestFileMessage(ctx, t, wsURL, senderSK, b.keys.PK, fileURL, "image/jpeg")
	waitForMessages(t, c, 1)

	cancel()
	<-runErr

	msgs := c.get()
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1", len(msgs))
	}

	// The message text should contain the local file path, not the URL.
	if strings.Contains(msgs[0].Text, "http") {
		t.Errorf("message still contains URL: %q", msgs[0].Text)
	}

	if !strings.Contains(msgs[0].Text, "attachments/") {
		t.Errorf("message should contain local attachment path, got: %q", msgs[0].Text)
	}
}

func TestRun_ReceivesEncryptedFileMessage(t *testing.T) {
	t.Parallel()

	plaintext := []byte("\xff\xd8\xff\xe0fake jpeg content for encryption test")
	ciphertext, keyHex, nonceHex, oxHex := aesGCMEncrypt(t, plaintext)

	// Serve the encrypted blob.
	imgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(ciphertext)
	}))
	defer imgServer.Close()

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

	fileURL := imgServer.URL + "/encrypted-blob"
	sendTestEncryptedFileMessage(ctx, t, wsURL, senderSK, b.keys.PK, fileURL, "image/jpeg",
		keyHex, nonceHex, oxHex)
	waitForMessages(t, c, 1)

	cancel()
	<-runErr

	msgs := c.get()
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1", len(msgs))
	}

	if !strings.Contains(msgs[0].Text, "attachments/") {
		t.Fatalf("message should contain local attachment path, got: %q", msgs[0].Text)
	}

	// Extract the file path and verify contents are decrypted.
	parts := strings.SplitN(msgs[0].Text, ": ", 2)
	filePath := strings.TrimSuffix(strings.SplitN(parts[1], "]", 2)[0], "]")

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading decrypted file: %v", err)
	}

	if !bytes.Equal(data, plaintext) {
		t.Errorf("decrypted content mismatch: got %d bytes, want %d bytes", len(data), len(plaintext))
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

	return slices.Clone(mc.messages)
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

	b, err := NewBackend(context.Background(), Config{
		PrivateKey:     sk.Hex(),
		Relays:         relays,
		AllowedUsers:   allowedUsers,
		SessionBaseDir: t.TempDir(),
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { b.Close() })

	return b
}

func newTestBackendWithSessionDir(t *testing.T, sk gonostr.SecretKey, relays []string, sessionDir string, handler backend.MessageHandler) *Backend {
	t.Helper()

	b, err := NewBackend(context.Background(), Config{
		PrivateKey:     sk.Hex(),
		Relays:         relays,
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: sessionDir,
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { b.Close() })

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
	sendTestDMWithTags(ctx, t, wsURL, senderSK, recipientPK, content, nil)
}

func TestMetadataCaching_SkipsRepublishOnRestart(t *testing.T) {
	t.Parallel()

	wsURL, cleanup := testutil.StartTestRelay(t)
	defer cleanup()

	botSK := gonostr.Generate()
	sessionDir := t.TempDir()
	cfg := Config{
		PrivateKey:     botSK.Hex(),
		Relays:         []string{wsURL},
		DMRelays:       []string{wsURL},
		SessionBaseDir: sessionDir,
		Profile: ProfileConfig{
			Name:  "TestBot",
			About: "A test bot",
		},
	}

	// First run: publishes profile + DM relay list.
	runBackendOnce(t, cfg)

	profilesBefore := countEventsOfKind(t, wsURL, 0, botSK.Public())
	dmRelaysBefore := countEventsOfKind(t, wsURL, gonostr.KindDMRelayList, botSK.Public())

	if profilesBefore == 0 {
		t.Fatal("first run should have published a profile event")
	}

	if dmRelaysBefore == 0 {
		t.Fatal("first run should have published a DM relay list event")
	}

	// Second run: same config, same sessionDir → should NOT re-publish.
	runBackendOnce(t, cfg)

	profilesAfter := countEventsOfKind(t, wsURL, 0, botSK.Public())
	dmRelaysAfter := countEventsOfKind(t, wsURL, gonostr.KindDMRelayList, botSK.Public())

	if profilesAfter != profilesBefore {
		t.Errorf("second run published extra profile events: before=%d after=%d", profilesBefore, profilesAfter)
	}

	if dmRelaysAfter != dmRelaysBefore {
		t.Errorf("second run published extra DM relay list events: before=%d after=%d", dmRelaysBefore, dmRelaysAfter)
	}
}

// runBackendOnce starts a backend with the given config, lets it run briefly,
// then shuts it down. Used by metadata caching tests to simulate restarts.
func runBackendOnce(t *testing.T, cfg Config) {
	t.Helper()

	b, err := NewBackend(context.Background(), cfg, func(context.Context, backend.Message) {})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := runBackendAsync(ctx, b)

	time.Sleep(500 * time.Millisecond)
	cancel()
	<-runErr
	b.Close()
}

// countEventsOfKind counts how many events of the given kind exist on the
// relay from the specified author.
func countEventsOfKind(t *testing.T, relayURL string, kind gonostr.Kind, author gonostr.PubKey) int {
	t.Helper()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("count events")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	count := 0
	for range pool.FetchMany(ctx, []string{relayURL}, gonostr.Filter{
		Kinds:   []gonostr.Kind{kind},
		Authors: []gonostr.PubKey{author},
	}, gonostr.SubscriptionOptions{}) {
		count++
	}

	return count
}

// aesGCMEncrypt encrypts plaintext with a deterministic test key and returns
// the ciphertext plus hex-encoded key, nonce, and pre-encryption hash.
func aesGCMEncrypt(t *testing.T, plaintext []byte) ([]byte, string, string, string) {
	t.Helper()

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}

	nonce := make([]byte, gcm.NonceSize())
	for i := range nonce {
		nonce[i] = byte(i + 100)
	}

	oxHash := sha256.Sum256(plaintext)

	return gcm.Seal(nil, nonce, plaintext, nil),
		hex.EncodeToString(key),
		hex.EncodeToString(nonce),
		hex.EncodeToString(oxHash[:])
}

// sendTestFileMessage sends a NIP-17 kind 15 file message gift-wrapped to the recipient.
func sendTestFileMessage(ctx context.Context, t *testing.T, wsURL string, senderSK gonostr.SecretKey, recipientPK gonostr.PubKey, fileURL, mimeType string) {
	t.Helper()

	sendTestFileMessageWithTags(ctx, t, wsURL, senderSK, recipientPK, fileURL, gonostr.Tags{
		{"file-type", mimeType},
	})
}

// sendTestEncryptedFileMessage sends a NIP-17 kind 15 file message with AES-GCM encryption tags.
func sendTestEncryptedFileMessage(ctx context.Context, t *testing.T, wsURL string, senderSK gonostr.SecretKey, recipientPK gonostr.PubKey, fileURL, mimeType, keyHex, nonceHex, oxHex string) {
	t.Helper()

	sendTestFileMessageWithTags(ctx, t, wsURL, senderSK, recipientPK, fileURL, gonostr.Tags{
		{"file-type", mimeType},
		{"encryption-algorithm", "aes-gcm"},
		{"decryption-key", keyHex},
		{"decryption-nonce", nonceHex},
		{"ox", oxHex},
	})
}

// sendTestFileMessageWithTags sends a NIP-17 kind 15 file message with arbitrary tags.
func sendTestFileMessageWithTags(ctx context.Context, t *testing.T, wsURL string, senderSK gonostr.SecretKey, recipientPK gonostr.PubKey, fileURL string, extraTags gonostr.Tags) {
	t.Helper()

	pool := gonostr.NewPool(gonostr.PoolOptions{})
	defer pool.Close("test done")

	kr := keyer.NewPlainKeySigner(senderSK)

	ourPubkey, err := kr.GetPublicKey(ctx)
	if err != nil {
		t.Fatalf("getting public key: %v", err)
	}

	tags := gonostr.Tags{{"p", recipientPK.Hex()}}
	tags = append(tags, extraTags...)

	rumor := gonostr.Event{
		Kind:      15, // KindFileMessage — not in go-nostr yet
		Content:   fileURL,
		CreatedAt: gonostr.Now(),
		PubKey:    ourPubkey,
		Tags:      tags,
	}
	rumor.ID = rumor.GetID()

	toThem, err := nip59.GiftWrap(
		rumor,
		recipientPK,
		func(s string) (string, error) { return kr.Encrypt(ctx, s, recipientPK) },
		func(e *gonostr.Event) error { return kr.SignEvent(ctx, e) },
		nil,
	)
	if err != nil {
		t.Fatalf("gift wrapping file message: %v", err)
	}

	relay, err := pool.EnsureRelay(wsURL)
	if err != nil {
		t.Fatalf("connecting to relay: %v", err)
	}

	if err := relay.Publish(ctx, toThem); err != nil {
		t.Fatalf("publishing gift wrap: %v", err)
	}
}

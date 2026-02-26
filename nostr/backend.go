// Package nostr implements the Backend interface for Nostr NIP-17 DMs.
package nostr

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip17"
	"fiatjaf.com/nostr/nip59"
	"github.com/pinpox/opencrow/backend"
)

// Config holds Nostr-specific configuration.
type Config struct {
	PrivateKey     string
	Relays         []string
	BlossomServers []string
	AllowedUsers   map[string]struct{}
	SessionBaseDir string
}

// Backend implements backend.Backend for Nostr NIP-17 DMs.
type Backend struct {
	keys    Keys
	kr      gonostr.Keyer
	cfg     Config
	pool    *gonostr.Pool
	handler backend.MessageHandler

	cancelMu sync.Mutex
	cancelFn context.CancelFunc

	// Single active conversation tracking
	activeMu     sync.Mutex
	activeConvID string

	// Dedup received events by ID; entries older than seenTTL are pruned.
	seenMu  sync.Mutex
	seen    map[gonostr.ID]time.Time
	seenTTL time.Duration

	// Last-seen DM timestamp
	lastSeenMu sync.Mutex
	lastSeen   gonostr.Timestamp
}

// NewBackend creates a new Nostr backend. The keys are derived from cfg.PrivateKey.
func NewBackend(cfg Config, handler backend.MessageHandler) *Backend {
	keys, err := loadKeys(cfg.PrivateKey)
	if err != nil {
		slog.Error("nostr: failed to load keys", "error", err)
	}

	return &Backend{
		keys:    keys,
		cfg:     cfg,
		handler: handler,
		seen:    make(map[gonostr.ID]time.Time),
		seenTTL: 3 * 24 * time.Hour, // matches the NIP-59 timestamp randomization window
	}
}

// Run starts the Nostr event loop — subscribes to kind 1059 gift wraps.
func (b *Backend) Run(ctx context.Context) error {
	b.pool = gonostr.NewPool(gonostr.PoolOptions{
		AuthRequiredHandler: func(ctx context.Context, evt *gonostr.Event) error {
			return evt.Sign(b.keys.SK)
		},
	})

	// Create keyer for NIP-44 encrypt/decrypt
	kr := keyer.NewPlainKeySigner(b.keys.SK)
	b.kr = kr

	// Load persisted last-seen timestamp
	b.lastSeen = loadLastSeen(b.cfg.SessionBaseDir)

	since := b.lastSeen - 259200 // 3 days offset for NIP-59 timestamp randomization
	if since < 0 {
		since = 0
	}

	slog.Info("nostr: subscribing to DMs", "pubkey", b.keys.PK.Hex(), "since", since, "relays", b.cfg.Relays)

	ctx, cancel := context.WithCancel(ctx)
	b.cancelMu.Lock()
	b.cancelFn = cancel
	b.cancelMu.Unlock()

	events := b.pool.SubscribeMany(ctx, b.cfg.Relays, gonostr.Filter{
		Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
		Tags:  gonostr.TagMap{"p": {b.keys.PK.Hex()}},
		Since: since,
	}, gonostr.SubscriptionOptions{})

	// Periodically prune the dedup set so it doesn't grow unbounded.
	go b.pruneSeenLoop(ctx)

	for ie := range events {
		if ctx.Err() != nil {
			break
		}
		evt := ie.Event
		b.processGiftWrap(ctx, &evt)
	}

	return ctx.Err()
}

// pruneSeenLoop removes entries from the seen map that are older than seenTTL.
func (b *Backend) pruneSeenLoop(ctx context.Context) {
	ticker := time.NewTicker(b.seenTTL / 3)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.pruneSeen()
		}
	}
}

func (b *Backend) pruneSeen() {
	cutoff := time.Now().Add(-b.seenTTL)
	b.seenMu.Lock()
	for id, t := range b.seen {
		if t.Before(cutoff) {
			delete(b.seen, id)
		}
	}
	b.seenMu.Unlock()
}

// Stop signals the backend to shut down.
func (b *Backend) Stop() {
	b.cancelMu.Lock()
	if b.cancelFn != nil {
		b.cancelFn()
	}
	b.cancelMu.Unlock()
}

// Close releases relay connections.
func (b *Backend) Close() error {
	return nil
}

// SendMessage sends a NIP-17 gift-wrapped DM. Fire-and-forget.
func (b *Backend) SendMessage(ctx context.Context, conversationID string, text string) {
	recipientPK, err := gonostr.PubKeyFromHex(conversationID)
	if err != nil {
		slog.Error("nostr: invalid recipient pubkey", "conversationID", conversationID, "error", err)
		return
	}

	if b.kr == nil {
		// Pool not started yet — create a temporary keyer and pool
		pool := gonostr.NewPool(gonostr.PoolOptions{})
		defer pool.Close("temporary pool done")
		kr := keyer.NewPlainKeySigner(b.keys.SK)
		b.sendDM(ctx, kr, pool, recipientPK, text)
		return
	}
	b.sendDM(ctx, b.kr, b.pool, recipientPK, text)
}

func (b *Backend) sendDM(ctx context.Context, kr gonostr.Keyer, pool *gonostr.Pool, recipientPK gonostr.PubKey, text string) {
	toUs, toThem, err := nip17.PrepareMessage(ctx, text, nil, kr, recipientPK, nil)
	if err != nil {
		slog.Error("nostr: failed to prepare DM", "recipient", recipientPK.Hex(), "error", err)
		return
	}

	for _, relayURL := range b.cfg.Relays {
		r, err := pool.EnsureRelay(relayURL)
		if err != nil {
			slog.Warn("nostr: failed to connect to relay", "relay", relayURL, "error", err)
			continue
		}
		if err := r.Publish(ctx, toUs); err != nil {
			slog.Warn("nostr: failed to publish toUs", "relay", relayURL, "error", err)
		}
		if err := r.Publish(ctx, toThem); err != nil {
			slog.Warn("nostr: failed to publish toThem", "relay", relayURL, "error", err)
		}
	}
}

// SendFile uploads a file to Blossom and sends the URL as a DM.
func (b *Backend) SendFile(ctx context.Context, conversationID string, filePath string) error {
	url, err := b.uploadToBlossomImpl(ctx, filePath)
	if err != nil {
		return err
	}
	b.SendMessage(ctx, conversationID, url)
	return nil
}

// SetTyping is a no-op on Nostr.
func (b *Backend) SetTyping(_ context.Context, _ string, _ bool) {}

// ResetConversation clears the active conversation if it matches conversationID.
func (b *Backend) ResetConversation(_ context.Context, conversationID string) {
	b.activeMu.Lock()
	if b.activeConvID == conversationID {
		b.activeConvID = ""
	}
	b.activeMu.Unlock()
}

// SystemPromptExtra returns Nostr-specific system prompt context.
func (b *Backend) SystemPromptExtra() string {
	extra := `You are communicating via Nostr encrypted DMs (NIP-17).

## Sending files to the user

You can send files back to the user. To do this, include a <sendfile> tag
in your response with the absolute path to the file:

<sendfile>/path/to/file.png</sendfile>

The bot will upload the file to a Blossom server and send the URL in a DM.
You can include multiple <sendfile> tags in a single response.`

	if len(b.cfg.BlossomServers) > 0 {
		extra += fmt.Sprintf("\n\nBlossom servers: %v", b.cfg.BlossomServers)
	}

	return extra
}

// processGiftWrap unwraps a kind 1059 event and dispatches to the handler.
func (b *Backend) processGiftWrap(ctx context.Context, evt *gonostr.Event) {
	if evt == nil {
		return
	}

	// Dedup by event ID
	b.seenMu.Lock()
	if _, ok := b.seen[evt.ID]; ok {
		b.seenMu.Unlock()
		return
	}
	b.seen[evt.ID] = time.Now()
	b.seenMu.Unlock()

	// Unwrap: gift wrap → seal → rumor
	rumor, err := nip59.GiftUnwrap(*evt,
		func(otherpubkey gonostr.PubKey, ciphertext string) (string, error) {
			return b.kr.Decrypt(ctx, ciphertext, otherpubkey)
		},
	)
	if err != nil {
		slog.Warn("nostr: failed to unwrap gift wrap", "event_id", evt.ID.Hex(), "error", err)
		return
	}

	senderPK := rumor.PubKey
	if senderPK == b.keys.PK {
		return
	}

	senderHex := senderPK.Hex()

	// Check allowed users
	if len(b.cfg.AllowedUsers) > 0 {
		if _, ok := b.cfg.AllowedUsers[senderHex]; !ok {
			slog.Debug("nostr: dropping DM from non-allowed user", "sender", senderHex)
			return
		}
	}

	// Single active conversation
	b.activeMu.Lock()
	if b.activeConvID == "" {
		b.activeConvID = senderHex
		slog.Info("nostr: active conversation set", "pubkey", senderHex)
	} else if b.activeConvID != senderHex {
		b.activeMu.Unlock()
		slog.Info("nostr: dropping DM, different active conversation", "active", b.activeConvID, "sender", senderHex)
		return
	}
	b.activeMu.Unlock()

	// Update last-seen
	if rumor.CreatedAt > b.lastSeen {
		b.lastSeenMu.Lock()
		b.lastSeen = rumor.CreatedAt
		b.lastSeenMu.Unlock()
		saveLastSeen(b.cfg.SessionBaseDir, rumor.CreatedAt)
	}

	slog.Info("nostr: received DM", "sender", senderHex, "len", len(rumor.Content))

	text := rumor.Content

	// Detect and download media URLs
	text = b.rewriteMediaURLs(ctx, text, senderHex)

	b.handler(ctx, backend.Message{
		ConversationID: senderHex,
		SenderID:       senderHex,
		Text:           text,
	})
}

var mediaURLRe = regexp.MustCompile(`(?i)https?://\S+\.(?:png|jpg|jpeg|gif|webp|pdf|mp3|mp4|wav|ogg|svg|bmp|tiff|zip)(?:\?\S*)?`)

// rewriteMediaURLs finds media URLs in text, downloads them, and rewrites the
// text with the local file path in the standard attachment format.
func (b *Backend) rewriteMediaURLs(ctx context.Context, text, conversationID string) string {
	urls := mediaURLRe.FindAllString(text, -1)
	if len(urls) == 0 {
		return text
	}

	processed := make(map[string]bool, len(urls))
	for _, rawURL := range urls {
		if processed[rawURL] {
			continue
		}

		localPath, err := downloadURL(ctx, rawURL, b.cfg.SessionBaseDir, conversationID)
		if err != nil {
			slog.Warn("nostr: failed to download media URL", "url", rawURL, "error", err)
			continue
		}

		processed[rawURL] = true
		replacement := fmt.Sprintf("[User sent a file (no caption): %s]\nUse the read tool to view it.", localPath)
		text = strings.ReplaceAll(text, rawURL, replacement)
	}

	return text
}

// Package nostr implements the Backend interface for Nostr NIP-17 DMs.
package nostr

import (
	"context"
	"encoding/json"
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

// ProfileConfig holds NIP-01 kind 0 metadata fields.
type ProfileConfig struct {
	Name        string // NIP-01 "name"
	DisplayName string // NIP-01 "display_name"
	About       string // NIP-01 "about"
	Picture     string // NIP-01 "picture"
}

// Config holds Nostr-specific configuration.
type Config struct {
	PrivateKey     string
	Relays         []string // general relays for querying and subscribing
	DMRelays       []string // relays advertised in kind 10050 for DM delivery
	BlossomServers []string
	AllowedUsers   map[string]struct{}
	SessionBaseDir string
	Profile        ProfileConfig
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

	// Dedup received gift wrap events by ID (in-memory, handles multi-relay duplicates).
	seenMu       sync.Mutex
	seenGiftWrap map[gonostr.ID]time.Time

	// Dedup processed rumor IDs (persisted to disk, survives restarts).
	seenRumorsMu sync.Mutex
	seenRumors   map[string]time.Time
	seenTTL      time.Duration

	// Persistent retry queue for failed publishes.
	pubQueue *publishQueue
}

// NewBackend creates a new Nostr backend. The keys are derived from cfg.PrivateKey.
func NewBackend(cfg Config, handler backend.MessageHandler) (*Backend, error) {
	keys, err := loadKeys(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("nostr: failed to load keys: %w", err)
	}

	// Default DMRelays to Relays if not explicitly configured.
	if len(cfg.DMRelays) == 0 {
		cfg.DMRelays = cfg.Relays
	}

	ttl := 7 * 24 * time.Hour // 7 days — covers NIP-59 randomization window with margin

	pq, err := newPublishQueue(cfg.SessionBaseDir)
	if err != nil {
		return nil, fmt.Errorf("loading publish queue: %w", err)
	}

	return &Backend{
		keys:         keys,
		cfg:          cfg,
		handler:      handler,
		seenGiftWrap: make(map[gonostr.ID]time.Time),
		seenRumors:   loadSeenRumors(cfg.SessionBaseDir, ttl),
		seenTTL:      ttl,
		pubQueue:     pq,
	}, nil
}

// Run starts the Nostr event loop — subscribes to kind 1059 gift wraps.
func (b *Backend) Run(ctx context.Context) error {
	b.pool = gonostr.NewPool(gonostr.PoolOptions{
		AuthRequiredHandler: func(_ context.Context, evt *gonostr.Event) error {
			return evt.Sign(b.keys.SK)
		},
	})

	// Create keyer for NIP-44 encrypt/decrypt
	kr := keyer.NewPlainKeySigner(b.keys.SK)
	b.kr = kr

	// Wire the pool into the publish queue so it can connect to relays.
	b.pubQueue.setPool(b.pool)

	// Publish NIP-01 profile metadata (kind 0) so the bot has a name/about.
	b.publishProfile(ctx)

	// Publish NIP-17 DM relay list (kind 10050) so clients know where to
	// send gift wraps. Without this, apps like 0xchat cannot discover the
	// bot's preferred DM relays.
	b.publishDMRelayList(ctx)

	// Discover allowed users' DM relay lists so we also subscribe there.
	// NIP-17 clients send gift wraps to the sender's own DM relays when
	// they don't know the recipient's, so we need to listen on those too.
	subRelays := b.discoverSubscriptionRelays(ctx)

	since := sinceFromSeenRumors(b.seenRumors, b.seenTTL)

	slog.Info("nostr: subscribing to DMs", "pubkey", b.keys.PK.Hex(), "since", since, "seen_rumors", len(b.seenRumors), "relays", subRelays)

	ctx, cancel := context.WithCancel(ctx)

	b.cancelMu.Lock()
	b.cancelFn = cancel
	b.cancelMu.Unlock()

	events := b.pool.SubscribeMany(ctx, subRelays, gonostr.Filter{
		Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
		Tags:  gonostr.TagMap{"p": {b.keys.PK.Hex()}},
		Since: since,
	}, gonostr.SubscriptionOptions{})

	// Periodically prune the dedup set so it doesn't grow unbounded.
	go b.pruneSeenLoop(ctx)

	// Drain the publish queue in the background.
	pubDone := make(chan struct{})

	go func() {
		b.pubQueue.run(ctx)
		close(pubDone)
	}()

	for ie := range events {
		if ctx.Err() != nil {
			break
		}

		slog.Debug("nostr: gift wrap received from relay", "relay", ie.Relay.URL, "event_id", ie.ID.Hex())
		evt := ie.Event
		b.processGiftWrap(ctx, &evt)
	}

	// Wait for the publish queue goroutine to finish so it doesn't
	// write to the data directory after Run returns.
	<-pubDone

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("nostr event loop: %w", err)
	}

	return nil
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
	if b.pool != nil {
		b.pool.Close("backend closed")
	}

	return nil
}

// SendMessage sends a NIP-17 gift-wrapped DM. When replyToID is non-empty,
// an "e" tag referencing that event is included in the rumor so the
// recipient's client can display threading. Returns the rumor event ID.
func (b *Backend) SendMessage(ctx context.Context, conversationID string, text string, replyToID string) string {
	recipientPK, err := gonostr.PubKeyFromHex(conversationID)
	if err != nil {
		slog.Error("nostr: invalid recipient pubkey", "conversationID", conversationID, "error", err)

		return ""
	}

	var extraTags gonostr.Tags
	if replyToID != "" {
		extraTags = gonostr.Tags{{"e", replyToID}}
	}

	if b.kr == nil {
		slog.Error("nostr: SendMessage called before Run()", "recipient", conversationID)

		return ""
	}

	return b.sendDM(ctx, b.kr, b.pool, recipientPK, text, extraTags)
}

// SendFile uploads a file to Blossom and sends the URL as a DM.
func (b *Backend) SendFile(ctx context.Context, conversationID string, filePath string) error {
	url, err := b.uploadToBlossomImpl(ctx, filePath)
	if err != nil {
		return err
	}

	b.SendMessage(ctx, conversationID, url, "")

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

// --- unexported methods ---

// discoverSubscriptionRelays returns the full set of relays to subscribe on:
// the bot's configured relays, its DM relays, plus the DM relay lists of all
// allowed users.
func (b *Backend) discoverSubscriptionRelays(ctx context.Context) []string {
	seen := make(map[string]struct{}, len(b.cfg.Relays)+len(b.cfg.DMRelays))
	relays := make([]string, 0, len(b.cfg.Relays)+len(b.cfg.DMRelays))

	addRelay := func(r string) {
		normalized := strings.TrimRight(r, "/")
		if _, ok := seen[normalized]; !ok {
			seen[normalized] = struct{}{}

			relays = append(relays, r)
		}
	}

	for _, r := range b.cfg.Relays {
		addRelay(r)
	}

	for _, r := range b.cfg.DMRelays {
		addRelay(r)
	}

	b.discoverUserDMRelays(ctx, seen, &relays)

	return relays
}

// discoverUserDMRelays queries each allowed user's DM relay list and adds
// any new relays to the set.
func (b *Backend) discoverUserDMRelays(ctx context.Context, seen map[string]struct{}, relays *[]string) {
	if len(b.cfg.AllowedUsers) == 0 {
		return
	}

	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for userHex := range b.cfg.AllowedUsers {
		pk, err := gonostr.PubKeyFromHex(userHex)
		if err != nil {
			slog.Warn("nostr: invalid allowed user pubkey", "pubkey", userHex, "error", err)

			continue
		}

		ie := b.pool.QuerySingle(queryCtx, b.cfg.Relays, gonostr.Filter{
			Authors: []gonostr.PubKey{pk},
			Kinds:   []gonostr.Kind{gonostr.KindDMRelayList},
		}, gonostr.SubscriptionOptions{})
		if ie == nil {
			slog.Debug("nostr: no DM relay list found for user", "pubkey", userHex)

			continue
		}

		for _, tag := range ie.Tags {
			if len(tag) >= 2 && tag[0] == "relay" {
				r := tag[1]

				normalized := strings.TrimRight(r, "/")
				if _, ok := seen[normalized]; !ok {
					seen[normalized] = struct{}{}

					*relays = append(*relays, r)
					slog.Info("nostr: discovered user DM relay", "pubkey", userHex, "relay", r)
				}
			}
		}
	}
}

// publishProfile publishes a NIP-01 kind 0 metadata event so the bot
// has a visible name, about, and picture on Nostr clients.
func (b *Backend) publishProfile(_ context.Context) {
	p := b.cfg.Profile
	if p.Name == "" && p.DisplayName == "" && p.About == "" && p.Picture == "" {
		return
	}

	meta := buildProfileMeta(p)

	content, err := json.Marshal(meta)
	if err != nil {
		slog.Error("nostr: failed to marshal profile metadata", "error", err)

		return
	}

	evt := gonostr.Event{
		Kind:      0,
		CreatedAt: gonostr.Now(),
		Content:   string(content),
		PubKey:    b.keys.PK,
	}
	if err := evt.Sign(b.keys.SK); err != nil {
		slog.Error("nostr: failed to sign profile event", "error", err)

		return
	}

	b.publishToRelays(evt, b.cfg.Relays, "profile")
}

// buildProfileMeta constructs the metadata map from non-empty profile fields.
func buildProfileMeta(p ProfileConfig) map[string]string {
	meta := make(map[string]string)
	if p.Name != "" {
		meta["name"] = p.Name
	}

	if p.DisplayName != "" {
		meta["display_name"] = p.DisplayName
	}

	if p.About != "" {
		meta["about"] = p.About
	}

	if p.Picture != "" {
		meta["picture"] = p.Picture
	}

	return meta
}

// publishDMRelayList publishes a NIP-17 DM relay list (kind 10050) so
// clients can discover where to send gift-wrapped DMs to this bot.
// Uses DMRelays (not Relays) because some general-purpose relays silently
// drop kind 1059 gift wraps.
func (b *Backend) publishDMRelayList(_ context.Context) {
	tags := make(gonostr.Tags, 0, len(b.cfg.DMRelays))
	for _, relay := range b.cfg.DMRelays {
		tags = append(tags, gonostr.Tag{"relay", relay})
	}

	evt := gonostr.Event{
		Kind:      gonostr.KindDMRelayList,
		CreatedAt: gonostr.Now(),
		Tags:      tags,
		PubKey:    b.keys.PK,
	}
	if err := evt.Sign(b.keys.SK); err != nil {
		slog.Error("nostr: failed to sign DM relay list", "error", err)

		return
	}

	b.publishToRelays(evt, b.cfg.Relays, "DM relay list")
}

// pruneSeenLoop prunes stale entries from both dedup sets.
func (b *Backend) pruneSeenLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
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
	for id, t := range b.seenGiftWrap {
		if t.Before(cutoff) {
			delete(b.seenGiftWrap, id)
		}
	}
	b.seenMu.Unlock()

	b.seenRumorsMu.Lock()
	for id, t := range b.seenRumors {
		if t.Before(cutoff) {
			delete(b.seenRumors, id)
		}
	}

	saveSeenRumors(b.cfg.SessionBaseDir, b.seenRumors)
	b.seenRumorsMu.Unlock()
}

func (b *Backend) sendDM(ctx context.Context, kr gonostr.Keyer, pool *gonostr.Pool, recipientPK gonostr.PubKey, text string, extraTags gonostr.Tags) string {
	rumor, err := b.buildDMRumor(ctx, kr, recipientPK, text, extraTags)
	if err != nil {
		slog.Error("nostr: failed to build DM rumor", "recipient", recipientPK.Hex(), "error", err)

		return ""
	}

	toUs, err := nip59.GiftWrap(rumor, rumor.PubKey,
		func(s string) (string, error) { return kr.Encrypt(ctx, s, rumor.PubKey) },
		func(e *gonostr.Event) error { return kr.SignEvent(ctx, e) },
		nil,
	)
	if err != nil {
		slog.Error("nostr: failed to gift-wrap DM (toUs)", "recipient", recipientPK.Hex(), "error", err)

		return ""
	}

	toThem, err := nip59.GiftWrap(rumor, recipientPK,
		func(s string) (string, error) { return kr.Encrypt(ctx, s, recipientPK) },
		func(e *gonostr.Event) error { return kr.SignEvent(ctx, e) },
		nil,
	)
	if err != nil {
		slog.Error("nostr: failed to gift-wrap DM (toThem)", "recipient", recipientPK.Hex(), "error", err)

		return ""
	}

	b.publishDMGiftWraps(ctx, pool, toUs, toThem, recipientPK)

	return rumor.ID.Hex()
}

// buildDMRumor constructs and returns a kind 14 rumor event for a DM.
func (b *Backend) buildDMRumor(ctx context.Context, kr gonostr.Keyer, recipientPK gonostr.PubKey, text string, extraTags gonostr.Tags) (gonostr.Event, error) {
	ourPubkey, err := kr.GetPublicKey(ctx)
	if err != nil {
		return gonostr.Event{}, fmt.Errorf("getting public key: %w", err)
	}

	tags := make(gonostr.Tags, 0, len(extraTags)+1)
	tags = append(tags, extraTags...)
	tags = append(tags, gonostr.Tag{"p", recipientPK.Hex()})

	rumor := gonostr.Event{
		Kind:      gonostr.KindDirectMessage,
		Content:   text,
		Tags:      tags,
		CreatedAt: gonostr.Now(),
		PubKey:    ourPubkey,
	}
	rumor.ID = rumor.GetID()

	return rumor, nil
}

// publishDMGiftWraps publishes the two gift-wrap copies to the appropriate relays.
func (b *Backend) publishDMGiftWraps(ctx context.Context, pool *gonostr.Pool, toUs, toThem gonostr.Event, recipientPK gonostr.PubKey) {
	// Publish our copy to our DM relays.
	b.publishToRelays(toUs, b.cfg.DMRelays, "DM toUs")

	theirRelays := b.recipientRelays(ctx, recipientPK, pool)

	slog.Debug("nostr: sending DM", "recipient", recipientPK.Hex(), "their_relays", theirRelays)

	b.publishToRelays(toThem, theirRelays, "DM toThem")
}

// sendReaction sends a NIP-25 kind 7 reaction gift-wrapped via NIP-59 to
// acknowledge receipt of an incoming DM. The reaction references the original
// rumor by its event ID so the sender's client can attach it to the right message.
func (b *Backend) sendReaction(ctx context.Context, rumorID gonostr.ID, recipientPK gonostr.PubKey) {
	rumor := gonostr.Event{
		Kind:      gonostr.KindReaction,
		Content:   "👍",
		CreatedAt: gonostr.Now(),
		PubKey:    b.keys.PK,
		Tags: gonostr.Tags{
			{"e", rumorID.Hex()},
			{"p", recipientPK.Hex()},
			{"k", "14"},
		},
	}
	rumor.ID = rumor.GetID()

	// Gift-wrap to recipient.
	toThem, err := nip59.GiftWrap(
		rumor,
		recipientPK,
		func(s string) (string, error) { return b.kr.Encrypt(ctx, s, recipientPK) },
		func(e *gonostr.Event) error { return b.kr.SignEvent(ctx, e) },
		nil,
	)
	if err != nil {
		slog.Warn("nostr: failed to gift-wrap reaction", "recipient", recipientPK.Hex(), "error", err)

		return
	}

	b.publishToRelays(toThem, b.recipientRelays(ctx, recipientPK, b.pool), "reaction")
}

// recipientRelays resolves the recipient's DM relays via NIP-17 discovery,
// falling back to our own DM relays when the recipient has no list published.
func (b *Backend) recipientRelays(ctx context.Context, recipientPK gonostr.PubKey, pool *gonostr.Pool) []string {
	relays := nip17.GetDMRelays(ctx, recipientPK, pool, b.cfg.Relays)
	if len(relays) == 0 {
		return b.cfg.DMRelays
	}

	return relays
}

// publishToRelays enqueues an event for publishing to the given relays.
// The actual publishing happens asynchronously via the queue's drain loop.
func (b *Backend) publishToRelays(evt gonostr.Event, relays []string, label string) {
	b.pubQueue.enqueue(evt, relays, label)
}

// processGiftWrap unwraps a kind 1059 event and dispatches to the handler.
func (b *Backend) processGiftWrap(ctx context.Context, evt *gonostr.Event) {
	if evt == nil {
		return
	}

	slog.Debug("nostr: processing gift wrap", "event_id", evt.ID.Hex(), "event_kind", evt.Kind)

	if !b.dedupGiftWrap(evt.ID) {
		return
	}

	rumor, err := nip59.GiftUnwrap(*evt,
		func(otherpubkey gonostr.PubKey, ciphertext string) (string, error) {
			return b.kr.Decrypt(ctx, ciphertext, otherpubkey)
		},
	)
	if err != nil {
		slog.Warn("nostr: failed to unwrap gift wrap", "event_id", evt.ID.Hex(), "error", err)

		return
	}

	if !b.dedupRumor(rumor.ID.Hex(), evt.ID.Hex()) {
		return
	}

	senderHex := rumor.PubKey.Hex()
	if rumor.PubKey == b.keys.PK || !b.isAllowed(senderHex) || !b.claimConversation(senderHex) {
		return
	}

	slog.Info("nostr: received DM", "sender", senderHex, "len", len(rumor.Content), "tags", len(rumor.Tags))

	// Send a 👍 reaction to acknowledge receipt before processing.
	go b.sendReaction(ctx, rumor.ID, rumor.PubKey)

	text := b.rewriteMediaURLs(ctx, rumor.Content, senderHex)

	b.handler(ctx, backend.Message{
		ConversationID: senderHex,
		SenderID:       senderHex,
		Text:           text,
		MessageID:      rumor.ID.Hex(),
		ReplyToID:      rumorReplyTarget(rumor),
	})
}

// dedupGiftWrap returns true if this gift wrap event has not been seen before.
func (b *Backend) dedupGiftWrap(id gonostr.ID) bool {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()

	if _, ok := b.seenGiftWrap[id]; ok {
		slog.Debug("nostr: dropping duplicate gift wrap", "event_id", id.Hex())

		return false
	}

	b.seenGiftWrap[id] = time.Now()

	return true
}

// dedupRumor returns true if this rumor has not been processed before.
func (b *Backend) dedupRumor(rumorHex, evtHex string) bool {
	b.seenRumorsMu.Lock()
	defer b.seenRumorsMu.Unlock()

	if _, ok := b.seenRumors[rumorHex]; ok {
		slog.Debug("nostr: dropping already-processed rumor", "rumor_id", rumorHex, "event_id", evtHex)

		return false
	}

	b.seenRumors[rumorHex] = time.Now()
	saveSeenRumors(b.cfg.SessionBaseDir, b.seenRumors)

	return true
}

// isAllowed checks whether the sender is in the allowed users set (or if the set is empty).
func (b *Backend) isAllowed(senderHex string) bool {
	if len(b.cfg.AllowedUsers) == 0 {
		return true
	}

	_, ok := b.cfg.AllowedUsers[senderHex]
	if !ok {
		slog.Debug("nostr: dropping DM from non-allowed user", "sender", senderHex)
	}

	return ok
}

// claimConversation tries to set the active conversation to senderHex.
// Returns false if a different conversation is already active.
func (b *Backend) claimConversation(senderHex string) bool {
	b.activeMu.Lock()
	defer b.activeMu.Unlock()

	if b.activeConvID == "" {
		b.activeConvID = senderHex
		slog.Info("nostr: active conversation set", "pubkey", senderHex)

		return true
	}

	if b.activeConvID != senderHex {
		slog.Info("nostr: dropping DM, different active conversation", "active", b.activeConvID, "sender", senderHex)

		return false
	}

	return true
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

		slog.Debug("nostr: downloaded media URL", "url", rawURL, "localPath", localPath)
		processed[rawURL] = true
		replacement := fmt.Sprintf("[User sent a file (no caption): %s]\nUse the read tool to view it.", localPath)
		text = strings.ReplaceAll(text, rawURL, replacement)
	}

	return text
}

// rumorReplyTarget returns the value of the first "e" tag in the rumor, or ""
// if none is present. An "e" tag indicates the user is replying to a specific
// previous message in their Nostr client.
func rumorReplyTarget(rumor gonostr.Event) string {
	for _, tag := range rumor.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			return tag[1]
		}
	}

	return ""
}

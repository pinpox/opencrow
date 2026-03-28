// Package nostr implements the Backend interface for Nostr NIP-17 DMs.
package nostr

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip59"
	"github.com/pinpox/opencrow/backend"
)

// metadataRefreshInterval controls how often profile and DM relay list
// events are re-published even when the content hasn't changed. Relays
// may drop data, so periodic refreshes ensure discoverability.
const metadataRefreshInterval = 24 * time.Hour

// seenRumorTTL is how long dedup entries are retained. 7 days covers the
// NIP-59 randomization window with margin.
const seenRumorTTL = 7 * 24 * time.Hour

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
	db      *DB

	cancel backend.Canceler

	// Single active conversation tracking
	active backend.ActiveConversation

	// Persistent retry queue for failed publishes.
	pubQueue *publishQueue

	// wg tracks in-flight background goroutines (e.g. reactions) so Run
	// can wait for them before returning.
	wg sync.WaitGroup
}

// NewBackend creates a new Nostr backend. The keys are derived from cfg.PrivateKey.
func NewBackend(ctx context.Context, cfg Config, handler backend.MessageHandler) (*Backend, error) {
	keys, err := loadKeys(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("nostr: failed to load keys: %w", err)
	}

	// Default DMRelays to Relays if not explicitly configured.
	if len(cfg.DMRelays) == 0 {
		cfg.DMRelays = cfg.Relays
	}

	db, err := OpenDB(ctx, cfg.SessionBaseDir)
	if err != nil {
		return nil, fmt.Errorf("nostr: opening db: %w", err)
	}

	migrateLegacySeenRumors(ctx, db, cfg.SessionBaseDir)

	pq, err := newPublishQueue(ctx, db)
	if err != nil {
		db.Close()

		return nil, fmt.Errorf("loading publish queue: %w", err)
	}

	return &Backend{
		keys:     keys,
		cfg:      cfg,
		handler:  handler,
		db:       db,
		pubQueue: pq,
	}, nil
}

// Run starts the Nostr event loop — subscribes to kind 1059 gift wraps.
//
//nolint:contextcheck // publish queue intentionally uses a non-inherited context
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

	since := subscriptionSince(ctx, b.db)

	slog.Info("nostr: subscribing to DMs", "pubkey", b.keys.PK.Hex(), "since", since, "relays", subRelays)

	ctx, cancel := context.WithCancel(ctx)

	b.cancel.Set(cancel)

	events := b.pool.SubscribeMany(ctx, subRelays, gonostr.Filter{
		Kinds: []gonostr.Kind{gonostr.KindGiftWrap},
		Tags:  gonostr.TagMap{"p": {b.keys.PK.Hex()}},
		Since: since,
	}, gonostr.SubscriptionOptions{})

	// Periodically prune the dedup set so it doesn't grow unbounded.
	go b.pruneSeenLoop(ctx)

	// Drain the publish queue in the background. Use a separate context
	// so the queue keeps running while reaction goroutines finish enqueueing
	// after the subscription context is cancelled.
	pubCtx, pubCancel := context.WithCancel(context.Background())
	pubDone := make(chan struct{})

	go func() {
		b.pubQueue.run(pubCtx)
		close(pubDone)
	}()

	for ie := range events {
		if ctx.Err() != nil {
			break
		}

		slog.Debug("nostr: gift wrap received from relay", "relay", ie.Relay.URL, "event_id", ie.ID.Hex())
		evt := ie.Event
		b.processGiftWrap(ctx, pubCtx, &evt)
	}

	return b.drainAndShutdown(ctx, pubCancel, pubDone)
}

// Stop signals the backend to shut down.
func (b *Backend) Stop() {
	b.cancel.Cancel()
}

// Close releases relay connections and the database.
func (b *Backend) Close() error {
	if b.pool != nil {
		b.pool.Close("backend closed")
	}

	return b.db.Close()
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

// SendFile encrypts a file with AES-256-GCM, uploads the ciphertext to
// Blossom, and sends the URL as a NIP-17 kind 15 file message. The
// decryption key and nonce are included in the encrypted rumor tags so
// only the conversation partner can read the file.
func (b *Backend) SendFile(ctx context.Context, conversationID string, filePath string) error {
	upload, err := b.uploadToBlossomImpl(ctx, filePath)
	if err != nil {
		return err
	}

	recipientPK, err := gonostr.PubKeyFromHex(conversationID)
	if err != nil {
		return fmt.Errorf("invalid recipient pubkey: %w", err)
	}

	if b.kr == nil {
		return errors.New("SendFile called before Run()")
	}

	tags := gonostr.Tags{
		{"file-type", upload.MIMEType},
		{"encryption-algorithm", "aes-gcm"},
		{"decryption-key", upload.Enc.KeyHex},
		{"decryption-nonce", upload.Enc.NonceHex},
		{"x", upload.XHex},
		{"ox", upload.Enc.OxHex},
	}

	b.sendFileMessage(ctx, b.kr, b.pool, recipientPK, upload.URL, tags)

	return nil
}

// SetTyping is a no-op on Nostr.
func (b *Backend) SetTyping(_ context.Context, _ string, _ bool) {}

// ResetConversation clears the active conversation if it matches conversationID.
func (b *Backend) ResetConversation(_ context.Context, conversationID string) {
	b.active.Reset(conversationID)
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

// MarkdownFlavor returns MarkdownBasic: popular Nostr clients (Damus,
// Amethyst, 0xchat) render fenced code blocks and inline backticks, but some
// (0xchat) display the language hint after the opening fence literally, so
// callers should emit plain ``` fences.
func (b *Backend) MarkdownFlavor() backend.MarkdownFlavor {
	return backend.MarkdownBasic
}

// --- unexported methods ---

// drainAndShutdown waits for in-flight reaction goroutines, then stops the
// publish queue and waits for it to finish writing.
func (b *Backend) drainAndShutdown(ctx context.Context, pubCancel context.CancelFunc, pubDone <-chan struct{}) error {
	// Wait for in-flight background goroutines (reactions) to finish
	// enqueueing before stopping the publish queue. Without this,
	// a reaction goroutine can write to the data directory after Run
	// returns, racing with cleanup of the session directory.
	b.wg.Wait()

	// Now cancel the publish queue's context so its drain loop exits
	// after processing any final items enqueued by the reactions above.
	pubCancel()

	// Wait for the publish queue goroutine to finish so it doesn't
	// write to the data directory after Run returns.
	<-pubDone

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("nostr event loop: %w", err)
	}

	return nil
}

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

	for _, r := range slices.Concat(b.cfg.Relays, b.cfg.DMRelays) {
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
// has a visible name, about, and picture on Nostr clients. Skips
// publishing when the profile content hasn't changed and was published
// recently (within metadataRefreshInterval).
func (b *Backend) publishProfile(ctx context.Context) {
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

	hash := hashContent(string(content), b.cfg.Relays)

	if b.metadataIsFresh(ctx, 0, hash) {
		slog.Debug("nostr: skipping profile publish (unchanged, still fresh)")

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

	b.pubQueue.enqueue(ctx, evt, b.cfg.Relays, "profile")
	b.markMetadataPublished(ctx, 0, hash)
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
// drop kind 1059 gift wraps. Skips publishing when the relay list hasn't
// changed and was published recently (within metadataRefreshInterval).
func (b *Backend) publishDMRelayList(ctx context.Context) {
	tags := make(gonostr.Tags, 0, len(b.cfg.DMRelays))
	for _, relay := range b.cfg.DMRelays {
		tags = append(tags, gonostr.Tag{"relay", relay})
	}

	hash := hashContent(strings.Join(b.cfg.DMRelays, "\n"), b.cfg.Relays)

	if b.metadataIsFresh(ctx, int64(gonostr.KindDMRelayList), hash) {
		slog.Debug("nostr: skipping DM relay list publish (unchanged, still fresh)")

		return
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

	b.pubQueue.enqueue(ctx, evt, b.cfg.Relays, "DM relay list")
	b.markMetadataPublished(ctx, int64(gonostr.KindDMRelayList), hash)
}

// pruneSeenLoop periodically removes stale entries from the seen_rumors table.
func (b *Backend) pruneSeenLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneStaleSeenRumors(ctx, b.db, seenRumorTTL)
		}
	}
}

// processGiftWrap unwraps a kind 1059 event and dispatches to the handler.
func (b *Backend) processGiftWrap(ctx, pubCtx context.Context, evt *gonostr.Event) {
	if evt == nil {
		return
	}

	slog.Debug("nostr: processing gift wrap", "event_id", evt.ID.Hex(), "event_kind", evt.Kind)

	rumor, err := nip59.GiftUnwrap(*evt,
		func(otherpubkey gonostr.PubKey, ciphertext string) (string, error) {
			return b.kr.Decrypt(ctx, ciphertext, otherpubkey)
		},
	)
	if err != nil {
		slog.Warn("nostr: failed to unwrap gift wrap", "event_id", evt.ID.Hex(), "error", err)

		return
	}

	if !b.dedupRumor(ctx, rumor.ID.Hex(), evt.ID.Hex()) {
		return
	}

	senderHex := rumor.PubKey.Hex()
	if rumor.PubKey == b.keys.PK || !b.isAllowed(senderHex) || !b.claimConversation(senderHex) {
		return
	}

	slog.Info("nostr: received DM", "sender", senderHex, "kind", rumor.Kind, "len", len(rumor.Content), "tags", len(rumor.Tags))

	// Send a 👍 reaction to acknowledge receipt before processing.
	// Use pubCtx so the reaction can finish encrypting and enqueueing
	// even after the subscription context is cancelled by Stop().
	b.wg.Go(func() {
		b.sendReaction(pubCtx, rumor.ID, rumor.PubKey, rumor.Kind)
	})

	var text string

	if rumor.Kind == KindFileMessage {
		text = b.handleFileMessage(ctx, rumor, senderHex)
	} else {
		// Kind 14 (chat message) or any other kind: treat content as text,
		// rewriting any inline media URLs to local paths.
		text = b.rewriteMediaURLs(ctx, rumor.Content, senderHex)
	}

	b.handler(ctx, backend.Message{
		ConversationID: senderHex,
		SenderID:       senderHex,
		Text:           text,
		MessageID:      rumor.ID.Hex(),
		ReplyToID:      rumorReplyTarget(rumor),
	})
}

// dedupRumor returns true if this rumor has not been processed before.
// Uses the SQLite database as the authoritative dedup store.
func (b *Backend) dedupRumor(ctx context.Context, rumorHex, evtHex string) bool {
	// Check if already seen via a cheap upsert: if the row already existed,
	// the INSERT is a no-op and we know it's a duplicate.
	seen, err := checkAndMarkSeen(ctx, b.db, rumorHex)
	if err != nil {
		slog.Warn("nostr: dedup check failed, accepting rumor", "rumor_id", rumorHex, "error", err)

		return true
	}

	if seen {
		slog.Debug("nostr: dropping already-processed rumor", "rumor_id", rumorHex, "event_id", evtHex)

		return false
	}

	return true
}

// isAllowed checks whether the sender is in the allowed users set (or if the set is empty).
func (b *Backend) isAllowed(senderHex string) bool {
	ok := backend.IsAllowed(b.cfg.AllowedUsers, senderHex)
	if !ok {
		slog.Debug("nostr: dropping DM from non-allowed user", "sender", senderHex)
	}

	return ok
}

// claimConversation tries to set the active conversation to senderHex.
// Returns false if a different conversation is already active.
func (b *Backend) claimConversation(senderHex string) bool {
	ok := b.active.Claim(senderHex)
	if !ok {
		slog.Info("nostr: dropping DM, different active conversation", "sender", senderHex)
	}

	return ok
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

// hashContent returns a hex-encoded SHA-256 of the event content and
// the target relay list, so a change in either triggers re-publishing.
func hashContent(content string, relays []string) string {
	h := sha256.New()
	h.Write([]byte(content))
	h.Write([]byte{0})

	for _, r := range relays {
		h.Write([]byte(r))
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
}

// metadataIsFresh returns true if the given kind was already published
// with the same hash within metadataRefreshInterval.
func (b *Backend) metadataIsFresh(ctx context.Context, kind int64, hash string) bool {
	row, err := b.db.queries.GetPublishedMetadata(ctx, kind)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false // never published
		}

		slog.Warn("nostr: failed to check published metadata cache", "kind", kind, "error", err)

		return false // publish to be safe
	}

	if row.Hash != hash {
		return false // content or target relays changed
	}

	publishedAt := time.Unix(row.PublishedAt, 0)

	return time.Since(publishedAt) < metadataRefreshInterval
}

// markMetadataPublished records that a metadata event was published with
// the given content hash.
func (b *Backend) markMetadataPublished(ctx context.Context, kind int64, hash string) {
	if err := b.db.queries.UpsertPublishedMetadata(ctx, UpsertPublishedMetadataParams{
		Kind:        kind,
		Hash:        hash,
		PublishedAt: time.Now().Unix(),
	}); err != nil {
		slog.Warn("nostr: failed to cache published metadata", "kind", kind, "error", err)
	}
}

package nostr

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip17"
	"fiatjaf.com/nostr/nip59"
)

func (b *Backend) sendDM(ctx context.Context, kr gonostr.Keyer, pool *gonostr.Pool, recipientPK gonostr.PubKey, text string, extraTags gonostr.Tags) string {
	rumor, err := buildRumor(ctx, kr, gonostr.KindDirectMessage, text, recipientPK, extraTags)
	if err != nil {
		slog.Error("nostr: failed to build DM rumor", "recipient", recipientPK.Hex(), "error", err)

		return ""
	}

	if !b.wrapAndPublish(ctx, kr, pool, rumor, recipientPK, "DM") {
		return ""
	}

	return rumor.ID.Hex()
}

// sendFileMessage sends a NIP-17 kind 15 file message. The URL is the rumor
// content and extraTags carry file metadata (mime type, encryption params).
func (b *Backend) sendFileMessage(ctx context.Context, kr gonostr.Keyer, pool *gonostr.Pool, recipientPK gonostr.PubKey, fileURL string, extraTags gonostr.Tags) {
	rumor, err := buildRumor(ctx, kr, KindFileMessage, fileURL, recipientPK, extraTags)
	if err != nil {
		slog.Error("nostr: failed to build file rumor", "recipient", recipientPK.Hex(), "error", err)

		return
	}

	b.wrapAndPublish(ctx, kr, pool, rumor, recipientPK, "file msg")
}

// buildRumor constructs an unsigned rumor event of the given kind with a
// "p" tag for the recipient plus any extra tags, and computes its ID.
func buildRumor(ctx context.Context, kr gonostr.Keyer, kind gonostr.Kind, content string, recipientPK gonostr.PubKey, extraTags gonostr.Tags) (gonostr.Event, error) {
	ourPubkey, err := kr.GetPublicKey(ctx)
	if err != nil {
		return gonostr.Event{}, fmt.Errorf("getting public key: %w", err)
	}

	tags := make(gonostr.Tags, 0, len(extraTags)+1)
	tags = append(tags, gonostr.Tag{"p", recipientPK.Hex()})
	tags = append(tags, extraTags...)

	rumor := gonostr.Event{
		Kind:      kind,
		Content:   content,
		Tags:      tags,
		CreatedAt: gonostr.Now(),
		PubKey:    ourPubkey,
	}
	rumor.ID = rumor.GetID()

	return rumor, nil
}

// wrapAndPublish gift-wraps the rumor twice (for us and the recipient)
// and enqueues both copies to the appropriate relays. Returns false if
// either wrap step failed.
func (b *Backend) wrapAndPublish(ctx context.Context, kr gonostr.Keyer, pool *gonostr.Pool, rumor gonostr.Event, recipientPK gonostr.PubKey, label string) bool {
	toUs, err := giftWrap(ctx, kr, rumor, rumor.PubKey)
	if err != nil {
		slog.Error("nostr: failed to gift-wrap "+label+" (toUs)", "recipient", recipientPK.Hex(), "error", err)

		return false
	}

	toThem, err := giftWrap(ctx, kr, rumor, recipientPK)
	if err != nil {
		slog.Error("nostr: failed to gift-wrap "+label+" (toThem)", "recipient", recipientPK.Hex(), "error", err)

		return false
	}

	b.publishDMGiftWraps(ctx, pool, toUs, toThem, recipientPK)

	return true
}

// giftWrap wraps a rumor for target using kr's encrypt/sign callbacks.
// Factors out the closure boilerplate every nip59.GiftWrap call repeats.
func giftWrap(ctx context.Context, kr gonostr.Keyer, rumor gonostr.Event, target gonostr.PubKey) (gonostr.Event, error) {
	evt, err := nip59.GiftWrap(rumor, target,
		func(s string) (string, error) { return kr.Encrypt(ctx, s, target) },
		func(e *gonostr.Event) error { return kr.SignEvent(ctx, e) },
		nil,
	)
	if err != nil {
		return evt, fmt.Errorf("gift-wrapping for %s: %w", target.Hex(), err)
	}

	return evt, nil
}

// publishDMGiftWraps publishes the two gift-wrap copies to the appropriate relays.
func (b *Backend) publishDMGiftWraps(ctx context.Context, pool *gonostr.Pool, toUs, toThem gonostr.Event, recipientPK gonostr.PubKey) {
	// Publish our copy to our DM relays.
	b.pubQueue.enqueue(ctx, toUs, b.cfg.DMRelays, "DM toUs")

	theirRelays := b.recipientRelays(ctx, recipientPK, pool)

	slog.Debug("nostr: sending DM", "recipient", recipientPK.Hex(), "their_relays", theirRelays)

	b.pubQueue.enqueue(ctx, toThem, theirRelays, "DM toThem")
}

// sendReaction sends a NIP-25 kind 7 reaction gift-wrapped via NIP-59 to
// acknowledge receipt of an incoming DM. The reaction references the original
// rumor by its event ID so the sender's client can attach it to the right message.
func (b *Backend) sendReaction(ctx context.Context, rumorID gonostr.ID, recipientPK gonostr.PubKey, rumorKind gonostr.Kind) {
	rumor := gonostr.Event{
		Kind:      gonostr.KindReaction,
		Content:   "👍",
		CreatedAt: gonostr.Now(),
		PubKey:    b.keys.PK,
		Tags: gonostr.Tags{
			{"e", rumorID.Hex()},
			{"p", recipientPK.Hex()},
			{"k", strconv.Itoa(int(rumorKind))},
		},
	}
	rumor.ID = rumor.GetID()

	toThem, err := giftWrap(ctx, b.kr, rumor, recipientPK)
	if err != nil {
		slog.Warn("nostr: failed to gift-wrap reaction", "recipient", recipientPK.Hex(), "error", err)

		return
	}

	b.pubQueue.enqueue(ctx, toThem, b.recipientRelays(ctx, recipientPK, b.pool), "reaction")
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

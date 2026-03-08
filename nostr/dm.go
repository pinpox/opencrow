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
	b.publishToRelays(ctx, toUs, b.cfg.DMRelays, "DM toUs")

	theirRelays := b.recipientRelays(ctx, recipientPK, pool)

	slog.Debug("nostr: sending DM", "recipient", recipientPK.Hex(), "their_relays", theirRelays)

	b.publishToRelays(ctx, toThem, theirRelays, "DM toThem")
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

	b.publishToRelays(ctx, toThem, b.recipientRelays(ctx, recipientPK, b.pool), "reaction")
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
func (b *Backend) publishToRelays(ctx context.Context, evt gonostr.Event, relays []string, label string) {
	b.pubQueue.enqueue(ctx, evt, relays, label)
}

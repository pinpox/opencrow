package nostr

import (
	"context"
	"log/slog"
	"time"

	gonostr "fiatjaf.com/nostr"
)

// nip59SafetyMargin accounts for NIP-59's randomized created_at timestamps
// which may be up to 2 days in the past. We add an extra day of margin.
const nip59SafetyMargin = 3 * 24 * time.Hour

// subscriptionSince returns a Nostr timestamp suitable for the Since filter
// on relay subscriptions. It looks up the newest seen rumor in the database
// and subtracts nip59SafetyMargin to account for NIP-59's randomized
// created_at. Falls back to now minus the margin when the DB is empty.
func subscriptionSince(ctx context.Context, db *DB) gonostr.Timestamp {
	newest := time.Now()

	ts, err := db.queries.NewestSeenRumor(ctx)
	if err != nil {
		slog.Warn("nostr: failed to query newest seen rumor", "error", err)
	} else if ts > 0 {
		newest = time.Unix(ts, 0)
	}

	since := newest.Add(-nip59SafetyMargin)

	return max(gonostr.Timestamp(since.Unix()), 0)
}

// checkAndMarkSeen atomically inserts a rumor ID if it doesn't already exist.
// Returns true if the rumor was already in the database (i.e. duplicate).
func checkAndMarkSeen(ctx context.Context, db *DB, id string) (bool, error) {
	rowsAffected, err := db.queries.InsertSeenRumorIfNew(ctx, InsertSeenRumorIfNewParams{
		ID:   id,
		Seen: time.Now().Unix(),
	})
	if err != nil {
		return false, err
	}

	// INSERT OR IGNORE: 0 rows affected means the row already existed.
	return rowsAffected == 0, nil
}

// pruneStaleSeenRumors removes entries older than maxAge from the database.
func pruneStaleSeenRumors(ctx context.Context, db *DB, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge).Unix()

	if err := db.queries.PruneSeenRumors(ctx, cutoff); err != nil {
		slog.Warn("nostr: failed to prune seen_rumors", "error", err)
	}
}

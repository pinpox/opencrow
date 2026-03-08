package nostr

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
)

const legacySeenRumorsFile = ".nostr_seen_rumors"

// legacySeenRumorsEntry matches the old JSON format.
type legacySeenRumorsEntry struct {
	ID   string `json:"id"`
	Seen int64  `json:"seen"`
}

// migrateLegacySeenRumors imports the old JSON-based seen_rumors file into
// SQLite on first run, then removes the file. This prevents re-processing
// of already-handled DMs after the upgrade.
func migrateLegacySeenRumors(ctx context.Context, db *DB, baseDir string) {
	if baseDir == "" {
		return
	}

	path := filepath.Join(baseDir, legacySeenRumorsFile)

	data, err := os.ReadFile(path)
	if err != nil {
		return // file doesn't exist or unreadable — nothing to migrate
	}

	var entries []legacySeenRumorsEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("nostr: failed to parse legacy seen_rumors for migration", "error", err)

		return
	}

	if len(entries) == 0 {
		os.Remove(path)

		return
	}

	imported := 0

	for _, e := range entries {
		if err := db.queries.UpsertSeenRumor(ctx, UpsertSeenRumorParams(e)); err != nil {
			slog.Warn("nostr: failed to migrate seen_rumor", "id", e.ID, "error", err)

			continue
		}

		imported++
	}

	slog.Info("nostr: migrated legacy seen_rumors to sqlite", "imported", imported, "total", len(entries))

	if imported < len(entries) {
		slog.Error("nostr: partial migration, keeping legacy file for manual recovery",
			"failed", len(entries)-imported, "path", path)

		return
	}

	if err := os.Remove(path); err != nil {
		slog.Warn("nostr: failed to remove legacy seen_rumors file", "error", err)
	}
}

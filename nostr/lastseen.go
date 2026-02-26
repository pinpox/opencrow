package nostr

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gonostr "fiatjaf.com/nostr"
)

const lastSeenFile = ".nostr_last_seen"

// loadLastSeen reads the persisted last-seen DM timestamp.
// Falls back to 3 days ago if the file doesn't exist or is unreadable.
func loadLastSeen(baseDir string) gonostr.Timestamp {
	fallback := gonostr.Timestamp(time.Now().Add(-3 * 24 * time.Hour).Unix())

	data, err := os.ReadFile(filepath.Join(baseDir, lastSeenFile))
	if err != nil {
		return fallback
	}

	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return fallback
	}

	return gonostr.Timestamp(v)
}

// saveLastSeen writes the last-seen DM timestamp to disk.
func saveLastSeen(baseDir string, ts gonostr.Timestamp) {
	path := filepath.Join(baseDir, lastSeenFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("nostr: failed to create dir for last_seen", "error", err)
		return
	}
	data := []byte(strconv.FormatInt(int64(ts), 10) + "\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn("nostr: failed to save last_seen", "error", err)
	}
}

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// HeartbeatScheduler periodically checks HEARTBEAT.md in each room's session
// directory and prompts the pi process if there are tasks to attend to.
// It also watches for trigger files from external processes.
type HeartbeatScheduler struct {
	pool      *PiPool
	cfg       HeartbeatConfig
	piCfg     PiConfig
	sendReply func(ctx context.Context, roomID string, text string)
	mu        sync.Mutex
	lastBeat  map[string]time.Time
}

// NewHeartbeatScheduler creates a new heartbeat scheduler.
func NewHeartbeatScheduler(
	pool *PiPool,
	piCfg PiConfig,
	hbCfg HeartbeatConfig,
	sendReply func(ctx context.Context, roomID string, text string),
) *HeartbeatScheduler {
	return &HeartbeatScheduler{
		pool:      pool,
		cfg:       hbCfg,
		piCfg:     piCfg,
		sendReply: sendReply,
		lastBeat:  make(map[string]time.Time),
	}
}

// Start begins the heartbeat loop. It ticks every minute, checking each room
// for due heartbeats or trigger files. Stops when ctx is cancelled.
func (h *HeartbeatScheduler) Start(ctx context.Context) {
	if h.cfg.Interval <= 0 {
		slog.Info("heartbeat disabled (interval not set)")

		return
	}

	slog.Info("heartbeat scheduler started", "interval", h.cfg.Interval)

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.tickAll(ctx)
			}
		}
	}()
}

// tickAll checks all rooms and trigger files.
func (h *HeartbeatScheduler) tickAll(ctx context.Context) {
	// Collect rooms with live pi processes
	rooms := h.pool.Rooms()

	// Check for trigger files
	triggers := h.readTriggers()

	// Scan session directories for rooms with a HEARTBEAT.md on disk
	heartbeatRooms := h.scanSessionDirs()

	// Merge all room sources
	roomSet := make(map[string]struct{})
	for _, r := range rooms {
		roomSet[r] = struct{}{}
	}

	for _, r := range heartbeatRooms {
		roomSet[r] = struct{}{}
	}

	for r := range triggers {
		roomSet[r] = struct{}{}
	}

	h.mu.Lock()
	now := time.Now()
	h.mu.Unlock()

	for roomID := range roomSet {
		h.ensureTriggerDir(roomID)

		triggerCtx := triggers[roomID]

		h.mu.Lock()
		last := h.lastBeat[roomID]
		due := time.Since(last) >= h.cfg.Interval
		h.mu.Unlock()

		if triggerCtx != "" || due {
			h.tick(ctx, roomID, triggerCtx)

			h.mu.Lock()
			h.lastBeat[roomID] = now
			h.mu.Unlock()
		}
	}
}

// readTriggers walks session subdirectories, reads all files from each
// room's triggers/ spool directory, concatenates their contents, and deletes
// them. Returns a map of room ID -> combined trigger content.
func (h *HeartbeatScheduler) readTriggers() map[string]string {
	triggers := make(map[string]string)

	sessionEntries, err := os.ReadDir(h.piCfg.SessionDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read session directory", "path", h.piCfg.SessionDir, "error", err)
		}

		return triggers
	}

	for _, entry := range sessionEntries {
		if !entry.IsDir() {
			continue
		}

		dir := filepath.Join(h.piCfg.SessionDir, entry.Name())

		// Read the original room ID
		data, readErr := os.ReadFile(filepath.Join(dir, ".room_id"))
		if readErr != nil {
			continue
		}

		roomID := strings.TrimSpace(string(data))
		if roomID == "" {
			continue
		}

		triggerDir := filepath.Join(dir, "triggers")

		triggerFiles, dirErr := os.ReadDir(triggerDir)
		if dirErr != nil {
			continue
		}

		var parts []string

		for _, tf := range triggerFiles {
			if tf.IsDir() {
				continue
			}

			tfPath := filepath.Join(triggerDir, tf.Name())

			content, rfErr := os.ReadFile(tfPath)
			if rfErr != nil {
				slog.Warn("failed to read trigger file", "path", tfPath, "error", rfErr)

				continue
			}

			if removeErr := os.Remove(tfPath); removeErr != nil {
				slog.Warn("failed to remove trigger file", "path", tfPath, "error", removeErr)
			}

			if s := strings.TrimSpace(string(content)); s != "" {
				parts = append(parts, s)
			}
		}

		if len(parts) > 0 {
			triggers[roomID] = strings.Join(parts, "\n\n")
		}
	}

	return triggers
}

// ensureTriggerDir creates the triggers/ spool directory inside a room's
// session directory so external writers can assume it exists.
func (h *HeartbeatScheduler) ensureTriggerDir(roomID string) {
	dir := filepath.Join(h.piCfg.SessionDir, sanitizeRoomID(roomID), "triggers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("failed to create trigger directory", "room", roomID, "path", dir, "error", err)
	}
}

// scanSessionDirs walks the session directory and returns room IDs for any
// session that has a HEARTBEAT.md file on disk. This ensures heartbeats fire
// even when the pi process has been reaped due to idle timeout.
func (h *HeartbeatScheduler) scanSessionDirs() []string {
	entries, err := os.ReadDir(h.piCfg.SessionDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read session directory", "path", h.piCfg.SessionDir, "error", err)
		}

		return nil
	}

	var rooms []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dir := filepath.Join(h.piCfg.SessionDir, entry.Name())

		// Check if HEARTBEAT.md exists
		hbPath := filepath.Join(dir, "HEARTBEAT.md")
		if _, statErr := os.Stat(hbPath); statErr != nil {
			continue
		}

		// Read the original room ID written by StartPi
		roomIDPath := filepath.Join(dir, ".room_id")

		data, readErr := os.ReadFile(roomIDPath)
		if readErr != nil {
			continue
		}

		if roomID := strings.TrimSpace(string(data)); roomID != "" {
			rooms = append(rooms, roomID)
		}
	}

	return rooms
}

// tick performs a single heartbeat for a room.
func (h *HeartbeatScheduler) tick(ctx context.Context, roomID string, triggerContext string) {
	sessionDir := filepath.Join(h.piCfg.SessionDir, sanitizeRoomID(roomID))
	heartbeatPath := filepath.Join(sessionDir, "HEARTBEAT.md")

	heartbeatContent, err := os.ReadFile(heartbeatPath)
	if err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to read HEARTBEAT.md", "room", roomID, "path", heartbeatPath, "error", err)
	}

	content := strings.TrimSpace(string(heartbeatContent))

	// If no heartbeat file content and no trigger, skip
	if isEffectivelyEmpty(content) && triggerContext == "" {
		return
	}

	slog.Info("heartbeat firing", "room", roomID, "has_heartbeat_md", !isEffectivelyEmpty(content), "has_trigger", triggerContext != "")

	pi, err := h.pool.Get(ctx, roomID)
	if err != nil {
		slog.Error("heartbeat: failed to get pi process", "room", roomID, "error", err)

		return
	}

	prompt := buildHeartbeatPrompt(h.cfg.Prompt, content, triggerContext)

	reply, err := pi.PromptNoTouch(ctx, prompt)
	if err != nil {
		slog.Error("heartbeat: pi prompt failed", "room", roomID, "error", err)
		h.pool.Remove(roomID)

		return
	}

	if containsHeartbeatOK(reply) {
		slog.Info("heartbeat: HEARTBEAT_OK, suppressing", "room", roomID)

		return
	}

	if reply == "" {
		slog.Info("heartbeat: empty response, suppressing", "room", roomID)

		return
	}

	h.sendReply(ctx, roomID, reply)
}

func buildHeartbeatPrompt(basePrompt, content, triggerContext string) string {
	var prompt strings.Builder

	prompt.WriteString(basePrompt)

	if !isEffectivelyEmpty(content) {
		prompt.WriteString("\n\n--- HEARTBEAT.md contents ---\n")
		prompt.WriteString(content)
		prompt.WriteString("\n--- end HEARTBEAT.md ---")
	}

	if triggerContext != "" {
		prompt.WriteString("\n\n--- External trigger ---\n")
		prompt.WriteString(triggerContext)
		prompt.WriteString("\n--- end trigger ---")
	}

	return prompt.String()
}

// isEffectivelyEmpty returns true if the content contains only headers,
// blank lines, and empty list items.
func isEffectivelyEmpty(content string) bool {
	if content == "" {
		return true
	}

	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)

		if line == "" || isMarkdownHeader(line) || isEmptyListItem(line) {
			continue
		}

		// Found a non-empty, non-structural line
		return false
	}

	return true
}

func isMarkdownHeader(line string) bool {
	return strings.HasPrefix(line, "#")
}

func isEmptyListItem(line string) bool {
	// Bare bullet markers
	if line == "-" || line == "*" || line == "+" {
		return true
	}

	// Bullet followed by only whitespace
	for _, prefix := range []string{"- ", "* ", "+ "} {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(after) == ""
		}
	}

	return false
}

// containsHeartbeatOK checks if the response is essentially just HEARTBEAT_OK.
// It strips the token (including bold markers) and returns true if the remaining
// content is negligible (< 50 non-whitespace characters).
func containsHeartbeatOK(response string) bool {
	if response == "" {
		return false
	}

	// Strip HEARTBEAT_OK token (with optional bold markers)
	cleaned := response
	cleaned = strings.ReplaceAll(cleaned, "**HEARTBEAT_OK**", "")
	cleaned = strings.ReplaceAll(cleaned, "HEARTBEAT_OK", "")

	// Count non-whitespace chars remaining
	count := 0

	for _, r := range cleaned {
		if !unicode.IsSpace(r) {
			count++

			if count >= 50 {
				return false
			}
		}
	}

	return true
}

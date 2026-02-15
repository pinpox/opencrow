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

	if err := os.MkdirAll(h.cfg.TriggerDir, 0o755); err != nil {
		slog.Error("failed to create trigger directory", "path", h.cfg.TriggerDir, "error", err)
	}

	slog.Info("heartbeat scheduler started", "interval", h.cfg.Interval, "trigger_dir", h.cfg.TriggerDir)

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

	// Merge trigger room IDs with live rooms
	roomSet := make(map[string]struct{})
	for _, r := range rooms {
		roomSet[r] = struct{}{}
	}

	for r := range triggers {
		roomSet[r] = struct{}{}
	}

	h.mu.Lock()
	now := time.Now()
	h.mu.Unlock()

	for roomID := range roomSet {
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

// readTriggers reads and removes trigger files from the trigger directory.
// Returns a map of room ID -> trigger content.
func (h *HeartbeatScheduler) readTriggers() map[string]string {
	triggers := make(map[string]string)

	entries, err := os.ReadDir(h.cfg.TriggerDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read trigger directory", "path", h.cfg.TriggerDir, "error", err)
		}

		return triggers
	}

	for _, entry := range entries {
		name := entry.Name()

		if !strings.HasSuffix(name, ".trigger") {
			continue
		}

		roomID := strings.TrimSuffix(name, ".trigger")
		path := filepath.Join(h.cfg.TriggerDir, name)

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			slog.Warn("failed to read trigger file", "path", path, "error", readErr)

			continue
		}

		if removeErr := os.Remove(path); removeErr != nil {
			slog.Warn("failed to remove trigger file", "path", path, "error", removeErr)
		}

		content := strings.TrimSpace(string(data))
		if content != "" {
			triggers[roomID] = content
		}
	}

	return triggers
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

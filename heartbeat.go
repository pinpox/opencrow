package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HeartbeatScheduler periodically checks HEARTBEAT.md in each room's session
// directory and prompts the pi process if there are tasks to attend to.
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
// for due heartbeats. Stops when ctx is cancelled.
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

// tickAll checks all rooms for due heartbeats.
func (h *HeartbeatScheduler) tickAll(ctx context.Context) {
	// Collect rooms with live pi processes
	rooms := h.pool.Rooms()

	// Check the session directory for a HEARTBEAT.md on disk
	heartbeatRooms := h.scanSessionDir()

	// Merge all room sources
	roomSet := make(map[string]struct{})
	for _, r := range rooms {
		roomSet[r] = struct{}{}
	}

	for _, r := range heartbeatRooms {
		roomSet[r] = struct{}{}
	}

	h.mu.Lock()
	now := time.Now()
	h.mu.Unlock()

	for roomID := range roomSet {
		h.mu.Lock()
		last := h.lastBeat[roomID]
		due := time.Since(last) >= h.cfg.Interval
		h.mu.Unlock()

		if due {
			h.tick(ctx, roomID)

			h.mu.Lock()
			h.lastBeat[roomID] = now
			h.mu.Unlock()
		}
	}
}

// scanSessionDir checks the session directory for a HEARTBEAT.md file on disk.
// This ensures heartbeats fire even when the pi process has been reaped due to
// idle timeout.
func (h *HeartbeatScheduler) scanSessionDir() []string {
	hbPath := filepath.Join(h.piCfg.SessionDir, "HEARTBEAT.md")
	if _, err := os.Stat(hbPath); err != nil {
		return nil
	}

	roomIDPath := filepath.Join(h.piCfg.SessionDir, ".room_id")

	data, err := os.ReadFile(roomIDPath)
	if err != nil {
		return nil
	}

	if roomID := strings.TrimSpace(string(data)); roomID != "" {
		return []string{roomID}
	}

	return nil
}

// tick performs a single heartbeat for a room.
func (h *HeartbeatScheduler) tick(ctx context.Context, roomID string) {
	heartbeatPath := filepath.Join(h.piCfg.SessionDir, "HEARTBEAT.md")

	heartbeatContent, err := os.ReadFile(heartbeatPath)
	if err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to read HEARTBEAT.md", "room", roomID, "path", heartbeatPath, "error", err)
	}

	content := strings.TrimSpace(string(heartbeatContent))

	// If no heartbeat file content, skip
	if isEffectivelyEmpty(content) {
		return
	}

	slog.Info("heartbeat firing", "room", roomID)

	pi, err := h.pool.Get(ctx, roomID)
	if err != nil {
		slog.Error("heartbeat: failed to get pi process", "room", roomID, "error", err)

		return
	}

	prompt := buildHeartbeatPrompt(h.cfg.Prompt, content)

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

func buildHeartbeatPrompt(basePrompt, content string) string {
	var prompt strings.Builder

	prompt.WriteString(basePrompt)

	if !isEffectivelyEmpty(content) {
		prompt.WriteString("\n\n--- HEARTBEAT.md contents ---\n")
		prompt.WriteString(content)
		prompt.WriteString("\n--- end HEARTBEAT.md ---")
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

// containsHeartbeatOK checks if the response contains HEARTBEAT_OK.
// If the LLM included the token anywhere in its reply, it's signaling that
// nothing actionable needs to reach the user — suppress the entire message.
func containsHeartbeatOK(response string) bool {
	return strings.Contains(response, "HEARTBEAT_OK")
}

package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TriggerPipeManager manages per-room named pipes (FIFOs) for immediate
// trigger delivery from external processes.
type TriggerPipeManager struct {
	pool      *PiPool
	piCfg     PiConfig
	prompt    string
	sendReply func(ctx context.Context, roomID string, text string)
	setTyping func(ctx context.Context, roomID string, typing bool)
	mu        sync.Mutex
	readers   map[string]context.CancelFunc
}

// NewTriggerPipeManager creates a new trigger pipe manager.
func NewTriggerPipeManager(
	pool *PiPool,
	piCfg PiConfig,
	prompt string,
	sendReply func(ctx context.Context, roomID string, text string),
	setTyping func(ctx context.Context, roomID string, typing bool),
) *TriggerPipeManager {
	return &TriggerPipeManager{
		pool:      pool,
		piCfg:     piCfg,
		prompt:    prompt,
		sendReply: sendReply,
		setTyping: setTyping,
		readers:   make(map[string]context.CancelFunc),
	}
}

// Start begins the trigger pipe manager. It performs an initial scan for
// existing session directories and then re-scans every minute to pick up
// new rooms.
func (t *TriggerPipeManager) Start(ctx context.Context) {
	slog.Info("trigger pipe manager started")

	t.syncReaders(ctx)

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				t.stopAll()

				return
			case <-ticker.C:
				t.syncReaders(ctx)
			}
		}
	}()
}

// StartRoom ensures a reader goroutine exists for the given room.
func (t *TriggerPipeManager) StartRoom(ctx context.Context, roomID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.readers[roomID]; ok {
		return
	}

	pipePath := TriggerPipePath(t.piCfg.SessionDir)

	if err := ensureFIFO(pipePath); err != nil {
		slog.Warn("trigger: failed to ensure FIFO", "path", pipePath, "error", err)

		return
	}

	roomCtx, cancel := context.WithCancel(ctx)
	t.readers[roomID] = cancel

	go t.readLoop(roomCtx, roomID, pipePath)
}

// syncReaders checks the session directory for a room and starts a reader
// if one doesn't exist yet.
func (t *TriggerPipeManager) syncReaders(ctx context.Context) {
	roomIDPath := filepath.Join(t.piCfg.SessionDir, ".room_id")

	data, err := os.ReadFile(roomIDPath)
	if err != nil {
		return
	}

	roomID := strings.TrimSpace(string(data))
	if roomID == "" {
		return
	}

	t.StartRoom(ctx, roomID)
}

// stopAll cancels all reader goroutines.
func (t *TriggerPipeManager) stopAll() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for roomID, cancel := range t.readers {
		cancel()
		delete(t.readers, roomID)
	}
}

// readLoop is a per-room goroutine that reads lines from a named pipe
// and processes each as a trigger.
func (t *TriggerPipeManager) readLoop(ctx context.Context, roomID, pipePath string) {
	defer func() {
		t.mu.Lock()
		delete(t.readers, roomID)
		t.mu.Unlock()
	}()

	// Open with O_RDWR so the fd stays open even when writers close their end.
	f, err := os.OpenFile(pipePath, os.O_RDWR, 0)
	if err != nil {
		slog.Error("trigger: failed to open FIFO", "path", pipePath, "error", err)

		return
	}
	defer f.Close()

	// Watch for context cancellation and close the fd to unblock the scanner.
	go func() {
		<-ctx.Done()
		f.Close()
	}()

	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if ctx.Err() != nil {
			return
		}

		slog.Info("trigger: received", "content", line)
		t.processTrigger(ctx, roomID, line)
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("trigger: scanner error", "error", err)
	}
}

// processTrigger sends a trigger message to pi and delivers the reply.
func (t *TriggerPipeManager) processTrigger(ctx context.Context, roomID, content string) {
	t.setTyping(ctx, roomID, true)
	defer t.setTyping(ctx, roomID, false)

	pi, err := t.pool.Get(ctx, roomID)
	if err != nil {
		slog.Error("trigger: failed to get pi process", "error", err)

		return
	}

	prompt := buildTriggerPrompt(t.prompt, content)

	reply, err := pi.PromptNoTouch(ctx, prompt)
	if err != nil {
		slog.Error("trigger: pi prompt failed", "error", err)
		t.pool.Remove(roomID)

		return
	}

	if containsHeartbeatOK(reply) {
		slog.Info("trigger: HEARTBEAT_OK, suppressing")

		return
	}

	if reply == "" {
		slog.Info("trigger: empty response, suppressing")

		return
	}

	t.sendReply(ctx, roomID, reply)
}

// TriggerPipePath returns the path to the trigger FIFO.
func TriggerPipePath(sessionDir string) string {
	return filepath.Join(sessionDir, "trigger.pipe")
}

// ensureFIFO creates a named pipe at the given path if it doesn't exist.
// If a non-FIFO file exists at the path, it is replaced.
func ensureFIFO(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating FIFO parent dir: %w", err)
	}

	info, err := os.Lstat(path)
	if err == nil {
		// File exists — check if it's already a FIFO
		if info.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}
		// Not a FIFO, remove it
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing non-FIFO at %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking FIFO at %s: %w", path, err)
	}

	if err := syscall.Mkfifo(path, 0o664); err != nil {
		return fmt.Errorf("creating FIFO at %s: %w", path, err)
	}

	return nil
}

func buildTriggerPrompt(basePrompt, content string) string {
	var prompt strings.Builder

	prompt.WriteString(basePrompt)
	prompt.WriteString("\n\n--- External trigger ---\n")
	prompt.WriteString(content)
	prompt.WriteString("\n--- end trigger ---")

	return prompt.String()
}

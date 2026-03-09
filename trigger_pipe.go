package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// startTriggerPipe reads lines from a named pipe (FIFO) and enqueues
// them into the inbox. Runs until ctx is cancelled.
func startTriggerPipe(ctx context.Context, w *Worker, sessionDir string) {
	pipePath := TriggerPipePath(sessionDir)

	if err := ensureFIFO(pipePath); err != nil {
		slog.Error("trigger: failed to ensure FIFO", "path", pipePath, "error", err)

		return
	}

	slog.Info("trigger pipe reader started", "path", pipePath)

	go triggerReadLoop(ctx, w, pipePath)
}

func triggerReadLoop(ctx context.Context, w *Worker, pipePath string) {
	// Open with O_RDWR so the fd stays open even when writers close their end.
	f, err := os.OpenFile(pipePath, os.O_RDWR, 0)
	if err != nil {
		slog.Error("trigger: failed to open FIFO", "path", pipePath, "error", err)

		return
	}
	defer f.Close()

	stop := context.AfterFunc(ctx, func() { f.Close() })
	defer stop()

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

		if err := w.inbox.Enqueue(ctx, PriorityTrigger, sourceTrigger, line, ""); err != nil {
			slog.Error("trigger: failed to enqueue", "error", err)
		}

		w.Notify(PriorityTrigger)
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("trigger: scanner error", "error", err)
	}
}

// TriggerPipePath returns the path to the trigger FIFO.
func TriggerPipePath(sessionDir string) string {
	return filepath.Join(sessionDir, "trigger.pipe")
}

func ensureFIFO(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating FIFO parent dir: %w", err)
	}

	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}

		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing non-FIFO at %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
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

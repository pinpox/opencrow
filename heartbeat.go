package main

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// startHeartbeat periodically enqueues heartbeat markers into the inbox.
// The worker reads HEARTBEAT.md at dequeue time and decides whether to
// prompt pi. Blocks until ctx is cancelled.
func startHeartbeat(ctx context.Context, w *Worker, cfg HeartbeatConfig) {
	if cfg.Interval <= 0 {
		slog.Info("heartbeat disabled (interval not set)")

		return
	}

	slog.Info("heartbeat scheduler started", "interval", cfg.Interval)

	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				inserted, err := w.inbox.EnqueueHeartbeat(ctx)
				if err != nil {
					slog.Error("heartbeat: failed to enqueue", "error", err)

					continue
				}

				if !inserted {
					slog.Debug("heartbeat: skipping, one already queued")

					continue
				}

				w.Notify(PriorityHeartbeat)
			}
		}
	}()
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

		if line == "" || strings.HasPrefix(line, "#") || isEmptyListItem(line) {
			continue
		}

		return false
	}

	return true
}

func isEmptyListItem(line string) bool {
	if line == "-" || line == "*" || line == "+" {
		return true
	}

	for _, prefix := range []string{"- ", "* ", "+ "} {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(after) == ""
		}
	}

	return false
}

// shouldSuppressReply returns true if the reply should not be forwarded
// to the user — either because it contains HEARTBEAT_OK or is empty.
func shouldSuppressReply(reply, label string) bool {
	if strings.Contains(reply, "HEARTBEAT_OK") {
		slog.Info(label + ": HEARTBEAT_OK, suppressing")

		return true
	}

	if reply == "" {
		slog.Info(label + ": empty response, suppressing")

		return true
	}

	return false
}

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// reminderTick is how often we poll the reminders table for due items.
// Independent of the heartbeat interval so one-shot reminders fire with
// reasonable precision even when heartbeat is set to 30m or disabled.
const reminderTick = 1 * time.Minute

// startHeartbeat runs two background loops:
//   - a reminder dispatcher (every reminderTick) that fires due one-shot
//     reminders from the reminders table as trigger items
//   - a heartbeat ticker (every cfg.Interval, if > 0) that enqueues a
//     heartbeat marker so the worker sends the configured heartbeat prompt
func startHeartbeat(ctx context.Context, w *Worker, cfg HeartbeatConfig) {
	go reminderLoop(ctx, w)

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

// reminderLoop polls the reminders table and enqueues any due reminders
// as trigger items. The scheduler owns cleanup: DueReminders is a
// DELETE … RETURNING, so fired reminders are removed atomically.
func reminderLoop(ctx context.Context, w *Worker) {
	slog.Info("reminder dispatcher started", "tick", reminderTick)

	ticker := time.NewTicker(reminderTick)
	defer ticker.Stop()

	// Fire once immediately so already-due reminders don't wait a full tick
	// after process start.
	dispatchDueReminders(ctx, w)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dispatchDueReminders(ctx, w)
		}
	}
}

func dispatchDueReminders(ctx context.Context, w *Worker) {
	now := time.Now().UTC().Format(time.RFC3339)

	due, err := w.inbox.queries.DueReminders(ctx, now)
	if err != nil {
		slog.Error("reminder: failed to query due reminders", "error", err)

		return
	}

	for _, r := range due {
		slog.Info("reminder: firing", "id", r.ID, "fire_at", r.FireAt)

		content := fmt.Sprintf("Reminder (set for %s): %s", r.FireAt, r.Prompt)

		if err := w.inbox.Enqueue(ctx, PriorityTrigger, sourceTrigger, content, ""); err != nil {
			// DueReminders is DELETE…RETURNING, so the row is already gone.
			// Re-insert it so the next tick retries instead of silently
			// dropping the reminder.
			slog.Error("reminder: failed to enqueue, re-inserting", "id", r.ID, "error", err)

			if rerr := w.inbox.queries.InsertReminder(ctx, InsertReminderParams{
				FireAt: r.FireAt,
				Prompt: r.Prompt,
			}); rerr != nil {
				slog.Error("reminder: re-insert failed, reminder lost", "id", r.ID, "error", rerr)
			}

			continue
		}

		w.Notify(PriorityTrigger)
	}
}

// parseHeartbeatItems extracts active checklist items from HEARTBEAT.md.
// Only `- text` lines count; `- [paused] text` is skipped. Everything else
// (headers, blank lines, prose) is ignored. No completed/priority metadata —
// obsolete checks are deleted, not marked.
func parseHeartbeatItems(content string) []string {
	var items []string

	for line := range strings.SplitSeq(content, "\n") {
		text, ok := strings.CutPrefix(strings.TrimSpace(line), "- ")
		if !ok {
			continue
		}

		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "[paused]") {
			continue
		}

		items = append(items, text)
	}

	return items
}

func buildHeartbeatPrompt(basePrompt string, items []string) string {
	var sb strings.Builder

	sb.WriteString(basePrompt)
	sb.WriteString("\n\nStanding checks:\n")

	for _, it := range items {
		sb.WriteString("- ")
		sb.WriteString(it)
		sb.WriteByte('\n')
	}

	return sb.String()
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

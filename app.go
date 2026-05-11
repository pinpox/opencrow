package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pinpox/opencrow/backend"
)

var sendFileRe = regexp.MustCompile(`<sendfile>\s*(.*?)\s*</sendfile>`)

// extractSendFiles finds all <sendfile>/path</sendfile> tags in text,
// returns the cleaned text with tags stripped and the list of file paths.
func extractSendFiles(text string) (string, []string) {
	matches := sendFileRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return text, nil
	}

	var paths []string

	for _, m := range matches {
		p := strings.TrimSpace(m[1])
		if p != "" {
			paths = append(paths, p)
		}
	}

	cleaned := sendFileRe.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, paths
}

// App orchestrates the business logic: command handling, inbox enqueueing,
// and file extraction. It delegates transport concerns to a Backend.
type App struct {
	backend backend.Backend
	worker  *Worker
	inbox   *InboxStore
	outbox  *outboxStore
}

// NewApp creates a new App. The db connection is shared with the inbox
// and owned by the caller.
func NewApp(b backend.Backend, worker *Worker, inbox *InboxStore, db *sql.DB) *App {
	return &App{
		backend: b,
		worker:  worker,
		inbox:   inbox,
		outbox:  newOutboxStore(db),
	}
}

// HandleMessage is the backend.MessageHandler callback. It dispatches
// commands and enqueues normal messages into the inbox.
func (a *App) HandleMessage(ctx context.Context, msg backend.Message) {
	// Record the incoming message so future reply-to references can quote it.
	a.outbox.Put(ctx, msg.ConversationID, msg.MessageID, msg.Text)

	text := strings.TrimSpace(msg.Text)

	switch {
	case text == "!help":
		a.handleHelp(ctx, msg)
	case text == "!restart":
		a.handleRestart(ctx, msg)
	case text == "!stop":
		a.handleStop(ctx, msg)
	case text == "!compact":
		a.handleCompact(ctx, msg)
	case text == "!skills":
		a.handleSkills(ctx, msg)
	case text == "!models":
		a.handleModels(ctx, msg)
	case text == "!model" || strings.HasPrefix(text, "!model "):
		a.handleModel(ctx, msg, strings.TrimSpace(strings.TrimPrefix(text, "!model")))
	default:
		a.handlePrompt(ctx, msg)
	}
}

func (a *App) handleHelp(ctx context.Context, msg backend.Message) {
	help := "Available commands:\n" +
		"  !help    — Show this help message\n" +
		"  !restart — Kill the current session and start fresh\n" +
		"  !stop    — Abort the currently running agent turn\n" +
		"  !compact — Compact conversation context to reduce token usage\n" +
		"  !skills  — List loaded skills\n" +
		"  !models  — List available models\n" +
		"  !model <provider>/<id> — Switch to the given model"
	a.backend.SendMessage(ctx, msg.ConversationID, help, "")
}

func (a *App) handleRestart(ctx context.Context, msg backend.Message) {
	a.backend.ResetConversation(ctx, msg.ConversationID)
	a.worker.Restart()
	a.backend.SendMessage(ctx, msg.ConversationID, "Session restarted. Next message starts a fresh session (previous context discarded).", "")
}

func (a *App) handleStop(ctx context.Context, msg backend.Message) {
	if !a.worker.IsActive() {
		a.backend.SendMessage(ctx, msg.ConversationID, "No active session.", "")

		return
	}

	if a.worker.Abort() {
		a.backend.SendMessage(ctx, msg.ConversationID, "Aborted current operation.", "")
	} else {
		a.backend.SendMessage(ctx, msg.ConversationID, "Nothing running to stop.", "")
	}
}

func (a *App) handleCompact(ctx context.Context, msg backend.Message) {
	if !a.worker.IsActive() {
		a.backend.SendMessage(ctx, msg.ConversationID, "No active session to compact.", "")

		return
	}

	result, err := a.worker.Compact(ctx)
	if err != nil {
		slog.Error("compact failed", "conversation", msg.ConversationID, "error", err)
		a.backend.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Compaction failed: %v", err), "")

		return
	}

	reply := fmt.Sprintf("Compacted conversation (was %d tokens).\nSummary: %s", result.TokensBefore, result.Summary)
	a.backend.SendMessage(ctx, msg.ConversationID, reply, "")
}

func (a *App) handleSkills(ctx context.Context, msg backend.Message) {
	a.backend.SendMessage(ctx, msg.ConversationID, a.worker.SkillsSummary(), "")
}

// handleModels lists pi's configured models as a single text reply,
// marking the active one with a leading asterisk. Works on any backend
// because it only uses SendMessage; the socket backend additionally
// exposes the same data as a structured 'models' event for its GUI.
func (a *App) handleModels(ctx context.Context, msg backend.Message) {
	models, err := a.worker.ListModels(ctx)
	if err != nil {
		a.backend.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Failed to list models: %v", err), "")

		return
	}

	if len(models) == 0 {
		a.backend.SendMessage(ctx, msg.ConversationID, "No models configured.", "")

		return
	}

	var sb strings.Builder

	sb.WriteString("Available models (* = active):\n")

	for _, m := range models {
		marker := "  "
		if m.Active {
			marker = "* "
		}

		fmt.Fprintf(&sb, "%s%s/%s\n", marker, m.Provider, m.ID)
	}

	a.backend.SendMessage(ctx, msg.ConversationID, strings.TrimRight(sb.String(), "\n"), "")
}

// handleModel switches pi to the requested model. arg is the substring
// that followed "!model" with surrounding whitespace stripped; it is
// required to be "<provider>/<id>". The slash form mirrors the protocol
// shape (provider + modelId) and is unambiguous even when the same id
// is registered under multiple providers.
func (a *App) handleModel(ctx context.Context, msg backend.Message, arg string) {
	if arg == "" {
		a.backend.SendMessage(ctx, msg.ConversationID,
			"Usage: !model <provider>/<id>. Use !models to list available models.", "")

		return
	}

	provider, id, ok := strings.Cut(arg, "/")
	if !ok || provider == "" || id == "" {
		a.backend.SendMessage(ctx, msg.ConversationID,
			"Invalid model spec. Use !model <provider>/<id> (e.g. !model openai/gpt-4).", "")

		return
	}

	model, err := a.worker.SetModel(ctx, provider, id)
	if err != nil {
		a.backend.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Failed to set model: %v", err), "")

		return
	}

	// Push the post-switch list to any GUI clients on this backend so
	// their dropdowns reconcile without a manual reopen. Synchronous is
	// fine: handleModel runs on a backend-handler goroutine, not the
	// worker's drain loop, so BroadcastModels' inbox round-trip won't
	// deadlock. Backends without a structured model UI (matrix, nostr,
	// signal) don't implement modelsBroadcaster and the call is skipped.
	if mb, ok := a.backend.(modelsBroadcaster); ok {
		mb.BroadcastModels(ctx)
	}

	a.backend.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Model switched to %s/%s.", model.Provider, model.ID), "")
}

func (a *App) handlePrompt(ctx context.Context, msg backend.Message) {
	a.worker.SetRoomID(msg.ConversationID)

	promptText := a.buildPromptText(ctx, msg)

	if err := a.inbox.Enqueue(ctx, PriorityUser, sourceUser, promptText, msg.ReplyToID); err != nil {
		slog.Error("failed to enqueue user message", "error", err)
		a.backend.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Error: %v", err), "")

		return
	}

	a.worker.Notify(PriorityUser)
}

// buildPromptText prepends reply-quote context to the message text.
func (a *App) buildPromptText(ctx context.Context, msg backend.Message) string {
	promptText := msg.Text

	if msg.ReplyToID != "" {
		if quoted := a.outbox.Get(ctx, msg.ConversationID, msg.ReplyToID); quoted != "" {
			promptText = fmt.Sprintf("[user replied to message: %q]\n%s", quoted, promptText)
		} else {
			promptText = "[user replied to a message whose content is unavailable — ask for clarification if their message is unclear]\n" + promptText
		}
	}

	return promptText
}

// sendReplyWithFiles extracts <sendfile> tags, uploads each file, and
// sends the final text reply.
func (a *App) sendReplyWithFiles(ctx context.Context, conversationID, reply, replyToID string) {
	slog.Info("sending reply", "conversation", conversationID, "len", len(reply))
	slog.Debug("outgoing reply content", "conversation", conversationID, "content", reply)

	cleanReply, filePaths := extractSendFiles(reply)

	var fileSendErrors strings.Builder

	for _, fp := range filePaths {
		slog.Info("sending file", "conversation", conversationID, "path", fp)

		if err := a.backend.SendFile(ctx, conversationID, fp); err != nil {
			slog.Error("failed to send file", "conversation", conversationID, "path", fp, "error", err)
			fileSendErrors.WriteString(fmt.Sprintf("\n\n(failed to send file %s: %v)", filepath.Base(fp), err))
		}
	}

	cleanReply += fileSendErrors.String()

	if cleanReply != "" {
		sentID := a.backend.SendMessage(ctx, conversationID, cleanReply, replyToID)
		a.outbox.Put(ctx, conversationID, sentID, cleanReply)
	}
}

// formatToolCall produces a short human-readable summary of a tool invocation.
// The Markdown flavor controls whether commands and paths are wrapped in
// fenced code blocks / inline backticks (and whether fences carry a language
// hint) so backends that do not render Markdown do not leak raw syntax.
func formatToolCall(evt ToolCallEvent, flavor backend.MarkdownFlavor) string {
	switch evt.ToolName {
	case "bash":
		return formatBashCall(evt, flavor)
	case "read":
		return formatPathCall(evt, flavor, "📄 reading", "file")
	case "edit":
		return formatPathCall(evt, flavor, "✏️ editing", "file")
	case "write":
		return formatPathCall(evt, flavor, "📝 writing", "file")
	default:
		return "🔧 " + evt.ToolName
	}
}

func formatBashCall(evt ToolCallEvent, flavor backend.MarkdownFlavor) string {
	cmd, ok := evt.Args["command"].(string)
	if !ok {
		return "🔧 bash"
	}

	switch flavor {
	case backend.MarkdownFull:
		return fmt.Sprintf("🔧\n```sh\n%s\n```", cmd)
	case backend.MarkdownBasic:
		// No language hint: some clients (e.g. Nostr/0xchat)
		// render the hint literally instead of hiding it.
		return fmt.Sprintf("🔧\n```\n%s\n```", cmd)
	case backend.MarkdownNone:
		return "🔧 " + cmd
	default:
		return "🔧 " + cmd
	}
}

func formatPathCall(evt ToolCallEvent, flavor backend.MarkdownFlavor, prefix, fallback string) string {
	p, ok := evt.Args["path"].(string)
	if !ok {
		return prefix + " " + fallback
	}

	if flavor == backend.MarkdownNone {
		return prefix + " " + p
	}

	return prefix + " `" + p + "`"
}

// systemPrompt returns the full system prompt including backend-specific extras.
func (a *App) systemPrompt(basePrompt string) string {
	extra := a.backend.SystemPromptExtra()
	if extra == "" {
		return basePrompt
	}

	return strings.TrimRight(basePrompt, "\n") + "\n\n" + extra
}

package main

import (
	"context"
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

// App orchestrates the business logic: command handling, pi prompting,
// and file extraction. It delegates transport concerns to a Backend.
type App struct {
	backend    backend.Backend
	pool       *PiPool
	triggerMgr *TriggerPipeManager
	sent       *sentMessageStore
}

// NewApp creates a new App. dataDir is used for persistent state (e.g.,
// sent message store). The triggerMgr may be nil if not used.
func NewApp(b backend.Backend, pool *PiPool, triggerMgr *TriggerPipeManager, dataDir string) *App {
	return &App{
		backend:    b,
		pool:       pool,
		triggerMgr: triggerMgr,
		sent:       newSentMessageStore(dataDir),
	}
}

// HandleMessage is the backend.MessageHandler callback. It dispatches
// commands and normal messages.
func (a *App) HandleMessage(ctx context.Context, msg backend.Message) {
	// Record the incoming message so future reply-to references can quote it.
	// Without this, replies to user messages would show "content unavailable".
	a.sent.Put(msg.ConversationID, msg.MessageID, msg.Text)

	switch msg.Text {
	case "!help":
		a.handleHelp(ctx, msg)
	case "!restart":
		a.handleRestart(ctx, msg)
	case "!stop":
		a.handleStop(ctx, msg)
	case "!compact":
		a.handleCompact(ctx, msg)
	case "!skills":
		a.handleSkills(ctx, msg)
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
		"  !skills  — List loaded skills"
	a.backend.SendMessage(ctx, msg.ConversationID, help, "")
}

func (a *App) handleRestart(ctx context.Context, msg backend.Message) {
	a.backend.ResetConversation(ctx, msg.ConversationID)
	a.pool.Remove(msg.ConversationID)
	a.backend.SendMessage(ctx, msg.ConversationID, "Session restarted. Next message will use a fresh process.", "")
}

func (a *App) handleStop(ctx context.Context, msg backend.Message) {
	pi := a.pool.GetExisting(msg.ConversationID)
	if pi == nil {
		a.backend.SendMessage(ctx, msg.ConversationID, "No active session.", "")

		return
	}

	if pi.Abort() {
		a.backend.SendMessage(ctx, msg.ConversationID, "Aborted current operation.", "")
	} else {
		a.backend.SendMessage(ctx, msg.ConversationID, "Nothing running to stop.", "")
	}
}

func (a *App) handleCompact(ctx context.Context, msg backend.Message) {
	pi := a.pool.GetExisting(msg.ConversationID)
	if pi == nil {
		a.backend.SendMessage(ctx, msg.ConversationID, "No active session to compact.", "")

		return
	}

	result, err := pi.Compact(ctx)
	if err != nil {
		slog.Error("compact failed", "conversation", msg.ConversationID, "error", err)
		a.backend.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Compaction failed: %v", err), "")

		return
	}

	reply := fmt.Sprintf("Compacted conversation (was %d tokens).\nSummary: %s", result.TokensBefore, result.Summary)
	a.backend.SendMessage(ctx, msg.ConversationID, reply, "")
}

func (a *App) handleSkills(ctx context.Context, msg backend.Message) {
	a.backend.SendMessage(ctx, msg.ConversationID, a.pool.SkillsSummary(), "")
}

func (a *App) handlePrompt(ctx context.Context, msg backend.Message) {
	a.backend.SetTyping(ctx, msg.ConversationID, true)
	defer a.backend.SetTyping(ctx, msg.ConversationID, false)

	pi, err := a.pool.Get(ctx, msg.ConversationID)
	if err != nil {
		slog.Error("failed to get pi process", "conversation", msg.ConversationID, "error", err)
		a.backend.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Error starting AI backend: %v", err), "")

		return
	}

	if a.triggerMgr != nil {
		a.triggerMgr.StartRoom(ctx, msg.ConversationID)
	}

	toolCallFn := a.makeToolCallFn(ctx, msg.ConversationID)
	promptText := a.buildPromptText(msg)

	reply := a.promptWithRetry(ctx, pi, msg.ConversationID, promptText, toolCallFn)

	a.sendReplyWithFiles(ctx, msg.ConversationID, reply, msg.ReplyToID)
}

// makeToolCallFn returns a callback that relays tool-call events to the chat,
// or nil when tool-call display is disabled.
func (a *App) makeToolCallFn(ctx context.Context, conversationID string) func(ToolCallEvent) {
	if !a.pool.cfg.ShowToolCalls {
		return nil
	}

	return func(evt ToolCallEvent) {
		text := formatToolCall(evt)
		a.backend.SendMessage(ctx, conversationID, text, "")
	}
}

// buildPromptText prepends reply-quote context to the message text.
func (a *App) buildPromptText(msg backend.Message) string {
	promptText := msg.Text

	if msg.ReplyToID != "" {
		if quoted := a.sent.Get(msg.ConversationID, msg.ReplyToID); quoted != "" {
			promptText = fmt.Sprintf("[user replied to message: %q]\n%s", quoted, promptText)
		} else {
			promptText = "[user replied to a message whose content is unavailable — ask for clarification if their message is unclear]\n" + promptText
		}
	}

	return promptText
}

// promptWithRetry sends the prompt to pi and re-prompts once when the
// response is empty. On error the pi process is removed from the pool.
func (a *App) promptWithRetry(ctx context.Context, pi *PiProcess, conversationID, promptText string, toolCallFn func(ToolCallEvent)) string {
	reply, err := pi.Prompt(ctx, promptText, toolCallFn)
	if err != nil {
		slog.Error("pi prompt failed", "conversation", conversationID, "error", err)
		a.pool.Remove(conversationID)

		return fmt.Sprintf("Error: %v", err)
	}

	if reply != "" {
		return reply
	}

	slog.Warn("pi returned empty response, re-prompting for summary", "conversation", conversationID)

	reply, err = pi.Prompt(ctx, "You just completed a task but your response contained no text for the user. Please briefly summarize what you did or respond to the user's message.")
	if err != nil {
		slog.Error("re-prompt after empty response failed", "conversation", conversationID, "error", err)

		return "(I completed some actions but failed to generate a summary.)"
	}

	if reply == "" {
		return "(empty response)"
	}

	return reply
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
		a.sent.Put(conversationID, sentID, cleanReply)
	}
}

// formatToolCall produces a short human-readable summary of a tool invocation.
func formatToolCall(evt ToolCallEvent) string {
	switch evt.ToolName {
	case "bash":
		if cmd, ok := evt.Args["command"].(string); ok {
			return fmt.Sprintf("🔧 `%s`", cmd)
		}

		return "🔧 bash"
	case "read":
		if p, ok := evt.Args["path"].(string); ok {
			return fmt.Sprintf("📄 reading `%s`", p)
		}

		return "📄 reading file"
	case "edit":
		if p, ok := evt.Args["path"].(string); ok {
			return fmt.Sprintf("✏️ editing `%s`", p)
		}

		return "✏️ editing file"
	case "write":
		if p, ok := evt.Args["path"].(string); ok {
			return fmt.Sprintf("📝 writing `%s`", p)
		}

		return "📝 writing file"
	default:
		return "🔧 " + evt.ToolName
	}
}

// systemPrompt returns the full system prompt including backend-specific extras.
func (a *App) systemPrompt(basePrompt string) string {
	extra := a.backend.SystemPromptExtra()
	if extra == "" {
		return basePrompt
	}

	return strings.TrimRight(basePrompt, "\n") + "\n\n" + extra
}

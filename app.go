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
	switch msg.Text {
	case "!restart":
		a.handleRestart(ctx, msg)
	case "!stop":
		a.handleStop(ctx, msg)
	case "!skills":
		a.handleSkills(ctx, msg)
	default:
		a.handlePrompt(ctx, msg)
	}
}

func (a *App) handleRestart(ctx context.Context, msg backend.Message) {
	a.backend.ResetConversation(ctx, msg.ConversationID)
	a.pool.Remove(msg.ConversationID)
	a.backend.SendMessage(ctx, msg.ConversationID, "Session restarted. Next message will use a fresh process.", "")
}

func (a *App) handleStop(ctx context.Context, msg backend.Message) {
	pi, err := a.pool.Get(ctx, msg.ConversationID)
	if err != nil {
		a.backend.SendMessage(ctx, msg.ConversationID, "No active session.", "")

		return
	}

	if pi.Abort() {
		a.backend.SendMessage(ctx, msg.ConversationID, "Aborted current operation.", "")
	} else {
		a.backend.SendMessage(ctx, msg.ConversationID, "Nothing running to stop.", "")
	}
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

	var toolCallFn func(ToolCallEvent)
	if a.pool.cfg.ShowToolCalls {
		toolCallFn = func(evt ToolCallEvent) {
			text := formatToolCall(evt)
			a.backend.SendMessage(ctx, msg.ConversationID, text, "")
		}
	}

	promptText := msg.Text

	if msg.ReplyToID != "" {
		if quoted := a.sent.Get(msg.ConversationID, msg.ReplyToID); quoted != "" {
			promptText = fmt.Sprintf("[user replied to your message: %q]\n%s", quoted, promptText)
		} else {
			promptText = "[user replied to a message whose content is unavailable — ask for clarification if their message is unclear]\n" + promptText
		}
	}

	reply, err := pi.Prompt(ctx, promptText, toolCallFn)
	if err != nil {
		slog.Error("pi prompt failed", "conversation", msg.ConversationID, "error", err)
		a.pool.Remove(msg.ConversationID)

		reply = fmt.Sprintf("Error: %v", err)
	}

	if reply == "" {
		slog.Warn("pi returned empty response, re-prompting for summary", "conversation", msg.ConversationID)

		reply, err = pi.Prompt(ctx, "You just completed a task but your response contained no text for the user. Please briefly summarize what you did or respond to the user's message.")
		if err != nil {
			slog.Error("re-prompt after empty response failed", "conversation", msg.ConversationID, "error", err)

			reply = "(I completed some actions but failed to generate a summary.)"
		}

		if reply == "" {
			reply = "(empty response)"
		}
	}

	slog.Info("sending reply", "conversation", msg.ConversationID, "len", len(reply))
	slog.Debug("outgoing reply content", "conversation", msg.ConversationID, "content", reply)

	cleanReply, filePaths := extractSendFiles(reply)

	var fileSendErrors strings.Builder

	for _, fp := range filePaths {
		slog.Info("sending file", "conversation", msg.ConversationID, "path", fp)

		if err := a.backend.SendFile(ctx, msg.ConversationID, fp); err != nil {
			slog.Error("failed to send file", "conversation", msg.ConversationID, "path", fp, "error", err)
			fileSendErrors.WriteString(fmt.Sprintf("\n\n(failed to send file %s: %v)", filepath.Base(fp), err))
		}
	}

	cleanReply += fileSendErrors.String()

	if cleanReply != "" {
		sentID := a.backend.SendMessage(ctx, msg.ConversationID, cleanReply, msg.ReplyToID)
		a.sent.Put(msg.ConversationID, sentID, cleanReply)
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

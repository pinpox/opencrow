// Package telegram implements the Backend interface for Telegram via the
// Bot API. It uses long-polling getUpdates so OpenCrow does not need a
// public webhook endpoint.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinpox/opencrow/backend"
)

const (
	defaultAPIBase     = "https://api.telegram.org"
	defaultPollTimeout = 25 * time.Second
	httpClientTimeout  = 60 * time.Second
	pollErrorBackoff   = 5 * time.Second
	attachmentsDirName = "attachments"
)

// Config holds Telegram-specific configuration.
type Config struct {
	// Token is the bot token issued by @BotFather (required).
	Token string
	// APIBase overrides the Telegram API base URL (default https://api.telegram.org).
	APIBase string
	// AllowedUsers is a set of permitted Telegram user IDs (numeric, as strings).
	// Empty means everyone is allowed.
	AllowedUsers map[string]struct{}
	// SessionBaseDir is the directory under which incoming attachments are
	// saved (in <SessionBaseDir>/attachments/).
	SessionBaseDir string
	// PollTimeout is the long-poll timeout passed to getUpdates.
	PollTimeout time.Duration
}

// Backend implements backend.Backend for Telegram.
type Backend struct {
	cfg     Config
	handler backend.MessageHandler
	http    *http.Client

	cancel backend.Canceler
	active backend.ActiveConversation

	mu     sync.Mutex
	offset int64
}

// New creates a new Telegram backend.
func New(cfg Config, handler backend.MessageHandler) (*Backend, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram bot token is required")
	}

	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPIBase
	}

	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = defaultPollTimeout
	}

	return &Backend{
		cfg:     cfg,
		handler: handler,
		http: &http.Client{
			// Long polls block server-side for PollTimeout; allow a
			// generous extra margin for network latency.
			Timeout: cfg.PollTimeout + httpClientTimeout,
		},
	}, nil
}

// Run starts the long-polling loop. It returns when ctx is cancelled.
func (b *Backend) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	b.cancel.Set(cancel)
	defer b.cancel.Set(nil)

	if err := b.ensureAttachmentsDir(); err != nil {
		return err
	}

	slog.Info("telegram: starting long-poll loop", "timeout", b.cfg.PollTimeout)

	for {
		if runCtx.Err() != nil {
			return nil
		}

		updates, err := b.fetchUpdates(runCtx)
		if err != nil {
			if runCtx.Err() != nil {
				return nil
			}

			slog.Warn("telegram: getUpdates failed", "error", err)

			select {
			case <-runCtx.Done():
				return nil
			case <-time.After(pollErrorBackoff):
			}

			continue
		}

		for _, upd := range updates {
			b.handleUpdate(runCtx, upd)
		}
	}
}

// Stop cancels the run loop.
func (b *Backend) Stop() {
	b.cancel.Cancel()
}

// Close releases resources. Telegram has none to clean up beyond cancelling.
func (b *Backend) Close() error {
	b.cancel.Cancel()

	return nil
}

// SendMessage sends a text message to the given conversation (chat ID as string).
// The text is rendered as Telegram HTML (parse_mode=HTML) so pi's markdown
// (**bold**, `code`, [links](url), etc.) appears formatted. If Telegram
// rejects the rendered HTML (unbalanced markdown slipping past the
// converter), the message is resent as plain text without parse_mode.
// Returns the new message ID, or "" on failure.
func (b *Backend) SendMessage(ctx context.Context, conversationID string, text string, replyToID string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}

	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		slog.Error("telegram: invalid conversation id", "id", conversationID, "error", err)

		return ""
	}

	rendered := markdownToHTML(text)

	id, err := b.sendText(ctx, chatID, rendered, replyToID, "HTML")
	if err == nil {
		return id
	}

	slog.Warn("telegram: HTML send failed, retrying as plain text",
		"conversation", conversationID, "error", err)

	id, err = b.sendText(ctx, chatID, text, replyToID, "")
	if err != nil {
		slog.Error("telegram: sendMessage failed", "conversation", conversationID, "error", err)

		return ""
	}

	return id
}

func (b *Backend) sendText(ctx context.Context, chatID int64, text, replyToID, parseMode string) (string, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}

	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}

	if replyToID != "" {
		if rid, err := strconv.ParseInt(replyToID, 10, 64); err == nil {
			payload["reply_parameters"] = map[string]any{
				"message_id":                  rid,
				"allow_sending_without_reply": true,
			}
		}
	}

	var result struct {
		MessageID int64 `json:"message_id"`
	}

	if err := b.callJSON(ctx, "sendMessage", payload, &result); err != nil {
		return "", err
	}

	return strconv.FormatInt(result.MessageID, 10), nil
}

// SendFile uploads and sends a local file as a Telegram document.
func (b *Backend) SendFile(ctx context.Context, conversationID string, filePath string) error {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid conversation id %q: %w", conversationID, err)
	}

	body, contentType, err := buildSendDocumentBody(chatID, filePath)
	if err != nil {
		return err
	}

	endpoint := b.endpoint("sendDocument")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("building sendDocument request: %w", err)
	}

	req.Header.Set("Content-Type", contentType)

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("sendDocument: %w", err)
	}

	defer resp.Body.Close()

	return decodeAPIResponse(resp.Body, nil)
}

// SetTyping shows the "typing..." indicator while typing is true. Telegram
// indicators auto-expire after ~5s, so callers that want a sustained typing
// state should call again periodically.
func (b *Backend) SetTyping(ctx context.Context, conversationID string, typing bool) {
	if !typing {
		return
	}

	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return
	}

	_ = b.callJSON(ctx, "sendChatAction", map[string]any{
		"chat_id": chatID,
		"action":  "typing",
	}, nil)
}

// ResetConversation clears the active-conversation lock if it matches.
func (b *Backend) ResetConversation(_ context.Context, conversationID string) {
	b.active.Reset(conversationID)
}

// SystemPromptExtra returns Telegram-specific system prompt context.
func (b *Backend) SystemPromptExtra() string {
	return `You are communicating via Telegram (Bot API backend).

## Sending files to the user

You can send files back to the user. Include a <sendfile> tag in your
response with the absolute path to the file:

<sendfile>/path/to/file.png</sendfile>

The bot strips the tag and uploads the file as a Telegram document. You can
include multiple <sendfile> tags in a single response.

## File attachments from the user

When users send files, you'll receive a message like:
"[User sent a file (...): /path/to/file]"
Use the read tool to inspect the file.

## Formatting

Markdown is rendered. You can use **bold**, *italic*, ` + "`inline code`" + `,
fenced ` + "```code blocks```" + `, [links](https://example.com), ~~strike~~, and
"-" / "*" bullet lists. Headings (#, ##) are rendered as bold lines.
Telegram has no real list / table layout — keep them simple.

Avoid raw HTML in your replies; the bridge converts your markdown to the
HTML subset Telegram understands.`
}

// MarkdownFlavor reports MarkdownFull because the backend converts
// markdown replies to Telegram HTML before sending (parse_mode=HTML),
// so callers may safely emit fenced code blocks with language hints,
// inline backticks, and other rich formatting.
func (b *Backend) MarkdownFlavor() backend.MarkdownFlavor {
	return backend.MarkdownFull
}

// --- internal ---

func (b *Backend) endpoint(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(b.cfg.APIBase, "/"), b.cfg.Token, method)
}

func (b *Backend) fileURL(filePath string) string {
	return fmt.Sprintf("%s/file/bot%s/%s", strings.TrimRight(b.cfg.APIBase, "/"), b.cfg.Token, filePath)
}

func (b *Backend) callJSON(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint(method), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building %s request: %w", method, err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}

	defer resp.Body.Close()

	return decodeAPIResponse(resp.Body, out)
}

func (b *Backend) fetchUpdates(ctx context.Context) ([]update, error) {
	b.mu.Lock()
	offset := b.offset
	b.mu.Unlock()

	timeoutSec := int64(b.cfg.PollTimeout / time.Second)
	if timeoutSec <= 0 {
		timeoutSec = int64(defaultPollTimeout / time.Second)
	}

	payload := map[string]any{
		"offset":  offset,
		"timeout": timeoutSec,
		"allowed_updates": []string{
			"message",
		},
	}

	var updates []update
	if err := b.callJSON(ctx, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}

	if len(updates) > 0 {
		b.mu.Lock()
		// Acknowledge by advancing offset past the highest update_id.
		b.offset = updates[len(updates)-1].UpdateID + 1
		b.mu.Unlock()
	}

	return updates, nil
}

func (b *Backend) handleUpdate(ctx context.Context, upd update) {
	if upd.Message == nil {
		return
	}

	msg := upd.Message
	if msg.From == nil {
		return
	}

	if msg.From.IsBot {
		return
	}

	senderID := strconv.FormatInt(msg.From.ID, 10)
	if !backend.IsAllowed(b.cfg.AllowedUsers, senderID) {
		// Also allow matching by @username for convenience.
		if msg.From.Username == "" || !backend.IsAllowed(b.cfg.AllowedUsers, "@"+msg.From.Username) {
			slog.Debug("telegram: dropping message from non-allowed sender", "sender", senderID, "username", msg.From.Username)

			return
		}
	}

	conversationID := strconv.FormatInt(msg.Chat.ID, 10)
	if !b.active.Claim(conversationID) {
		slog.Info("telegram: dropping message from different active conversation", "conversation", conversationID)

		return
	}

	text := strings.TrimSpace(msg.Text)

	if attachmentText := b.downloadAttachments(ctx, msg); attachmentText != "" {
		caption := strings.TrimSpace(msg.Caption)
		if caption != "" {
			if text != "" {
				text += "\n"
			}

			text += caption
		}

		if text != "" {
			text += "\n"
		}

		text += attachmentText
	}

	if text == "" {
		return
	}

	messageID := strconv.FormatInt(msg.MessageID, 10)

	var replyToID string
	if msg.ReplyToMessage != nil {
		replyToID = strconv.FormatInt(msg.ReplyToMessage.MessageID, 10)
	}

	slog.Info("telegram: received message",
		"conversation", conversationID,
		"sender", senderID,
		"len", len(text),
	)

	b.handler(ctx, backend.Message{
		ConversationID: conversationID,
		SenderID:       senderID,
		Text:           text,
		MessageID:      messageID,
		ReplyToID:      replyToID,
	})
}

// downloadAttachments fetches every file referenced by msg and returns the
// canonical "[User sent a file ...]" lines for them.
func (b *Backend) downloadAttachments(ctx context.Context, msg *message) string {
	type attachment struct {
		fileID   string
		filename string
	}

	var atts []attachment

	if len(msg.Photo) > 0 {
		// Photo array is sorted from smallest to largest; pick the largest.
		largest := msg.Photo[len(msg.Photo)-1]
		atts = append(atts, attachment{fileID: largest.FileID})
	}

	if msg.Document != nil {
		atts = append(atts, attachment{fileID: msg.Document.FileID, filename: msg.Document.FileName})
	}

	if msg.Audio != nil {
		atts = append(atts, attachment{fileID: msg.Audio.FileID, filename: msg.Audio.FileName})
	}

	if msg.Video != nil {
		atts = append(atts, attachment{fileID: msg.Video.FileID, filename: msg.Video.FileName})
	}

	if msg.Voice != nil {
		atts = append(atts, attachment{fileID: msg.Voice.FileID})
	}

	if msg.VideoNote != nil {
		atts = append(atts, attachment{fileID: msg.VideoNote.FileID})
	}

	if len(atts) == 0 {
		return ""
	}

	lines := make([]string, 0, len(atts))

	for _, a := range atts {
		path, err := b.downloadFile(ctx, a.fileID, a.filename)
		if err != nil {
			slog.Warn("telegram: failed to download attachment", "file_id", a.fileID, "error", err)
			lines = append(lines, backend.AttachmentText(a.filename, ""))

			continue
		}

		lines = append(lines, backend.AttachmentText(filepath.Base(path), path))
	}

	return strings.Join(lines, "\n")
}

func (b *Backend) downloadFile(ctx context.Context, fileID, preferredName string) (string, error) {
	var fileInfo struct {
		FileID   string `json:"file_id"`
		FilePath string `json:"file_path"`
	}

	if err := b.callJSON(ctx, "getFile", map[string]any{"file_id": fileID}, &fileInfo); err != nil {
		return "", fmt.Errorf("getFile: %w", err)
	}

	if fileInfo.FilePath == "" {
		return "", errors.New("getFile returned empty file_path")
	}

	// Build the destination path: prefer the user-provided filename, then
	// the basename Telegram exposes, falling back to the file_id.
	name := strings.TrimSpace(preferredName)
	if name == "" {
		name = filepath.Base(fileInfo.FilePath)
	}

	if name == "" || name == "." || name == "/" {
		name = fileID
	}

	dir := b.attachmentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating attachments dir: %w", err)
	}

	dest := filepath.Join(dir, fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.fileURL(fileInfo.FilePath), nil)
	if err != nil {
		return "", fmt.Errorf("building file download request: %w", err)
	}

	resp, err := b.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading file: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("file download returned %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("creating attachment file: %w", err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(dest)

		return "", fmt.Errorf("writing attachment file: %w", err)
	}

	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing attachment file: %w", err)
	}

	return dest, nil
}

func (b *Backend) attachmentsDir() string {
	if b.cfg.SessionBaseDir == "" {
		return attachmentsDirName
	}

	return filepath.Join(b.cfg.SessionBaseDir, attachmentsDirName)
}

func (b *Backend) ensureAttachmentsDir() error {
	if b.cfg.SessionBaseDir == "" {
		return nil
	}

	if err := os.MkdirAll(b.attachmentsDir(), 0o755); err != nil {
		return fmt.Errorf("creating telegram attachments dir: %w", err)
	}

	return nil
}

// --- HTTP helpers ---

// decodeAPIResponse reads a Bot API envelope and unmarshals result into out
// (or discards it if out is nil). It returns an error when ok=false.
func decodeAPIResponse(r io.Reader, out any) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}

	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decoding response: %w (body=%s)", err, truncate(string(body), 200))
	}

	if !env.OK {
		return fmt.Errorf("telegram api error: %s", env.Description)
	}

	if out == nil || len(env.Result) == 0 {
		return nil
	}

	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("decoding result: %w", err)
	}

	return nil
}

// buildSendDocumentBody builds a multipart body for sendDocument. Returns
// the body and the Content-Type header (which carries the random boundary).
func buildSendDocumentBody(chatID int64, filePath string) (io.Reader, string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, "", fmt.Errorf("opening file %s: %w", filePath, err)
	}

	var buf bytes.Buffer

	w := multipart.NewWriter(&buf)

	if err := w.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		f.Close()

		return nil, "", fmt.Errorf("writing chat_id field: %w", err)
	}

	part, err := w.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		f.Close()

		return nil, "", fmt.Errorf("creating form file: %w", err)
	}

	if _, err := io.Copy(part, f); err != nil {
		f.Close()

		return nil, "", fmt.Errorf("copying file body: %w", err)
	}

	if err := f.Close(); err != nil {
		return nil, "", fmt.Errorf("closing source file: %w", err)
	}

	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("closing multipart writer: %w", err)
	}

	return &buf, w.FormDataContentType(), nil
}

// truncate returns s shortened to n runes with an ellipsis suffix when cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}

// --- wire types ---

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message,omitempty"`
}

type message struct {
	MessageID      int64       `json:"message_id"`
	From           *user       `json:"from,omitempty"`
	Chat           chat        `json:"chat"`
	Date           int64       `json:"date"`
	Text           string      `json:"text,omitempty"`
	Caption        string      `json:"caption,omitempty"`
	ReplyToMessage *message    `json:"reply_to_message,omitempty"`
	Photo          []photoSize `json:"photo,omitempty"`
	Document       *document   `json:"document,omitempty"`
	Audio          *document   `json:"audio,omitempty"`
	Video          *document   `json:"video,omitempty"`
	Voice          *document   `json:"voice,omitempty"`
	VideoNote      *document   `json:"video_note,omitempty"`
}

type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
	IsBot    bool   `json:"is_bot,omitempty"`
}

type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type photoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

type document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}


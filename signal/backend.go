// Package signal implements the Backend interface for Signal via signal-cli.
package signal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinpox/opencrow/backend"
)

const (
	groupConversationPrefix = "signal-group:"
	defaultRPCTimeout       = 10 * time.Second
)

// Config holds Signal-specific configuration.
type Config struct {
	BinaryPath   string
	Account      string
	ConfigDir    string
	SocketPath   string
	AllowedUsers map[string]struct{}
}

// Backend implements backend.Backend for Signal (signal-cli daemon + JSON-RPC).
type Backend struct {
	cfg          Config
	handler      backend.MessageHandler
	allowedUsers map[string]struct{}

	active backend.ActiveConversation

	cancelMu sync.Mutex
	cancelFn context.CancelFunc

	rpcMu sync.RWMutex
	rpc   *jsonRPCClient

	daemonMu      sync.Mutex
	daemonCmd     *exec.Cmd
	daemonDone    chan error
	daemonLogTail []string

	subMu          sync.Mutex
	subscriptionID int
}

// New creates a new Signal backend.
func New(cfg Config, handler backend.MessageHandler) (*Backend, error) {
	if cfg.Account == "" {
		return nil, errors.New("signal account is required")
	}

	if cfg.BinaryPath == "" {
		cfg.BinaryPath = "signal-cli"
	}

	if cfg.ConfigDir != "" && cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(cfg.ConfigDir, "opencrow-jsonrpc.sock")
	}

	if cfg.SocketPath == "" {
		return nil, errors.New("signal socket path is required")
	}

	return &Backend{
		cfg:            cfg,
		handler:        handler,
		allowedUsers:   cfg.AllowedUsers,
		subscriptionID: -1,
	}, nil
}

// Run starts signal-cli daemon mode and processes JSON-RPC receive notifications.
//
//nolint:gocognit,cyclop,funlen // event loop combines setup, stream handling, and shutdown paths.
func (b *Backend) Run(ctx context.Context) error {
	if _, err := exec.LookPath(b.cfg.BinaryPath); err != nil {
		return fmt.Errorf("signal-cli binary not found (%s): %w", b.cfg.BinaryPath, err)
	}

	if err := b.preparePaths(); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	b.cancelMu.Lock()
	b.cancelFn = cancel
	b.cancelMu.Unlock()

	defer func() {
		b.cancelMu.Lock()
		b.cancelFn = nil
		b.cancelMu.Unlock()
	}()

	if err := b.startDaemon(runCtx); err != nil {
		return err
	}
	defer b.stopDaemon()

	conn, err := b.waitForSocket(runCtx)
	if err != nil {
		return err
	}

	client := newJSONRPCClient(conn)
	b.setRPC(client)

	defer b.clearRPC()
	defer client.Close()

	if err := b.subscribeReceive(runCtx, client); err != nil {
		return err
	}

	defer b.unsubscribeReceive(context.WithoutCancel(runCtx), client)

	notifications := client.Notifications()
	errs := client.Errors()

	for {
		select {
		case <-runCtx.Done():
			return fmt.Errorf("signal receive loop: %w", runCtx.Err())
		case err := <-errs:
			if err == nil {
				continue
			}

			if runCtx.Err() != nil {
				return fmt.Errorf("signal receive loop: %w", runCtx.Err())
			}

			return fmt.Errorf("signal json-rpc stream failed: %w", err)
		case n := <-notifications:
			if n.Method != "receive" {
				continue
			}

			msg, ok, err := decodeReceiveNotification(n.Params, b.cfg.ConfigDir)
			if err != nil {
				slog.Warn("signal: failed to parse incoming notification", "error", err)

				continue
			}

			if !ok {
				continue
			}

			if msg.SenderID == b.cfg.Account {
				continue
			}

			if !b.isAllowed(msg.SenderID) {
				slog.Debug("signal: dropping message from non-allowed sender", "sender", msg.SenderID)

				continue
			}

			if !b.claimConversation(msg.ConversationID) {
				slog.Info("signal: dropping message from different active conversation", "sender", msg.SenderID)

				continue
			}

			slog.Info("signal: received message",
				"conversation", msg.ConversationID,
				"sender", msg.SenderID,
				"len", len(msg.Text),
			)

			b.handler(runCtx, *msg)
		}
	}
}

// Stop signals the Signal receive loop to stop.
func (b *Backend) Stop() {
	b.cancelMu.Lock()
	if b.cancelFn != nil {
		b.cancelFn()
	}
	b.cancelMu.Unlock()
}

// Close releases JSON-RPC and daemon resources.
func (b *Backend) Close() error {
	b.clearRPC()
	b.stopDaemon()

	return nil
}

// SendMessage sends a Signal message. Returns the sent message timestamp.
func (b *Backend) SendMessage(ctx context.Context, conversationID string, text string, replyToID string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}

	timestamp, err := b.sendMessage(ctx, conversationID, text, replyToID)
	if err != nil {
		slog.Error("signal: failed to send message", "conversation", conversationID, "error", err)

		return ""
	}

	return timestamp
}

// SendFile sends a file attachment via Signal.
func (b *Backend) SendFile(ctx context.Context, conversationID string, filePath string) error {
	params := map[string]any{
		"attachment": filePath,
	}
	addRecipientParams(params, conversationID)

	var result sendResult
	if err := b.rpcCall(ctx, "send", params, &result); err != nil {
		return fmt.Errorf("signal send attachment: %w", err)
	}

	slog.Info("signal: sent attachment",
		"conversation", conversationID,
		"path", filePath,
		"timestamp", result.Timestamp,
	)

	return nil
}

// SetTyping is a no-op on Signal.
func (b *Backend) SetTyping(_ context.Context, _ string, _ bool) {}

// ResetConversation clears active conversation tracking.
func (b *Backend) ResetConversation(_ context.Context, conversationID string) {
	b.active.Reset(conversationID)
}

// SystemPromptExtra returns Signal-specific system prompt context.
func (b *Backend) SystemPromptExtra() string {
	return `You are communicating via Signal (signal-cli backend).

## Sending files to the user

You can send files back to the user in Signal. To do this, include a <sendfile> tag
in your response with the absolute path to the file:

<sendfile>/path/to/file.png</sendfile>

The bot will send the file as an attachment via signal-cli.
You can include multiple <sendfile> tags in a single response.

## File attachments from the user

When users send files in Signal, you'll receive a message like:
"[User sent a file (...): /path/to/file]"
Use the read tool to inspect the file.`
}

// MarkdownFlavor returns MarkdownNone: Signal does not interpret Markdown
// syntax and would display backticks and fences literally.
func (b *Backend) MarkdownFlavor() backend.MarkdownFlavor {
	return backend.MarkdownNone
}

func (b *Backend) sendMessage(ctx context.Context, conversationID, text, replyToID string) (string, error) {
	params := map[string]any{
		"message": text,
	}
	addRecipientParams(params, conversationID)

	if replyToID != "" {
		if ts, err := strconv.ParseInt(replyToID, 10, 64); err == nil {
			params["quoteTimestamp"] = ts
		}
	}

	var result sendResult
	if err := b.rpcCall(ctx, "send", params, &result); err != nil {
		return "", fmt.Errorf("signal send: %w", err)
	}

	if result.Timestamp == 0 {
		return "", nil
	}

	return strconv.FormatInt(result.Timestamp, 10), nil
}

func (b *Backend) rpcCall(ctx context.Context, method string, params any, out any) error {
	client := b.getRPC()
	if client == nil {
		return errors.New("signal json-rpc client is not connected")
	}

	callCtx, cancel := context.WithTimeout(ctx, defaultRPCTimeout)
	defer cancel()

	if err := client.Call(callCtx, method, params, out); err != nil {
		return fmt.Errorf("json-rpc %s: %w", method, err)
	}

	return nil
}

func (b *Backend) isAllowed(senderID string) bool {
	return backend.IsAllowed(b.allowedUsers, senderID)
}

func (b *Backend) claimConversation(conversationID string) bool {
	return b.active.Claim(conversationID)
}

func (b *Backend) subscribeReceive(ctx context.Context, client *jsonRPCClient) error {
	var subID int
	if err := client.Call(ctx, "subscribeReceive", map[string]any{}, &subID); err != nil {
		return fmt.Errorf("subscribing to signal receive stream: %w", err)
	}

	b.subMu.Lock()
	b.subscriptionID = subID
	b.subMu.Unlock()

	slog.Info("signal: subscribed to receive stream", "subscription", subID)

	return nil
}

func (b *Backend) unsubscribeReceive(ctx context.Context, client *jsonRPCClient) {
	b.subMu.Lock()
	subID := b.subscriptionID
	b.subscriptionID = -1
	b.subMu.Unlock()

	if subID < 0 {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if err := client.Call(callCtx, "unsubscribeReceive", map[string]any{"subscription": subID}, nil); err != nil {
		slog.Debug("signal: failed to unsubscribe receive stream", "subscription", subID, "error", err)
	}
}

func (b *Backend) setRPC(client *jsonRPCClient) {
	b.rpcMu.Lock()
	b.rpc = client
	b.rpcMu.Unlock()
}

func (b *Backend) getRPC() *jsonRPCClient {
	b.rpcMu.RLock()
	defer b.rpcMu.RUnlock()

	return b.rpc
}

func (b *Backend) clearRPC() {
	b.rpcMu.Lock()
	client := b.rpc
	b.rpc = nil
	b.rpcMu.Unlock()

	if client != nil {
		client.Close()
	}
}

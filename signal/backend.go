// Package signal implements the Backend interface for Signal via signal-cli.
package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

	activeMu     sync.Mutex
	activeConvID string

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
	b.activeMu.Lock()
	if b.activeConvID == conversationID {
		b.activeConvID = ""
	}
	b.activeMu.Unlock()
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
	if len(b.allowedUsers) == 0 {
		return true
	}

	_, ok := b.allowedUsers[senderID]

	return ok
}

func (b *Backend) claimConversation(conversationID string) bool {
	b.activeMu.Lock()
	defer b.activeMu.Unlock()

	if b.activeConvID == "" {
		b.activeConvID = conversationID

		return true
	}

	return b.activeConvID == conversationID
}

func (b *Backend) preparePaths() error {
	if b.cfg.ConfigDir != "" {
		if err := os.MkdirAll(b.cfg.ConfigDir, 0o700); err != nil {
			return fmt.Errorf("creating signal config dir: %w", err)
		}
	}

	sockDir := filepath.Dir(b.cfg.SocketPath)
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return fmt.Errorf("creating signal socket dir: %w", err)
	}

	if err := os.Remove(b.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale signal socket: %w", err)
	}

	return nil
}

func (b *Backend) startDaemon(ctx context.Context) error {
	args := []string{"--output", "json", "--account", b.cfg.Account}
	if b.cfg.ConfigDir != "" {
		args = append(args, "--config", b.cfg.ConfigDir)
	}

	args = append(args,
		"daemon",
		"--socket="+b.cfg.SocketPath,
		"--receive-mode", "manual",
	)

	slog.Info("signal: starting daemon",
		"binary", b.cfg.BinaryPath,
		"account", b.cfg.Account,
		"config_dir", b.cfg.ConfigDir,
		"socket", b.cfg.SocketPath,
		"args", strings.Join(args, " "),
	)

	cmd := exec.CommandContext(ctx, b.cfg.BinaryPath, args...) //nolint:gosec // binary path and args come from trusted service config.

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating signal daemon stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("creating signal daemon stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting signal daemon: %w", err)
	}

	done := make(chan error, 1)

	b.resetDaemonLogTail()

	go b.captureDaemonStream("stdout", stdout)
	go b.captureDaemonStream("stderr", stderr)

	go func() {
		done <- cmd.Wait()

		close(done)
	}()

	b.daemonMu.Lock()
	b.daemonCmd = cmd
	b.daemonDone = done
	b.daemonMu.Unlock()

	return nil
}

func (b *Backend) captureDaemonStream(stream string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		b.appendDaemonLogLine(fmt.Sprintf("%s: %s", stream, line))
		slog.Debug("signal-cli daemon", "stream", stream, "line", line)
	}

	if err := scanner.Err(); err != nil {
		b.appendDaemonLogLine(fmt.Sprintf("%s scanner error: %v", stream, err))
		slog.Debug("signal-cli daemon stream error", "stream", stream, "error", err)
	}
}

func (b *Backend) appendDaemonLogLine(line string) {
	b.daemonMu.Lock()
	defer b.daemonMu.Unlock()

	const maxLines = 40

	b.daemonLogTail = append(b.daemonLogTail, line)
	if len(b.daemonLogTail) > maxLines {
		b.daemonLogTail = b.daemonLogTail[len(b.daemonLogTail)-maxLines:]
	}
}

func (b *Backend) resetDaemonLogTail() {
	b.daemonMu.Lock()
	b.daemonLogTail = nil
	b.daemonMu.Unlock()
}

func (b *Backend) daemonLogTailString() string {
	b.daemonMu.Lock()
	defer b.daemonMu.Unlock()

	if len(b.daemonLogTail) == 0 {
		return "(no daemon output captured)"
	}

	return strings.Join(b.daemonLogTail, " | ")
}

func (b *Backend) stopDaemon() {
	b.daemonMu.Lock()
	cmd := b.daemonCmd
	done := b.daemonDone
	b.daemonCmd = nil
	b.daemonDone = nil
	b.daemonMu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	// signal-cli handles SIGTERM gracefully (flushes state, closes DB).
	// Give it time before escalating to SIGKILL.
	_ = cmd.Process.Signal(syscall.SIGTERM)

	if done != nil {
		select {
		case <-done:
			return
		case <-time.After(5 * time.Second):
			slog.Warn("signal: daemon did not exit after SIGTERM, sending SIGKILL")
		}
	}

	_ = cmd.Process.Kill()

	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			slog.Error("signal: daemon did not exit after SIGKILL")
		}
	}
}

func (b *Backend) waitForSocket(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(15 * time.Second)

	dialer := net.Dialer{Timeout: 500 * time.Millisecond}

	for {
		conn, err := dialer.DialContext(ctx, "unix", b.cfg.SocketPath)
		if err == nil {
			return conn, nil
		}

		if ctx.Err() != nil {
			return nil, fmt.Errorf("connecting to signal daemon socket: %w", ctx.Err())
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for signal daemon socket %s (daemon output: %s)", b.cfg.SocketPath, b.daemonLogTailString())
		}

		b.daemonMu.Lock()
		done := b.daemonDone
		b.daemonMu.Unlock()

		if done != nil {
			select {
			case daemonErr := <-done:
				tail := b.daemonLogTailString()
				if daemonErr != nil {
					return nil, fmt.Errorf("signal daemon exited before socket ready: %w (daemon output: %s)", daemonErr, tail)
				}

				return nil, fmt.Errorf("signal daemon exited before socket ready (daemon output: %s)", tail)
			default:
			}
		}

		time.Sleep(200 * time.Millisecond)
	}
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

func addRecipientParams(params map[string]any, conversationID string) {
	if groupID, ok := parseGroupConversationID(conversationID); ok {
		params["groupId"] = groupID

		return
	}

	params["recipient"] = []string{conversationID}
}

func parseGroupConversationID(conversationID string) (string, bool) {
	if !strings.HasPrefix(conversationID, groupConversationPrefix) {
		return "", false
	}

	groupID := strings.TrimPrefix(conversationID, groupConversationPrefix)
	if groupID == "" {
		return "", false
	}

	return groupID, true
}

func conversationIDForGroup(groupID string) string {
	return groupConversationPrefix + groupID
}

type sendResult struct {
	Timestamp int64 `json:"timestamp"`
}

type receiveLine struct {
	Envelope *receiveEnvelope `json:"envelope"`
}

type receiveNotificationWrapped struct {
	Result *receiveLine `json:"result"`
}

type receiveEnvelope struct {
	Source       string          `json:"source"`
	SourceNumber string          `json:"sourceNumber"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
	SourceUUID   string          `json:"sourceUuid"`   //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
	Timestamp    int64           `json:"timestamp"`
	DataMessage  *receiveDataMsg `json:"dataMessage"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
}

type receiveDataMsg struct {
	Timestamp   int64               `json:"timestamp"`
	Message     string              `json:"message"`
	Quote       *receiveQuote       `json:"quote"`
	GroupInfo   *receiveGroupInfo   `json:"groupInfo"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
	Attachments []receiveAttachment `json:"attachments"`
}

type receiveQuote struct {
	ID int64 `json:"id"`
}

type receiveGroupInfo struct {
	GroupID string `json:"groupId"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
}

type receiveAttachment struct {
	Filename    string `json:"filename"`
	Caption     string `json:"caption"`
	ContentType string `json:"contentType"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
	ID          string `json:"id"`
}

func decodeReceiveNotification(params json.RawMessage, configDir string) (*backend.Message, bool, error) {
	msg, ok, err := decodeReceiveMessage(params, configDir)
	if err != nil {
		return nil, false, err
	}

	if ok {
		return msg, true, nil
	}

	var wrapped receiveNotificationWrapped
	if err := json.Unmarshal(params, &wrapped); err != nil {
		return nil, false, fmt.Errorf("decoding wrapped signal receive payload: %w", err)
	}

	if wrapped.Result == nil {
		return nil, false, nil
	}

	inner, err := json.Marshal(wrapped.Result)
	if err != nil {
		return nil, false, fmt.Errorf("encoding wrapped receive payload: %w", err)
	}

	return decodeReceiveMessage(inner, configDir)
}

//nolint:cyclop // straightforward mapping from signal-cli JSON to backend message.
func decodeReceiveMessage(payload []byte, configDir string) (*backend.Message, bool, error) {
	var line receiveLine
	if err := json.Unmarshal(payload, &line); err != nil {
		return nil, false, fmt.Errorf("decoding signal receive payload: %w", err)
	}

	env := line.Envelope
	if env == nil || env.DataMessage == nil {
		return nil, false, nil
	}

	sender := firstNonEmpty(env.SourceNumber, env.SourceUUID, env.Source)
	if sender == "" {
		return nil, false, nil
	}

	conversationID := sender
	if env.DataMessage.GroupInfo != nil && env.DataMessage.GroupInfo.GroupID != "" {
		conversationID = conversationIDForGroup(env.DataMessage.GroupInfo.GroupID)
	}

	text := strings.TrimSpace(env.DataMessage.Message)
	if attachmentText := formatAttachmentText(env.DataMessage.Attachments, configDir); attachmentText != "" {
		if text != "" {
			text += "\n"
		}

		text += attachmentText
	}

	if text == "" {
		return nil, false, nil
	}

	var messageID string
	if ts := firstNonZero(env.DataMessage.Timestamp, env.Timestamp); ts != 0 {
		messageID = strconv.FormatInt(ts, 10)
	}

	var replyTo string
	if env.DataMessage.Quote != nil && env.DataMessage.Quote.ID != 0 {
		replyTo = strconv.FormatInt(env.DataMessage.Quote.ID, 10)
	}

	return &backend.Message{
		ConversationID: conversationID,
		SenderID:       sender,
		Text:           text,
		MessageID:      messageID,
		ReplyToID:      replyTo,
	}, true, nil
}

func formatAttachmentText(attachments []receiveAttachment, configDir string) string {
	if len(attachments) == 0 {
		return ""
	}

	lines := make([]string, 0, len(attachments)+1)

	for _, a := range attachments {
		caption := strings.TrimSpace(a.Caption)
		if caption == "" {
			filename := strings.TrimSpace(a.Filename)
			if filename != "" {
				caption = filepath.Base(filename)
			}
		}

		if caption == "" {
			caption = firstNonEmpty(a.ContentType, a.ID, "attachment")
		}

		filePath := resolveAttachmentPath(a.Filename, configDir)

		switch {
		case filePath != "":
			lines = append(lines, fmt.Sprintf("[User sent a file (%s): %s]", caption, filePath))
		default:
			lines = append(lines, fmt.Sprintf("[User sent a file (%s)]", caption))
		}
	}

	lines = append(lines, "Use the read tool to view it.")

	return strings.Join(lines, "\n")
}

func resolveAttachmentPath(path, configDir string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	if filepath.IsAbs(path) || configDir == "" {
		return path
	}

	return filepath.Join(configDir, path)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}

	return 0
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      string `json:"id"`
}

type rpcResponseEnvelope struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type rpcNotification struct {
	Method string
	Params json.RawMessage
}

type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
	Err    error
}

type jsonRPCClient struct {
	conn net.Conn
	enc  *json.Encoder

	writeMu sync.Mutex

	nextID atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	notifications chan rpcNotification
	errCh         chan error

	closeOnce sync.Once
	done      chan struct{}
}

func newJSONRPCClient(conn net.Conn) *jsonRPCClient {
	c := &jsonRPCClient{
		conn:          conn,
		enc:           json.NewEncoder(conn),
		pending:       make(map[string]chan rpcResponse),
		notifications: make(chan rpcNotification, 128),
		errCh:         make(chan error, 1),
		done:          make(chan struct{}),
	}

	go c.readLoop()

	return c
}

func (c *jsonRPCClient) Notifications() <-chan rpcNotification {
	return c.notifications
}

func (c *jsonRPCClient) Errors() <-chan error {
	return c.errCh
}

func (c *jsonRPCClient) Close() {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
		<-c.done
	})
}

//nolint:cyclop // response handling branches on context, stream state, and rpc errors.
func (c *jsonRPCClient) Call(ctx context.Context, method string, params any, out any) error {
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	respCh := make(chan rpcResponse, 1)

	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}

	c.writeMu.Lock()
	err := c.enc.Encode(req)
	c.writeMu.Unlock()

	if err != nil {
		c.removePending(id)

		return fmt.Errorf("sending json-rpc request: %w", err)
	}

	select {
	case <-ctx.Done():
		c.removePending(id)

		return fmt.Errorf("waiting for json-rpc response: %w", ctx.Err())
	case <-c.done:
		return errors.New("json-rpc connection closed")
	case resp := <-respCh:
		if resp.Err != nil {
			return resp.Err
		}

		if resp.Error != nil {
			return fmt.Errorf("json-rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}

		if out != nil && len(resp.Result) > 0 && string(resp.Result) != "null" {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("decoding json-rpc response: %w", err)
			}
		}

		return nil
	}
}

//nolint:cyclop // stream parser distinguishes notifications, responses, and malformed frames.
func (c *jsonRPCClient) readLoop() {
	defer close(c.done)

	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg rpcResponseEnvelope
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			slog.Debug("signal: skipping malformed json-rpc line", "error", err)

			continue
		}

		if msg.Method != "" && len(msg.ID) == 0 {
			c.notifications <- rpcNotification{Method: msg.Method, Params: msg.Params}

			continue
		}

		id := normalizeJSONRPCID(msg.ID)
		if id == "" {
			continue
		}

		c.pendingMu.Lock()
		respCh := c.pending[id]
		delete(c.pending, id)
		c.pendingMu.Unlock()

		if respCh != nil {
			respCh <- rpcResponse{Result: msg.Result, Error: msg.Error}
		}
	}

	readErr := scanner.Err()
	if readErr == nil {
		readErr = io.EOF
	}

	c.failPending(fmt.Errorf("json-rpc read loop ended: %w", readErr))

	select {
	case c.errCh <- readErr:
	default:
	}
}

func (c *jsonRPCClient) removePending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *jsonRPCClient) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	for id, ch := range c.pending {
		ch <- rpcResponse{Err: err}

		delete(c.pending, id)
	}
}

func normalizeJSONRPCID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}

	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		return num.String()
	}

	return ""
}

// Package socket implements the Backend interface over a local UNIX socket.
//
// The protocol is NDJSON, compatible with nostr-chatd so existing clients
// (noctalia nostr-chat plugin, opencrow-send CLI) work without changes.
//
// Since client and server share a filesystem (via the bind-mounted state
// dir), file transfers are just path references — no upload needed.
package socket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinpox/opencrow/backend"
)

// Config holds socket backend configuration.
type Config struct {
	SocketPath string // path to the UNIX socket
	Name       string // display name for status events
}

// --- Wire protocol (nostr-chatd compatible) ---

type eventKind string

const (
	evStatus eventKind = "status"
	evMsg    eventKind = "msg"
	evSent   eventKind = "sent"
	evAck    eventKind = "ack"
	evImg    eventKind = "img"
	evError  eventKind = "error"
)

type dir string

const (
	dirIn  dir = "in"
	dirOut dir = "out"
)

type msgState string

const (
	stateSent msgState = "sent"
)

// Wire types use camelCase JSON tags to match the nostr-chatd protocol.
//
//nolint:tagliatelle // protocol compatibility with nostr-chatd
type event struct {
	Kind        eventKind `json:"kind"`
	Msg         *message  `json:"msg,omitempty"`
	Target      string    `json:"target,omitempty"`
	Mark        string    `json:"mark,omitempty"`
	Image       string    `json:"image,omitempty"`
	State       msgState  `json:"state,omitempty"`
	Streaming   bool      `json:"streaming"`
	RelaysUp    int       `json:"relaysUp"`
	RelaysTotal int       `json:"relaysTotal,omitempty"`
	Name        string    `json:"name,omitempty"`
	Unread      int       `json:"unread,omitempty"`
	Text        string    `json:"text,omitempty"`
}

//nolint:tagliatelle // protocol compatibility with nostr-chatd
type message struct {
	ID      string   `json:"id"`
	PubKey  string   `json:"pubkey"`
	Content string   `json:"content"`
	TS      int64    `json:"ts"`
	Dir     dir      `json:"dir"`
	Ack     string   `json:"ack"`
	Read    bool     `json:"read"`
	Image   string   `json:"image,omitempty"`
	ReplyTo string   `json:"replyTo,omitempty"`
	State   msgState `json:"state"`
}

type cmdName string

const (
	cmdSend     cmdName = "send"
	cmdSendFile cmdName = "send-file"
	cmdReplay   cmdName = "replay"
	cmdMarkRead cmdName = "mark-read"
)

//nolint:tagliatelle // protocol compatibility with nostr-chatd
type command struct {
	Cmd     cmdName `json:"cmd"`
	Text    string  `json:"text,omitempty"`
	ReplyTo string  `json:"replyTo,omitempty"`
	Path    string  `json:"path,omitempty"`
	N       int     `json:"n,omitempty"`
}

// --- Backend implementation ---

const conversationID = "local"

// Backend implements backend.Backend over a local UNIX socket.
type Backend struct {
	cfg     Config
	handler backend.MessageHandler

	cancel backend.Canceler

	mu    sync.Mutex
	conns map[net.Conn]struct{}

	msgSeq atomic.Int64
}

// New creates a new socket backend.
func New(cfg Config, handler backend.MessageHandler) (*Backend, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("socket path is required")
	}

	if cfg.Name == "" {
		cfg.Name = "OpenCrow"
	}

	return &Backend{
		cfg:     cfg,
		handler: handler,
		conns:   make(map[net.Conn]struct{}),
	}, nil
}

// Run listens on the UNIX socket and dispatches commands. Blocks until ctx is cancelled.
func (b *Backend) Run(ctx context.Context) error {
	_ = os.Remove(b.cfg.SocketPath)

	if err := os.MkdirAll(filepath.Dir(b.cfg.SocketPath), 0o755); err != nil {
		return fmt.Errorf("creating socket parent dir: %w", err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "unix", b.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", b.cfg.SocketPath, err)
	}

	// Make socket world-accessible so the host user can connect
	// through the bind-mounted state dir.
	if err := os.Chmod(b.cfg.SocketPath, 0o666); err != nil {
		slog.Warn("socket: chmod failed", "error", err)
	}

	slog.Info("socket: listening", "path", b.cfg.SocketPath)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	b.cancel.Set(cancel)
	defer b.cancel.Set(nil)

	go func() { <-runCtx.Done(); ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if runCtx.Err() != nil {
				return nil
			}

			slog.Warn("socket: accept error", "error", err)

			continue
		}

		b.mu.Lock()
		b.conns[conn] = struct{}{}
		b.mu.Unlock()

		go b.handleConn(runCtx, conn)
	}
}

// Stop signals the backend to shut down.
func (b *Backend) Stop() {
	b.cancel.Cancel()
}

// Close releases resources.
func (b *Backend) Close() error {
	return nil
}

// SendMessage sends a text reply to connected clients.
func (b *Backend) SendMessage(_ context.Context, _ string, text string, replyToID string) string {
	id := b.nextID()

	b.push(event{
		Kind:      evMsg,
		Streaming: true,
		Msg: &message{
			ID:      id,
			Content: text,
			TS:      time.Now().Unix(),
			Dir:     dirIn, // "in" from the client's perspective (bot → client)
			State:   stateSent,
			ReplyTo: replyToID,
		},
	})

	return id
}

// SendFile sends a file path reference to connected clients.
func (b *Backend) SendFile(_ context.Context, _ string, filePath string) error {
	id := b.nextID()

	b.push(event{
		Kind:      evMsg,
		Streaming: true,
		Msg: &message{
			ID:      id,
			Content: "",
			TS:      time.Now().Unix(),
			Dir:     dirIn,
			State:   stateSent,
			Image:   filePath,
		},
	})

	return nil
}

// SetTyping pushes a typing status event.
func (b *Backend) SetTyping(_ context.Context, _ string, typing bool) {
	// nostr-chatd protocol doesn't have a typing event.
	// We could add one, but existing clients ignore unknown kinds gracefully.
	if typing {
		b.push(event{Kind: "typing", Streaming: true})
	}
}

// ResetConversation is a no-op for the socket backend (single conversation).
func (b *Backend) ResetConversation(_ context.Context, _ string) {}

// SystemPromptExtra returns socket-specific system prompt additions.
func (b *Backend) SystemPromptExtra() string {
	return ""
}

// MarkdownFlavor reports full Markdown support (local clients typically render it).
func (b *Backend) MarkdownFlavor() backend.MarkdownFlavor {
	return backend.MarkdownFull
}

// --- Internal ---

func (b *Backend) handleConn(ctx context.Context, conn net.Conn) {
	defer func() {
		b.mu.Lock()
		delete(b.conns, conn)
		b.mu.Unlock()
		conn.Close()
	}()

	sc := bufio.NewScanner(conn)

	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}

		var cmd command
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			slog.Warn("socket: bad command", "raw", sc.Text(), "error", err)

			continue
		}

		b.handleCommand(ctx, cmd, conn)
	}
}

func (b *Backend) handleCommand(ctx context.Context, cmd command, conn net.Conn) {
	switch cmd.Cmd {
	case cmdSend:
		b.handleSend(ctx, cmd)
	case cmdSendFile:
		b.handleSendFile(ctx, cmd)
	case cmdReplay:
		b.pushTo(conn, event{
			Kind:        evStatus,
			Streaming:   true,
			RelaysUp:    1,
			RelaysTotal: 1,
			Name:        b.cfg.Name,
		})
	case cmdMarkRead:
		// No-op for local backend.
	default:
		slog.Warn("socket: unknown command", "cmd", cmd.Cmd)
	}
}

func (b *Backend) handleSend(ctx context.Context, cmd command) {
	if cmd.Text == "" {
		return
	}

	id := b.nextID()

	b.push(event{
		Kind:      evMsg,
		Streaming: true,
		Msg: &message{
			ID:      id,
			Content: cmd.Text,
			TS:      time.Now().Unix(),
			Dir:     dirOut,
			State:   stateSent,
			ReplyTo: cmd.ReplyTo,
		},
	})

	b.handler(ctx, backend.Message{
		ConversationID: conversationID,
		SenderID:       "local",
		Text:           cmd.Text,
		MessageID:      id,
		ReplyToID:      cmd.ReplyTo,
	})
}

func (b *Backend) handleSendFile(ctx context.Context, cmd command) {
	if cmd.Path == "" {
		return
	}

	id := b.nextID()

	b.push(event{
		Kind:      evMsg,
		Streaming: true,
		Msg: &message{
			ID:    id,
			TS:    time.Now().Unix(),
			Dir:   dirOut,
			State: stateSent,
			Image: cmd.Path,
		},
	})

	b.handler(ctx, backend.Message{
		ConversationID: conversationID,
		SenderID:       "local",
		Text:           backend.AttachmentText("", cmd.Path),
		MessageID:      id,
	})
}

func (b *Backend) push(ev event) {
	line, err := json.Marshal(ev)
	if err != nil {
		slog.Error("socket: marshal failed", "error", err)

		return
	}

	line = append(line, '\n')

	b.mu.Lock()
	defer b.mu.Unlock()

	for c := range b.conns {
		if _, werr := c.Write(line); werr != nil {
			delete(b.conns, c)
			c.Close()
		}
	}
}

func (b *Backend) pushTo(conn net.Conn, ev event) {
	line, err := json.Marshal(ev)
	if err != nil {
		slog.Error("socket: marshal failed", "error", err)

		return
	}

	line = append(line, '\n')

	if _, err := conn.Write(line); err != nil {
		slog.Warn("socket: write failed", "error", err)
	}
}

func (b *Backend) nextID() string {
	return "local-" + strconv.FormatInt(b.msgSeq.Add(1), 10)
}

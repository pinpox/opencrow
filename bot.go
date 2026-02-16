package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
	_ "modernc.org/sqlite"
)

const maxMessageLen = 30000

type Bot struct {
	client        *mautrix.Client
	cryptoHelper  *cryptohelper.CryptoHelper
	pool          *PiPool
	triggerMgr    *TriggerPipeManager
	userID        id.UserID
	allowedUsers  map[string]struct{}
	initialSynced atomic.Bool
}

// SetTriggerPipeManager sets the trigger pipe manager on the bot.
func (b *Bot) SetTriggerPipeManager(mgr *TriggerPipeManager) {
	b.triggerMgr = mgr
}

func NewBot(cfg MatrixConfig, pool *PiPool) (*Bot, error) {
	client, err := mautrix.NewClient(cfg.Homeserver, id.UserID(cfg.UserID), cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("creating matrix client: %w", err)
	}

	if cfg.DeviceID != "" {
		client.DeviceID = id.DeviceID(cfg.DeviceID)
	}

	client.Log = zerolog.New(zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
		w.Out = os.Stderr
	})).With().Timestamp().Logger().Level(zerolog.InfoLevel)

	return &Bot{
		client:       client,
		pool:         pool,
		userID:       id.UserID(cfg.UserID),
		allowedUsers: cfg.AllowedUsers,
	}, nil
}

// SendToRoom sends a text message to a Matrix room by string room ID.
// Used as a callback for the heartbeat scheduler.
func (b *Bot) SendToRoom(ctx context.Context, roomID string, text string) {
	b.sendReply(ctx, id.RoomID(roomID), text)
}

func (b *Bot) Run(ctx context.Context, matrixCfg MatrixConfig) error {
	if err := b.setupCrypto(ctx, matrixCfg); err != nil {
		return fmt.Errorf("setting up e2ee: %w", err)
	}

	syncer, ok := b.client.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return errors.New("unexpected syncer type")
	}

	syncer.OnEventType(event.StateMember, func(_ context.Context, evt *event.Event) {
		go b.handleMembership(ctx, evt)
	})

	syncer.OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		go b.handleMessage(ctx, evt)
	})

	syncer.OnSync(func(_ context.Context, resp *mautrix.RespSync, since string) bool {
		if since != "" {
			b.initialSynced.Store(true)
		}

		slog.Debug("sync", "since", since, "joined_rooms", len(resp.Rooms.Join), "invited_rooms", len(resp.Rooms.Invite))

		return true
	})

	slog.Info("starting matrix sync", "user_id", b.userID)

	if err := b.client.SyncWithContext(ctx); err != nil {
		return fmt.Errorf("matrix sync: %w", err)
	}

	return nil
}

func (b *Bot) Stop() {
	b.client.StopSync()
}

func (b *Bot) Close() error {
	if b.cryptoHelper != nil {
		return fmt.Errorf("closing crypto helper: %w", b.cryptoHelper.Close())
	}

	return nil
}

func (b *Bot) setupCrypto(ctx context.Context, cfg MatrixConfig) error {
	// Resolve device ID from server if not configured
	if b.client.DeviceID == "" {
		resp, err := b.client.Whoami(ctx)
		if err != nil {
			return fmt.Errorf("fetching device ID from server: %w", err)
		}

		if resp.DeviceID == "" {
			return errors.New("server did not return a device ID; set OPENCROW_MATRIX_DEVICE_ID")
		}

		b.client.DeviceID = resp.DeviceID

		slog.Info("resolved device ID from server", "device_id", resp.DeviceID)
	}

	// Open SQLite database with pure-Go driver
	sqlDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_txlock=immediate&_pragma=foreign_keys(1)&_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)", cfg.CryptoDBPath))
	if err != nil {
		return fmt.Errorf("opening crypto database: %w", err)
	}

	db, err := dbutil.NewWithDB(sqlDB, "sqlite")
	if err != nil {
		sqlDB.Close()

		return fmt.Errorf("wrapping crypto database: %w", err)
	}

	cryptoHelper, err := cryptohelper.NewCryptoHelper(b.client, []byte(cfg.PickleKey), db)
	if err != nil {
		db.Close()

		return fmt.Errorf("creating crypto helper: %w", err)
	}

	if err := cryptoHelper.Init(ctx); err != nil {
		db.Close()

		return fmt.Errorf("initializing crypto: %w", err)
	}

	b.client.Crypto = cryptoHelper
	b.cryptoHelper = cryptoHelper

	slog.Info("e2ee initialized", "device_id", b.client.DeviceID)

	return nil
}

func (b *Bot) handleMembership(ctx context.Context, evt *event.Event) {
	if !b.initialSynced.Load() {
		return
	}

	mem := evt.Content.AsMember()
	if mem == nil {
		return
	}

	switch mem.Membership {
	case event.MembershipInvite:
		b.handleInvite(ctx, evt)
	case event.MembershipLeave, event.MembershipBan:
		b.handleLeave(ctx, evt, mem)
	}
}

func (b *Bot) handleInvite(ctx context.Context, evt *event.Event) {
	if id.UserID(*evt.StateKey) != b.userID {
		return
	}

	if len(b.allowedUsers) > 0 {
		if _, ok := b.allowedUsers[string(evt.Sender)]; !ok {
			slog.Info("ignoring invite from non-allowed user", "sender", evt.Sender, "room", evt.RoomID)

			return
		}
	}

	slog.Info("accepting invite", "sender", evt.Sender, "room", evt.RoomID)

	_, err := b.client.JoinRoomByID(ctx, evt.RoomID)
	if err != nil {
		slog.Error("failed to join room", "room", evt.RoomID, "error", err)
	}
}

func (b *Bot) handleLeave(ctx context.Context, evt *event.Event, mem *event.MemberEventContent) {
	target := id.UserID(*evt.StateKey)
	roomID := string(evt.RoomID)

	// Bot itself was removed or banned
	if target == b.userID {
		slog.Info("bot removed from room, cleaning up", "room", roomID, "membership", mem.Membership)
		b.cleanupRoom(roomID)

		return
	}

	// Another user left — check if the bot is now alone
	members, err := b.client.JoinedMembers(ctx, evt.RoomID)
	if err != nil {
		slog.Warn("failed to query room members", "room", roomID, "error", err)

		return
	}

	if len(members.Joined) > 1 {
		return
	}

	slog.Info("bot is alone in room, leaving and cleaning up", "room", roomID)

	if _, err := b.client.LeaveRoom(ctx, evt.RoomID); err != nil {
		slog.Error("failed to leave room", "room", roomID, "error", err)
	}

	b.cleanupRoom(roomID)
}

// cleanupRoom kills the pi process and removes the session directory for a room.
func (b *Bot) cleanupRoom(roomID string) {
	if b.triggerMgr != nil {
		b.triggerMgr.StopRoom(roomID)
	}

	b.pool.Remove(roomID)

	sessionDir := filepath.Join(b.pool.cfg.SessionDir, sanitizeRoomID(roomID))

	if err := os.RemoveAll(sessionDir); err != nil {
		slog.Error("failed to remove session directory", "room", roomID, "path", sessionDir, "error", err)

		return
	}

	slog.Info("removed session directory", "room", roomID, "path", sessionDir)
}

func (b *Bot) handleMessage(ctx context.Context, evt *event.Event) {
	if !b.initialSynced.Load() {
		return
	}

	if evt.Sender == b.userID {
		return
	}

	if len(b.allowedUsers) > 0 {
		if _, ok := b.allowedUsers[string(evt.Sender)]; !ok {
			return
		}
	}

	msg := evt.Content.AsMessage()
	if msg == nil {
		return
	}

	switch msg.MsgType {
	case event.MsgText, event.MsgImage, event.MsgFile, event.MsgAudio, event.MsgVideo:
		// supported
	default:
		return
	}

	roomID := string(evt.RoomID)
	text := msg.Body

	slog.Info("received message", "room", roomID, "sender", evt.Sender, "type", msg.MsgType, "len", len(text))

	// For file/image/audio/video messages, download the attachment and rewrite the prompt
	if msg.MsgType != event.MsgText {
		filePath, err := b.downloadAttachment(ctx, msg, roomID)
		if err != nil {
			slog.Error("failed to download attachment", "room", roomID, "error", err)
			b.sendReply(ctx, evt.RoomID, fmt.Sprintf("Failed to download attachment: %v", err))

			return
		}

		caption := msg.Body
		if caption == "" || caption == msg.FileName {
			caption = "no caption"
		}

		text = fmt.Sprintf("[User sent a file (%s): %s]\nUse the read tool to view it.", caption, filePath)
	}

	if text == "!restart" {
		b.pool.Remove(roomID)
		b.sendReply(ctx, evt.RoomID, "Session restarted. Next message will use a fresh process.")

		return
	}

	if text == "!skills" {
		b.sendReply(ctx, evt.RoomID, b.pool.SkillsSummary())

		return
	}

	if text == "!rooms" {
		b.sendReply(ctx, evt.RoomID, b.pool.RoomsSummary())

		return
	}

	pi, err := b.pool.Get(ctx, roomID)
	if err != nil {
		slog.Error("failed to get pi process", "room", roomID, "error", err)
		b.sendReply(ctx, evt.RoomID, fmt.Sprintf("Error starting AI backend: %v", err))

		return
	}

	if b.triggerMgr != nil {
		b.triggerMgr.StartRoom(ctx, roomID)
	}

	reply, err := pi.Prompt(ctx, text)
	if err != nil {
		slog.Error("pi prompt failed", "room", roomID, "error", err)
		b.pool.Remove(roomID)

		reply = fmt.Sprintf("Error: %v", err)
	}

	if reply == "" {
		reply = "(empty response)"
	}

	// Extract <sendfile> tags and upload any referenced files
	cleanReply, filePaths := extractSendFiles(reply)

	for _, fp := range filePaths {
		if err := b.sendFile(ctx, evt.RoomID, fp); err != nil {
			slog.Error("failed to send file", "room", roomID, "path", fp, "error", err)
			cleanReply += fmt.Sprintf("\n\n(failed to send file %s: %v)", filepath.Base(fp), err)
		}
	}

	if cleanReply != "" {
		b.sendReply(ctx, evt.RoomID, cleanReply)
	}
}

// downloadAttachment downloads a Matrix media attachment to the session directory.
// Handles both unencrypted (msg.URL) and encrypted (msg.File) media.
// Returns the local file path.
func (b *Bot) downloadAttachment(ctx context.Context, msg *event.MessageEventContent, roomID string) (string, error) {
	var urlStr id.ContentURIString

	encrypted := msg.File != nil && msg.File.URL != ""
	if encrypted {
		urlStr = msg.File.URL
	} else if msg.URL != "" {
		urlStr = msg.URL
	} else {
		return "", errors.New("message has no media URL")
	}

	mxcURL, err := urlStr.Parse()
	if err != nil {
		return "", fmt.Errorf("parsing mxc URL: %w", err)
	}

	sessionDir := filepath.Join(b.pool.cfg.SessionDir, sanitizeRoomID(roomID))
	downloadDir := filepath.Join(sessionDir, "attachments")

	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return "", fmt.Errorf("creating attachments dir: %w", err)
	}

	filename := msg.FileName
	if filename == "" {
		filename = msg.Body
	}

	if filename == "" {
		filename = "image.png"
	}

	// Strip directory components to prevent path traversal
	filename = filepath.Base(filename)

	destPath := filepath.Join(downloadDir, filename)

	if encrypted {
		ciphertext, err := b.client.DownloadBytes(ctx, mxcURL)
		if err != nil {
			return "", fmt.Errorf("downloading encrypted media: %w", err)
		}

		plaintext, err := msg.File.Decrypt(ciphertext)
		if err != nil {
			return "", fmt.Errorf("decrypting media: %w", err)
		}

		if err := os.WriteFile(destPath, plaintext, 0o644); err != nil {
			return "", fmt.Errorf("writing decrypted file: %w", err)
		}
	} else {
		resp, err := b.client.Download(ctx, mxcURL)
		if err != nil {
			return "", fmt.Errorf("downloading from matrix: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("download returned status %d", resp.StatusCode)
		}

		f, err := os.Create(destPath)
		if err != nil {
			return "", fmt.Errorf("creating file: %w", err)
		}
		defer f.Close()

		if _, err := io.Copy(f, resp.Body); err != nil {
			return "", fmt.Errorf("writing file: %w", err)
		}
	}

	slog.Info("downloaded attachment", "room", roomID, "path", destPath, "encrypted", encrypted)

	return destPath, nil
}

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

// sendFile reads a file from disk, uploads it to Matrix, and sends it as an attachment message.
func (b *Bot) sendFile(ctx context.Context, roomID id.RoomID, filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}

	// Detect MIME type: try extension first, fall back to content sniffing
	contentType := mime.TypeByExtension(filepath.Ext(filePath))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}

	resp, err := b.client.UploadMedia(ctx, mautrix.ReqUploadMedia{
		ContentBytes: data,
		ContentType:  contentType,
		FileName:     filepath.Base(filePath),
	})
	if err != nil {
		return fmt.Errorf("uploading media: %w", err)
	}

	// Pick appropriate message type based on MIME category
	msgType := event.MsgFile
	switch {
	case strings.HasPrefix(contentType, "image/"):
		msgType = event.MsgImage
	case strings.HasPrefix(contentType, "audio/"):
		msgType = event.MsgAudio
	case strings.HasPrefix(contentType, "video/"):
		msgType = event.MsgVideo
	}

	content := &event.MessageEventContent{
		MsgType:  msgType,
		Body:     filepath.Base(filePath),
		URL:      resp.ContentURI.CUString(),
		FileName: filepath.Base(filePath),
		Info: &event.FileInfo{
			MimeType: contentType,
			Size:     len(data),
		},
	}

	_, err = b.client.SendMessageEvent(ctx, roomID, event.EventMessage, content)
	if err != nil {
		return fmt.Errorf("sending file message: %w", err)
	}

	slog.Info("sent file to room", "room", roomID, "path", filePath, "mime", contentType, "size", len(data))

	return nil
}

func (b *Bot) sendReply(ctx context.Context, roomID id.RoomID, text string) {
	for len(text) > 0 {
		chunk := text
		if len(chunk) > maxMessageLen {
			cutoff := maxMessageLen

			if idx := lastNewline(chunk[:cutoff]); idx > 0 {
				cutoff = idx + 1
			}

			chunk = text[:cutoff]
			text = text[cutoff:]
		} else {
			text = ""
		}

		content := format.RenderMarkdown(chunk, true, false)

		_, err := b.client.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
		if err != nil {
			slog.Error("failed to send message", "room", roomID, "error", err)

			return
		}
	}
}

func lastNewline(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return i
		}
	}

	return -1
}

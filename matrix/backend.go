// Package matrix implements the Backend interface for Matrix messaging.
package matrix

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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinpox/opencrow/backend"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
	_ "modernc.org/sqlite" // register sqlite driver for database/sql
)

const maxMessageLen = 30000

// Config holds Matrix-specific configuration.
type Config struct {
	Homeserver     string
	UserID         string
	AccessToken    string
	DeviceID       string
	AllowedUsers   map[string]struct{}
	PickleKey      string
	CryptoDBPath   string
	SessionBaseDir string // base dir for per-conversation session subdirs
}

// Backend implements backend.Backend for Matrix.
type Backend struct {
	client        *mautrix.Client
	cryptoHelper  *cryptohelper.CryptoHelper
	handler       backend.MessageHandler
	cfg           Config
	userID        id.UserID
	allowedUsers  map[string]struct{}
	initialSynced atomic.Bool

	roomMu     sync.Mutex
	activeRoom string

	// onRoomCleanup is called when a room is cleaned up (leave/ban).
	// Wired by the caller to kill pi processes and stop trigger pipes.
	onRoomCleanup func(roomID string)
}

// New creates a new Matrix backend.
func New(cfg Config, handler backend.MessageHandler) (*Backend, error) {
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

	return &Backend{
		client:       client,
		handler:      handler,
		cfg:          cfg,
		userID:       id.UserID(cfg.UserID),
		allowedUsers: cfg.AllowedUsers,
	}, nil
}

// SetRoomCleanupCallback sets the callback invoked when a room is cleaned up.
func (b *Backend) SetRoomCleanupCallback(fn func(roomID string)) {
	b.onRoomCleanup = fn
}

// Run starts the Matrix sync loop (blocking).
func (b *Backend) Run(ctx context.Context) error {
	if err := b.setupCrypto(ctx); err != nil {
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

// Stop signals the Matrix sync to stop.
func (b *Backend) Stop() {
	b.client.StopSync()
}

// Close releases crypto resources.
func (b *Backend) Close() error {
	if b.cryptoHelper != nil {
		return fmt.Errorf("closing crypto helper: %w", b.cryptoHelper.Close())
	}

	return nil
}

// SendMessage sends a text message to a Matrix room. When replyToID is
// non-empty, the first chunk is sent as a reply to that event.
// Returns the event ID of the last sent chunk (or "" on failure).
func (b *Backend) SendMessage(ctx context.Context, conversationID string, text string, replyToID string) string {
	roomID := id.RoomID(conversationID)
	firstChunk := true

	var lastEventID string

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

		if firstChunk && replyToID != "" {
			content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(id.EventID(replyToID))
		}

		firstChunk = false

		resp, err := b.client.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
		if err != nil {
			slog.Error("failed to send message", "room", roomID, "error", err)

			return lastEventID
		}

		lastEventID = string(resp.EventID)
	}

	return lastEventID
}

// SendFile uploads and sends a file to a Matrix room.
func (b *Backend) SendFile(ctx context.Context, conversationID string, filePath string) error {
	roomID := id.RoomID(conversationID)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}

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

// SetTyping sets the typing indicator for a Matrix room.
func (b *Backend) SetTyping(ctx context.Context, conversationID string, typing bool) {
	timeout := 30 * time.Second
	if !typing {
		timeout = 0
	}

	if _, err := b.client.UserTyping(ctx, id.RoomID(conversationID), typing, timeout); err != nil {
		slog.Warn("failed to set typing indicator", "room", conversationID, "error", err)
	}
}

// ResetConversation clears the active room tracking.
func (b *Backend) ResetConversation(_ context.Context, conversationID string) {
	b.roomMu.Lock()
	if b.activeRoom == conversationID {
		b.activeRoom = ""
	}
	b.roomMu.Unlock()
}

// SystemPromptExtra returns Matrix-specific system prompt context.
func (b *Backend) SystemPromptExtra() string {
	return `You are living in a Matrix chat room.

## Sending files to the user

You can send files back to the user in the Matrix chat. To do this, include a <sendfile> tag
in your response with the absolute path to the file:

<sendfile>/path/to/file.png</sendfile>

The bot will upload the file and deliver it as an attachment. You can include multiple
<sendfile> tags in a single response. The tags will be stripped from the text message.
Use this whenever you create a file the user should receive (charts, images, PDFs, scripts, etc.).

## File attachments from the user

When users send files (images, documents, etc.) in the chat, they are downloaded to your
session directory. You'll see them as "[User sent a file (<caption>): <path>]". Use the
read tool to view them.`
}

// --- internal handlers ---

func (b *Backend) setupCrypto(ctx context.Context) error {
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

	sqlDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_txlock=immediate&_pragma=foreign_keys(1)&_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)", b.cfg.CryptoDBPath))
	if err != nil {
		return fmt.Errorf("opening crypto database: %w", err)
	}

	db, err := dbutil.NewWithDB(sqlDB, "sqlite")
	if err != nil {
		sqlDB.Close()

		return fmt.Errorf("wrapping crypto database: %w", err)
	}

	cryptoHelper, err := cryptohelper.NewCryptoHelper(b.client, []byte(b.cfg.PickleKey), db)
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

	if err := b.ensureCrossSigning(ctx); err != nil {
		slog.Warn("cross-signing setup failed", "error", err)
	}

	return nil
}

func (b *Backend) ensureCrossSigning(ctx context.Context) error {
	mach := b.cryptoHelper.Machine()

	hasKeys, isVerified, err := mach.GetOwnVerificationStatus(ctx)
	if err != nil {
		return fmt.Errorf("checking verification status: %w", err)
	}

	if hasKeys && isVerified {
		slog.Info("device is already cross-signed")

		return nil
	}

	slog.Info("setting up cross-signing", "has_keys", hasKeys, "is_verified", isVerified)

	recoveryKey, err := mach.GenerateAndVerifyWithRecoveryKey(ctx)
	if err != nil {
		return fmt.Errorf("generating cross-signing keys: %w", err)
	}

	slog.Info("cross-signing setup complete, store this recovery key securely", "recovery_key", recoveryKey)

	return nil
}

func (b *Backend) handleMembership(ctx context.Context, evt *event.Event) {
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
	case event.MembershipJoin, event.MembershipKnock:
		// Intentionally ignored — no action needed for joins or knocks.
	}
}

func (b *Backend) handleInvite(ctx context.Context, evt *event.Event) {
	if id.UserID(*evt.StateKey) != b.userID {
		return
	}

	if len(b.allowedUsers) > 0 {
		if _, ok := b.allowedUsers[string(evt.Sender)]; !ok {
			slog.Info("ignoring invite from non-allowed user", "sender", evt.Sender, "room", evt.RoomID)

			return
		}
	}

	b.roomMu.Lock()
	if b.activeRoom != "" {
		b.roomMu.Unlock()
		slog.Info("ignoring invite, already active in a room", "active_room", b.activeRoom, "invited_room", evt.RoomID)

		return
	}
	b.roomMu.Unlock()

	slog.Info("accepting invite", "sender", evt.Sender, "room", evt.RoomID)

	_, err := b.client.JoinRoomByID(ctx, evt.RoomID)
	if err != nil {
		slog.Error("failed to join room", "room", evt.RoomID, "error", err)

		return
	}

	b.roomMu.Lock()
	b.activeRoom = string(evt.RoomID)
	b.roomMu.Unlock()
}

func (b *Backend) handleLeave(ctx context.Context, evt *event.Event, mem *event.MemberEventContent) {
	target := id.UserID(*evt.StateKey)
	roomID := string(evt.RoomID)

	if target == b.userID {
		slog.Info("bot removed from room, cleaning up", "room", roomID, "membership", mem.Membership)
		b.cleanupRoom(roomID)

		return
	}

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

func (b *Backend) cleanupRoom(roomID string) {
	b.roomMu.Lock()
	if b.activeRoom == roomID {
		b.activeRoom = ""
	}
	b.roomMu.Unlock()

	if b.onRoomCleanup != nil {
		b.onRoomCleanup(roomID)
	}
}

func (b *Backend) handleMessage(ctx context.Context, evt *event.Event) {
	msg := b.filterMessage(evt)
	if msg == nil {
		return
	}

	if err := b.client.MarkRead(ctx, evt.RoomID, evt.ID); err != nil {
		slog.Warn("failed to send read receipt", "event_id", evt.ID, "error", err)
	}

	roomID := string(evt.RoomID)
	b.trackRoom(roomID)

	text := msg.Body

	slog.Info("received message", "room", roomID, "sender", evt.Sender, "type", msg.MsgType, "len", len(text))

	if text == "!verify" {
		b.handleVerify(ctx, evt.RoomID)

		return
	}

	if msg.MsgType != event.MsgText {
		text = b.handleAttachment(ctx, msg, roomID)
		if text == "" {
			return
		}
	}

	var replyToID string

	if msg.RelatesTo != nil {
		if replyTo := msg.RelatesTo.GetReplyTo(); replyTo != "" {
			replyToID = string(replyTo)
		}
	}

	b.handler(ctx, backend.Message{
		ConversationID: roomID,
		SenderID:       string(evt.Sender),
		Text:           text,
		MessageID:      string(evt.ID),
		ReplyToID:      replyToID,
	})
}

// filterMessage checks whether the event should be processed and returns
// the message content, or nil if the event should be dropped.
func (b *Backend) filterMessage(evt *event.Event) *event.MessageEventContent {
	if !b.initialSynced.Load() {
		return nil
	}

	if evt.Sender == b.userID {
		return nil
	}

	if len(b.allowedUsers) > 0 {
		if _, ok := b.allowedUsers[string(evt.Sender)]; !ok {
			return nil
		}
	}

	msg := evt.Content.AsMessage()
	if msg == nil {
		return nil
	}

	switch msg.MsgType {
	case event.MsgText, event.MsgImage, event.MsgFile, event.MsgAudio, event.MsgVideo:
		return msg
	case event.MsgEmote, event.MsgNotice, event.MsgLocation, event.MsgVerificationRequest, event.MsgBeeperGallery:
		return nil
	default:
		return nil
	}
}

// trackRoom sets the active room if not already set.
func (b *Backend) trackRoom(roomID string) {
	b.roomMu.Lock()
	if b.activeRoom == "" {
		b.activeRoom = roomID
		slog.Info("discovered active room from message", "room", roomID)
	}
	b.roomMu.Unlock()
}

// handleAttachment downloads and formats an attachment message.
// Returns the formatted text, or empty string on failure (error already reported).
func (b *Backend) handleAttachment(ctx context.Context, msg *event.MessageEventContent, roomID string) string {
	filePath, err := b.downloadAttachment(ctx, msg, roomID)
	if err != nil {
		slog.Error("failed to download attachment", "room", roomID, "error", err)
		b.SendMessage(ctx, roomID, fmt.Sprintf("Failed to download attachment: %v", err), "")

		return ""
	}

	caption := msg.Body
	if caption == "" || caption == msg.FileName {
		caption = "no caption"
	}

	return fmt.Sprintf("[User sent a file (%s): %s]\nUse the read tool to view it.", caption, filePath)
}

func (b *Backend) handleVerify(ctx context.Context, roomID id.RoomID) {
	if err := b.ensureCrossSigning(ctx); err != nil {
		b.SendMessage(ctx, string(roomID), fmt.Sprintf(
			"Cross-signing failed: %v\n\n"+
				"If the homeserver requires browser approval, log in as the bot at:\n"+
				"https://account.matrix.org/account/?action=org.matrix.cross_signing_reset\n\n"+
				"Approve the reset, then run `!verify` again.",
			err), "")
	} else {
		b.SendMessage(ctx, string(roomID), "Cross-signing verified.", "")
	}
}

func (b *Backend) downloadAttachment(ctx context.Context, msg *event.MessageEventContent, roomID string) (string, error) {
	urlStr, encrypted := attachmentURL(msg)
	if urlStr == "" {
		return "", errors.New("message has no media URL")
	}

	mxcURL, err := urlStr.Parse()
	if err != nil {
		return "", fmt.Errorf("parsing mxc URL: %w", err)
	}

	destPath, err := attachmentDestPath(b.cfg.SessionBaseDir, msg)
	if err != nil {
		return "", err
	}

	switch {
	case encrypted:
		if err := b.downloadEncrypted(ctx, mxcURL, msg, destPath); err != nil {
			return "", err
		}
	default:
		if err := b.downloadPlain(ctx, mxcURL, destPath); err != nil {
			return "", err
		}
	}

	slog.Info("downloaded attachment", "room", roomID, "path", destPath, "encrypted", encrypted)

	return destPath, nil
}

// attachmentURL returns the content URI and whether the attachment is encrypted.
func attachmentURL(msg *event.MessageEventContent) (id.ContentURIString, bool) {
	switch {
	case msg.File != nil && msg.File.URL != "":
		return msg.File.URL, true
	case msg.URL != "":
		return msg.URL, false
	default:
		return "", false
	}
}

// attachmentDestPath builds the local filesystem path for a downloaded attachment.
func attachmentDestPath(sessionBaseDir string, msg *event.MessageEventContent) (string, error) {
	downloadDir := filepath.Join(sessionBaseDir, "attachments")
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

	filename = filepath.Base(filename)

	return filepath.Join(downloadDir, filename), nil
}

// downloadEncrypted downloads and decrypts an encrypted Matrix attachment.
func (b *Backend) downloadEncrypted(ctx context.Context, mxcURL id.ContentURI, msg *event.MessageEventContent, destPath string) error {
	ciphertext, err := b.client.DownloadBytes(ctx, mxcURL)
	if err != nil {
		return fmt.Errorf("downloading encrypted media: %w", err)
	}

	if err := msg.File.DecryptInPlace(ciphertext); err != nil {
		return fmt.Errorf("decrypting media: %w", err)
	}

	if err := os.WriteFile(destPath, ciphertext, 0o600); err != nil {
		return fmt.Errorf("writing decrypted file: %w", err)
	}

	return nil
}

// downloadPlain downloads an unencrypted Matrix attachment.
func (b *Backend) downloadPlain(ctx context.Context, mxcURL id.ContentURI, destPath string) error {
	resp, err := b.client.Download(ctx, mxcURL)
	if err != nil {
		return fmt.Errorf("downloading from matrix: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	return nil
}

func lastNewline(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return i
		}
	}

	return -1
}

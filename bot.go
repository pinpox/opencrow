package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "modernc.org/sqlite"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

const maxMessageLen = 30000

type Bot struct {
	client        *mautrix.Client
	cryptoHelper  *cryptohelper.CryptoHelper
	pool          *PiPool
	userID        id.UserID
	allowedUsers  map[string]struct{}
	initialSynced atomic.Bool
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

func (b *Bot) setupCrypto(ctx context.Context, cfg MatrixConfig) error {
	// Resolve device ID from server if not configured
	if b.client.DeviceID == "" {
		resp, err := b.client.Whoami(ctx)
		if err != nil {
			return fmt.Errorf("fetching device ID from server: %w", err)
		}
		if resp.DeviceID == "" {
			return fmt.Errorf("server did not return a device ID; set OPENCROW_MATRIX_DEVICE_ID")
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

func (b *Bot) Run(ctx context.Context, matrixCfg MatrixConfig) error {
	if err := b.setupCrypto(ctx, matrixCfg); err != nil {
		return fmt.Errorf("setting up e2ee: %w", err)
	}

	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)

	syncer.OnEventType(event.StateMember, func(_ context.Context, evt *event.Event) {
		go b.handleInvite(ctx, evt)
	})

	syncer.OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		go b.handleMessage(ctx, evt)
	})

	syncer.OnSync(func(ctx context.Context, resp *mautrix.RespSync, since string) bool {
		if since != "" {
			b.initialSynced.Store(true)
		}
		slog.Debug("sync", "since", since, "joined_rooms", len(resp.Rooms.Join), "invited_rooms", len(resp.Rooms.Invite))
		return true
	})

	slog.Info("starting matrix sync", "user_id", b.userID)
	return b.client.SyncWithContext(ctx)
}

func (b *Bot) Stop() {
	b.client.StopSync()
}

func (b *Bot) Close() error {
	if b.cryptoHelper != nil {
		return b.cryptoHelper.Close()
	}
	return nil
}

func (b *Bot) handleInvite(ctx context.Context, evt *event.Event) {
	mem := evt.Content.AsMember()
	if mem == nil || mem.Membership != event.MembershipInvite {
		return
	}
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
	if msg == nil || msg.MsgType != event.MsgText {
		return
	}

	roomID := string(evt.RoomID)
	text := msg.Body

	slog.Info("received message", "room", roomID, "sender", evt.Sender, "len", len(text))

	pi, err := b.pool.Get(ctx, roomID)
	if err != nil {
		slog.Error("failed to get pi process", "room", roomID, "error", err)
		b.sendReply(ctx, evt.RoomID, fmt.Sprintf("Error starting AI backend: %v", err))
		return
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

	b.sendReply(ctx, evt.RoomID, reply)
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

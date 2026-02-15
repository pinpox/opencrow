package main

import (
	"context"
	"fmt"
	"log/slog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const maxMessageLen = 30000

type Bot struct {
	client       *mautrix.Client
	pool         *PiPool
	userID       id.UserID
	allowedUsers map[string]struct{}
}

func NewBot(cfg MatrixConfig, pool *PiPool) (*Bot, error) {
	client, err := mautrix.NewClient(cfg.Homeserver, id.UserID(cfg.UserID), cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("creating matrix client: %w", err)
	}

	if cfg.DeviceID != "" {
		client.DeviceID = id.DeviceID(cfg.DeviceID)
	}

	return &Bot{
		client:       client,
		pool:         pool,
		userID:       id.UserID(cfg.UserID),
		allowedUsers: cfg.AllowedUsers,
	}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)

	syncer.OnEventType(event.StateMember, func(_ context.Context, evt *event.Event) {
		go b.handleInvite(ctx, evt)
	})

	syncer.OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		go b.handleMessage(ctx, evt)
	})

	syncer.OnSync(func(ctx context.Context, resp *mautrix.RespSync, since string) bool {
		slog.Debug("sync", "since", since, "joined_rooms", len(resp.Rooms.Join), "invited_rooms", len(resp.Rooms.Invite))
		return true
	})

	slog.Info("starting matrix sync", "user_id", b.userID)
	return b.client.SyncWithContext(ctx)
}

func (b *Bot) Stop() {
	b.client.StopSync()
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

		_, err := b.client.SendText(ctx, roomID, chunk)
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

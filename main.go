package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/pinpox/opencrow/backend"
	"github.com/pinpox/opencrow/matrix"
	nostrbackend "github.com/pinpox/opencrow/nostr"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(os.Getenv("OPENCROW_LOG_LEVEL")),
	})))

	os.Exit(run())
}

func run() int {
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)

		return 1
	}

	slog.Info("config loaded", "backend", cfg.BackendType)

	pool := NewPiPool(cfg.Pi)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.StartIdleReaper(ctx)

	// The App needs the backend, but the backend needs the App's handler.
	// Resolve this with a handler that forwards to the App once it's wired.
	var app *App
	handler := func(ctx context.Context, msg backend.Message) {
		app.HandleMessage(ctx, msg)
	}

	var b backend.Backend

	switch cfg.BackendType {
	case "matrix":
		b, err = createMatrixBackend(cfg, pool, handler)
	case "nostr":
		b = createNostrBackend(cfg, handler)
	}

	if err != nil {
		slog.Error("failed to create backend", "error", err)
		return 1
	}

	app = NewApp(b, pool, nil)

	// Update system prompt with backend-specific context
	cfg.Pi.SystemPrompt = app.systemPrompt(cfg.Pi.SystemPrompt)

	hb := NewHeartbeatScheduler(pool, cfg.Pi, cfg.Heartbeat, func(ctx context.Context, roomID string, text string) {
		b.SendMessage(ctx, roomID, text)
	})
	hb.Start(ctx)

	triggerMgr := NewTriggerPipeManager(pool, cfg.Pi, defaultTriggerPrompt,
		func(ctx context.Context, roomID string, text string) {
			b.SendMessage(ctx, roomID, text)
		},
		func(ctx context.Context, roomID string, typing bool) {
			b.SetTyping(ctx, roomID, typing)
		},
	)
	triggerMgr.Start(ctx)
	app.triggerMgr = triggerMgr

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		b.Stop()
		pool.StopAll()
		cancel()
	}()

	slog.Info("opencrow starting")

	if err := b.Run(ctx); err != nil {
		if ctx.Err() != nil {
			slog.Info("shutdown complete")
		} else {
			slog.Error("backend exited with error", "error", err)

			return 1
		}
	}

	if err := b.Close(); err != nil {
		slog.Error("failed to close backend", "error", err)
	}

	return 0
}

func createMatrixBackend(cfg *Config, pool *PiPool, handler backend.MessageHandler) (backend.Backend, error) {
	matrixCfg := matrix.Config{
		Homeserver:     cfg.Matrix.Homeserver,
		UserID:         cfg.Matrix.UserID,
		AccessToken:    cfg.Matrix.AccessToken,
		DeviceID:       cfg.Matrix.DeviceID,
		AllowedUsers:   cfg.Matrix.AllowedUsers,
		PickleKey:      cfg.Matrix.PickleKey,
		CryptoDBPath:   cfg.Matrix.CryptoDBPath,
		SessionBaseDir: cfg.Pi.SessionDir,
	}

	b, err := matrix.New(matrixCfg, handler)
	if err != nil {
		return nil, err
	}

	b.SetRoomCleanupCallback(func(roomID string) {
		pool.Remove(roomID)
	})

	return b, nil
}

func createNostrBackend(cfg *Config, handler backend.MessageHandler) backend.Backend {
	nostrCfg := nostrbackend.Config{
		PrivateKey:     cfg.Nostr.PrivateKey,
		Relays:         cfg.Nostr.Relays,
		BlossomServers: cfg.Nostr.BlossomServers,
		AllowedUsers:   cfg.Nostr.AllowedUsers,
		SessionBaseDir: cfg.Pi.SessionDir,
	}

	return nostrbackend.NewBackend(nostrCfg, handler)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

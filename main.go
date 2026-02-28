package main

import (
	"context"
	"fmt"
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

	b, app, err := wireServices(ctx, cfg, pool)
	if err != nil {
		slog.Error("failed to initialize services", "error", err)

		return 1
	}

	setupShutdown(b, pool, cancel)

	slog.Info("opencrow starting")

	return runBackend(ctx, b, app)
}

// wireServices creates the backend, app, heartbeat scheduler, and trigger pipe manager.
func wireServices(ctx context.Context, cfg *Config, pool *PiPool) (backend.Backend, *App, error) { //nolint:ireturn // factory returns interface by design
	var app *App

	handler := func(ctx context.Context, msg backend.Message) {
		if app == nil {
			slog.Error("received message before app was initialized")

			return
		}

		app.HandleMessage(ctx, msg)
	}

	b, err := createBackend(cfg, pool, handler)
	if err != nil {
		return nil, nil, err
	}

	app = NewApp(b, pool, nil)
	cfg.Pi.SystemPrompt = app.systemPrompt(cfg.Pi.SystemPrompt)

	startSchedulers(ctx, cfg, pool, b, app)

	return b, app, nil
}

func createBackend(cfg *Config, pool *PiPool, handler backend.MessageHandler) (backend.Backend, error) { //nolint:ireturn // factory returns interface by design
	switch cfg.BackendType {
	case backendMatrix:
		return createMatrixBackend(cfg, pool, handler)
	case backendNostr:
		return createNostrBackend(cfg, handler)
	default:
		return nil, fmt.Errorf("unsupported backend type: %q", cfg.BackendType)
	}
}

func startSchedulers(ctx context.Context, cfg *Config, pool *PiPool, b backend.Backend, app *App) {
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
}

func setupShutdown(b backend.Backend, pool *PiPool, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		b.Stop()
		pool.StopAll()
		cancel()
	}()
}

func runBackend(ctx context.Context, b backend.Backend, _ *App) int {
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

func createMatrixBackend(cfg *Config, pool *PiPool, handler backend.MessageHandler) (*matrix.Backend, error) {
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
		return nil, fmt.Errorf("creating matrix backend: %w", err)
	}

	b.SetRoomCleanupCallback(func(roomID string) {
		pool.Remove(roomID)
	})

	return b, nil
}

func createNostrBackend(cfg *Config, handler backend.MessageHandler) (*nostrbackend.Backend, error) {
	nostrCfg := nostrbackend.Config{
		PrivateKey:     cfg.Nostr.PrivateKey,
		Relays:         cfg.Nostr.Relays,
		DMRelays:       cfg.Nostr.DMRelays,
		BlossomServers: cfg.Nostr.BlossomServers,
		AllowedUsers:   cfg.Nostr.AllowedUsers,
		SessionBaseDir: cfg.Pi.SessionDir,
		Profile: nostrbackend.ProfileConfig{
			Name:        cfg.Nostr.Name,
			DisplayName: cfg.Nostr.DisplayName,
			About:       cfg.Nostr.About,
			Picture:     cfg.Nostr.Picture,
		},
	}

	b, err := nostrbackend.NewBackend(nostrCfg, handler)
	if err != nil {
		return nil, fmt.Errorf("creating nostr backend: %w", err)
	}

	return b, nil
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

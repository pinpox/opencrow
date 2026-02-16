package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	os.Exit(run())
}

func run() int {
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)

		return 1
	}

	slog.Info("config loaded")

	pool := NewPiPool(cfg.Pi)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.StartIdleReaper(ctx)

	bot, err := NewBot(cfg.Matrix, pool)
	if err != nil {
		slog.Error("failed to create bot", "error", err)

		return 1
	}

	hb := NewHeartbeatScheduler(pool, cfg.Pi, cfg.Heartbeat, bot.SendToRoom)
	hb.Start(ctx)

	triggerMgr := NewTriggerPipeManager(pool, cfg.Pi, defaultTriggerPrompt, bot.SendToRoom)
	triggerMgr.Start(ctx)
	bot.SetTriggerPipeManager(triggerMgr)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
		pool.StopAll()
		bot.Stop()
	}()

	slog.Info("opencrow starting")

	if err := bot.Run(ctx, cfg.Matrix); err != nil {
		if ctx.Err() != nil {
			slog.Info("shutdown complete")
		} else {
			slog.Error("bot exited with error", "error", err)

			return 1
		}
	}

	if err := bot.Close(); err != nil {
		slog.Error("failed to close bot", "error", err)
	}

	return 0
}

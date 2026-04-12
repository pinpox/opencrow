package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pinpox/opencrow/backend"
	"github.com/pinpox/opencrow/matrix"
	nostrbackend "github.com/pinpox/opencrow/nostr"
	signalbackend "github.com/pinpox/opencrow/signal"
	// Register the pure-Go SQLite driver.
	_ "modernc.org/sqlite"
)

const (
	opencrowDBFile     = "opencrow.db"
	legacyOutboxDBFile = "sent_messages.db"
)

//go:embed sqlc/schema.sql
var dbSchema string

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Fprintln(os.Stdout, versionString())
		os.Exit(0)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(os.Getenv("OPENCROW_LOG_LEVEL")),
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
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

	if err := os.MkdirAll(cfg.Pi.SessionDir, 0o750); err != nil {
		slog.Error("failed to create session directory", "error", err)

		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := openDB(ctx, cfg.Pi.SessionDir)
	if err != nil {
		slog.Error("failed to open database", "error", err)

		return 1
	}
	defer db.Close()

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		slog.Error("failed to initialize inbox", "error", err)

		return 1
	}

	b, worker, err := wireServices(ctx, cfg, db, inbox)
	if err != nil {
		slog.Error("failed to initialize services", "error", err)

		return 1
	}

	setupShutdown(b, cancel)

	workerDone := spawnWorker(ctx, worker)

	slog.Info("opencrow starting")

	exitCode := 0

	if err := b.Run(ctx); err != nil {
		if ctx.Err() == nil {
			slog.Error("backend exited with error", "error", err)

			exitCode = 1
		} else {
			slog.Info("shutdown complete")
		}
	}

	// Backend may have returned without a signal (error path); ensure
	// the worker sees ctx.Done so the join below cannot hang.
	cancel()
	<-workerDone

	_ = b.Close()

	return exitCode
}

// sqliteDSNParams are the connection parameters for modernc.org/sqlite.
// Note: modernc uses _pragma=name(value), not the mattn/go-sqlite3
// shorthand _name=value — the latter is silently ignored, leaving
// busy_timeout=0 and journal_mode=delete, which causes SQLITE_BUSY
// on concurrent writes. _txlock=immediate prevents deferred-lock
// upgrade deadlocks when two writers overlap under WAL.
const sqliteDSNParams = "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

// openDB opens the shared database for inbox and outbox tables.
func openDB(ctx context.Context, sessionDir string) (*sql.DB, error) {
	dbPath := filepath.Join(sessionDir, opencrowDBFile)

	db, err := sql.Open("sqlite", dbPath+sqliteDSNParams)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if _, err := db.ExecContext(ctx, dbSchema); err != nil {
		db.Close()

		return nil, fmt.Errorf("migrating schema: %w", err)
	}

	if err := migrateLegacyOutbox(ctx, db, sessionDir); err != nil {
		slog.Warn("failed to migrate legacy sent_messages.db", "error", err)
	}

	return db, nil
}

func migrateLegacyOutbox(ctx context.Context, db *sql.DB, sessionDir string) error {
	legacyPath := filepath.Join(sessionDir, legacyOutboxDBFile)

	if _, err := os.Stat(legacyPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("checking legacy db: %w", err)
	}

	slog.Info("migrating legacy sent_messages.db into opencrow.db")

	if _, err := db.ExecContext(ctx, "ATTACH DATABASE ? AS legacy", legacyPath); err != nil {
		return fmt.Errorf("attaching legacy db: %w", err)
	}

	defer db.ExecContext(ctx, "DETACH DATABASE legacy") //nolint:errcheck // best-effort detach

	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO sent_messages (conversation_id, message_id, text)
		SELECT conversation_id, message_id, text FROM legacy.sent_messages
	`); err != nil {
		return fmt.Errorf("copying legacy rows: %w", err)
	}

	if err := os.Remove(legacyPath); err != nil {
		return fmt.Errorf("removing legacy db: %w", err)
	}

	_ = os.Remove(legacyPath + "-wal")
	_ = os.Remove(legacyPath + "-shm")

	slog.Info("legacy sent_messages.db migrated and removed")

	return nil
}

// wireServices creates backend, app, and worker using two-phase init.
func wireServices(ctx context.Context, cfg *Config, db *sql.DB, inbox *InboxStore) (backend.Backend, *Worker, error) { //nolint:ireturn // factory returns interface by design
	// Phase 1: create objects with nil cross-references.
	worker := NewWorker(inbox, cfg.Pi, cfg.Heartbeat.Prompt, defaultTriggerPrompt)

	var app *App

	b, err := createBackend(ctx, cfg,
		func(ctx context.Context, msg backend.Message) { app.HandleMessage(ctx, msg) },
		func(_ string) { worker.Restart() },
	)
	if err != nil {
		return nil, nil, err
	}

	// Phase 2: wire cross-references.
	app = NewApp(b, worker, inbox, db)
	worker.SetApp(app)
	worker.SetBackend(b)

	worker.piCfg.SystemPrompt = app.systemPrompt(worker.piCfg.SystemPrompt)

	if cfg.Heartbeat.Interval > 0 {
		worker.piCfg.SystemPrompt += "\n\n" + heartbeatSoul
	}

	// Start background services.
	startHeartbeat(ctx, worker, cfg.Heartbeat)
	startTriggerPipe(ctx, worker, cfg.Pi.SessionDir)
	worker.StartIdleReaper(ctx)

	return b, worker, nil
}

func createBackend(ctx context.Context, cfg *Config, handler backend.MessageHandler, onRoomCleanup func(string)) (backend.Backend, error) { //nolint:ireturn // factory returns interface by design
	switch cfg.BackendType {
	case backendMatrix:
		return createMatrixBackend(cfg, handler, onRoomCleanup)
	case backendNostr:
		return createNostrBackend(ctx, cfg, handler)
	case backendSignal:
		return createSignalBackend(cfg, handler)
	default:
		return nil, fmt.Errorf("unsupported backend type: %q", cfg.BackendType)
	}
}

// spawnWorker runs the worker loop in a goroutine and returns a channel
// that closes when it exits. main must join on this before returning:
// Worker.Run is the only path that calls stopPi on shutdown, and
// os.Exit otherwise races it, leaving pi (plus tool subprocesses)
// running. Pdeathsig in StartPi is the backstop, but graceful SIGTERM
// via stopPi is the intended path.
func spawnWorker(ctx context.Context, w *Worker) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		w.Run(ctx)
		close(done)
	}()

	return done
}

func setupShutdown(b backend.Backend, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		b.Stop()
		cancel()
	}()
}

func createMatrixBackend(cfg *Config, handler backend.MessageHandler, onRoomCleanup func(string)) (*matrix.Backend, error) {
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

	b.SetRoomCleanupCallback(onRoomCleanup)

	return b, nil
}

func createNostrBackend(ctx context.Context, cfg *Config, handler backend.MessageHandler) (*nostrbackend.Backend, error) {
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

	b, err := nostrbackend.NewBackend(ctx, nostrCfg, handler)
	if err != nil {
		return nil, fmt.Errorf("creating nostr backend: %w", err)
	}

	return b, nil
}

func createSignalBackend(cfg *Config, handler backend.MessageHandler) (*signalbackend.Backend, error) {
	signalCfg := signalbackend.Config{
		BinaryPath:   cfg.Signal.BinaryPath,
		Account:      cfg.Signal.Account,
		ConfigDir:    cfg.Signal.ConfigDir,
		SocketPath:   cfg.Signal.SocketPath,
		AllowedUsers: cfg.Signal.AllowedUsers,
	}

	b, err := signalbackend.New(signalCfg, handler)
	if err != nil {
		return nil, fmt.Errorf("creating signal backend: %w", err)
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

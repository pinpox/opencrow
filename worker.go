package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Source names for inbox items.
const (
	sourceUser      = "user"
	sourceTrigger   = "trigger"
	sourceHeartbeat = "heartbeat"
	sourceCompact   = "compact"
)

// Worker owns the single pi process and drains the inbox in priority order.
// There is exactly one worker per opencrow instance.
type Worker struct {
	inbox   *InboxStore
	piCfg   PiConfig
	pi      *PiProcess
	app     *App
	be      Backend
	lastUse time.Time

	roomID string // resolved lazily from .room_id file

	// config
	hbPrompt      string
	triggerPrompt string

	// mu protects fields accessed from multiple goroutines:
	// pi, roomID, lastUse, compactResult, currentPriority, currentCancel.
	mu              sync.Mutex
	currentPriority int64
	currentCancel   context.CancelFunc

	// wake is signalled (non-blocking) on every Notify call so the
	// worker can poll the DB for the highest-priority item.
	wake chan struct{}

	// compactResult is set before enqueueing a compact item, read by processCompact.
	compactResult chan compactOutcome
}

// compactOutcome carries the result of a compact operation back to the caller.
type compactOutcome struct {
	result *CompactResult
	err    error
}

// Backend is the subset of backend.Backend the worker needs.
type Backend interface {
	SetTyping(ctx context.Context, conversationID string, typing bool)
	SendMessage(ctx context.Context, conversationID string, text string, replyToID string) string
}

// NewWorker creates a new worker. The pi process is started lazily on first dequeue.
// app and be are set after construction via SetApp/SetBackend (two-phase init).
func NewWorker(inbox *InboxStore, piCfg PiConfig, hbCfg HeartbeatConfig, triggerPrompt string) *Worker {
	return &Worker{
		inbox:           inbox,
		piCfg:           piCfg,
		hbPrompt:        hbCfg.Prompt,
		triggerPrompt:   triggerPrompt,
		lastUse:         time.Now(),
		currentPriority: -1,
		wake:            make(chan struct{}, 1),
	}
}

// SetApp wires the app reference (phase 2 of init).
func (w *Worker) SetApp(app *App) { w.app = app }

// SetBackend wires the backend reference (phase 2 of init).
func (w *Worker) SetBackend(be Backend) { w.be = be }

// Notify wakes the worker loop. Called after enqueueing an item.
// If the new item has strictly higher priority than the running one,
// the running operation is preempted.
func (w *Worker) Notify(priority int64) {
	w.mu.Lock()
	if w.currentCancel != nil && priority < w.currentPriority {
		slog.Info("worker: preempting current operation",
			"new_priority", priority,
			"current_priority", w.currentPriority,
		)
		w.currentCancel()
	}
	w.mu.Unlock()

	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run is the main worker loop. It blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	slog.Info("worker: started")

	w.drainOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			w.stopPi()
			slog.Info("worker: stopped")

			return
		case <-w.wake:
			w.drainOnce(ctx)
		}
	}
}

// Abort cancels the currently running operation, if any.
func (w *Worker) Abort() bool {
	w.mu.Lock()
	cancel := w.currentCancel
	w.mu.Unlock()

	if cancel != nil {
		cancel()

		return true
	}

	return false
}

// IsActive returns true if a pi process is alive.
func (w *Worker) IsActive() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.pi != nil && w.pi.IsAlive()
}

// Restart kills the current pi process. The next inbox item starts a fresh one.
func (w *Worker) Restart() { w.stopPi() }

// Compact enqueues a compact operation and waits for the result.
func (w *Worker) Compact(ctx context.Context) (*CompactResult, error) {
	if !w.IsActive() {
		return nil, errors.New("no active session")
	}

	w.mu.Lock()
	if w.compactResult != nil {
		w.mu.Unlock()

		return nil, errors.New("compact already in progress")
	}

	ch := make(chan compactOutcome, 1)
	w.compactResult = ch
	w.mu.Unlock()

	if err := w.inbox.Enqueue(ctx, PriorityUser, sourceCompact, "", ""); err != nil {
		w.mu.Lock()
		w.compactResult = nil
		w.mu.Unlock()

		return nil, fmt.Errorf("enqueuing compact: %w", err)
	}

	w.Notify(PriorityUser)

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("compact cancelled: %w", ctx.Err())
	case outcome := <-ch:
		return outcome.result, outcome.err
	}
}

// SetRoomID sets the room ID for this worker.
func (w *Worker) SetRoomID(roomID string) {
	w.mu.Lock()
	w.roomID = roomID
	w.mu.Unlock()
}

// SkillsSummary returns a formatted list of loaded skill paths.
func (w *Worker) SkillsSummary() string {
	skills := w.piCfg.Skills
	if len(skills) == 0 {
		return "No skills loaded."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d skill(s) loaded:\n", len(skills)))

	for _, s := range skills {
		sb.WriteString(fmt.Sprintf("- %s\n", filepath.Base(s)))
	}

	return sb.String()
}

// StartIdleReaper kills the pi process after the configured idle timeout.
func (w *Worker) StartIdleReaper(ctx context.Context) {
	if w.piCfg.IdleTimeout <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.mu.Lock()
				processing := w.currentCancel != nil
				idle := !processing && w.pi != nil && w.pi.IsAlive() && time.Since(w.lastUse) > w.piCfg.IdleTimeout
				w.mu.Unlock()

				if idle {
					slog.Info("worker: reaping idle pi process")
					w.stopPi()
				}
			}
		}
	}()
}

func (w *Worker) drainOnce(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		item, err := w.inbox.Dequeue(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}

		if err != nil {
			if ctx.Err() == nil {
				slog.Error("worker: dequeue failed", "error", err)
			}

			return
		}

		w.processItem(ctx, item)
	}
}

func (w *Worker) processItem(ctx context.Context, item Inbox) {
	slog.Info("worker: processing", "source", item.Source, "priority", item.Priority, "id", item.ID)

	itemCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.mu.Lock()
	w.currentPriority = item.Priority
	w.currentCancel = cancel
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.currentPriority = -1
		w.currentCancel = nil
		w.mu.Unlock()
	}()

	if item.Source == sourceCompact {
		w.processCompact(itemCtx)

		return
	}

	w.processPrompt(itemCtx, item)
}

func (w *Worker) processPrompt(ctx context.Context, item Inbox) {
	prompt, ok := w.buildPrompt(item)
	if !ok {
		return
	}

	convID := w.resolveRoomID()
	if convID == "" {
		w.handleNoRoomID(item) //nolint:contextcheck // requeue uses context.Background intentionally

		return
	}

	w.be.SetTyping(ctx, convID, true)
	defer w.be.SetTyping(context.Background(), convID, false) //nolint:contextcheck // must clear typing even after preemption

	pi, err := w.ensurePi(ctx)
	if err != nil {
		slog.Error("worker: failed to start pi", "source", item.Source, "error", err)

		if item.Source == sourceUser {
			w.be.SendMessage(ctx, convID, fmt.Sprintf("Error starting AI backend: %v", err), "")
		}

		return
	}

	reply, err := pi.sendAndWait(ctx, prompt)
	if err != nil {
		w.handlePromptError(ctx, item, convID, err)

		return
	}

	w.mu.Lock()
	w.lastUse = time.Now()
	w.mu.Unlock()

	if item.Source == sourceUser && reply == "" {
		reply = w.retryEmptyResponse(ctx, pi)
	}

	if shouldSuppressReply(reply, item.Source) {
		return
	}

	w.app.sendReplyWithFiles(ctx, convID, reply, item.ReplyTo)
}

func (w *Worker) buildPrompt(item Inbox) (string, bool) {
	switch item.Source {
	case sourceUser:
		return item.Content, true
	case sourceTrigger:
		return buildTriggerPrompt(w.triggerPrompt, item.Content), true
	case sourceHeartbeat:
		content := w.readHeartbeatContent()
		if isEffectivelyEmpty(content) {
			return "", false
		}

		return buildHeartbeatPrompt(w.hbPrompt, content), true
	default:
		return "", false
	}
}

func (w *Worker) handleNoRoomID(item Inbox) {
	if item.Source == sourceTrigger {
		if err := w.inbox.Requeue(context.Background(), item); err != nil {
			slog.Error("worker: failed to requeue trigger (item lost)", "error", err)
		} else {
			slog.Info("worker: no room ID for trigger, requeued")
		}

		return
	}

	slog.Error("worker: no room ID available, dropping item", "source", item.Source)
}

func (w *Worker) handlePromptError(ctx context.Context, item Inbox, convID string, err error) {
	if wasPreempted(ctx, err) {
		slog.Info("worker: preempted", "source", item.Source)

		if item.Source != sourceHeartbeat {
			if err := w.inbox.Requeue(context.Background(), item); err != nil { //nolint:contextcheck // item ctx is cancelled; need a live ctx to persist
				slog.Error("worker: failed to requeue after preemption (item lost)", "source", item.Source, "error", err)
			}
		}

		return
	}

	slog.Error("worker: pi prompt failed", "source", item.Source, "error", err)
	w.stopPi()

	if item.Source == sourceUser {
		w.be.SendMessage(ctx, convID, fmt.Sprintf("Error: %v", err), "")
	}
}

func (w *Worker) processCompact(ctx context.Context) {
	w.mu.Lock()
	ch := w.compactResult
	w.compactResult = nil
	w.mu.Unlock()

	if ch == nil {
		slog.Warn("worker: compact item but no result channel")

		return
	}

	w.mu.Lock()
	pi := w.pi
	w.mu.Unlock()

	if pi == nil || !pi.IsAlive() {
		ch <- compactOutcome{err: errors.New("no active session")}

		return
	}

	result, err := pi.Compact(ctx)
	ch <- compactOutcome{result: result, err: err}
}

func wasPreempted(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		ctx.Err() != nil
}

func (w *Worker) ensurePi(ctx context.Context) (*PiProcess, error) {
	w.mu.Lock()
	if w.pi != nil && w.pi.IsAlive() {
		pi := w.pi
		w.mu.Unlock()

		return pi, nil
	}
	w.mu.Unlock()

	roomID := w.resolveRoomID()

	pi, err := StartPi(ctx, w.piCfg, roomID)
	if err != nil {
		return nil, err
	}

	if w.piCfg.ShowToolCalls {
		pi.onToolCall = func(evt ToolCallEvent) { //nolint:contextcheck // fire-and-forget notification, no parent ctx
			w.be.SendMessage(context.Background(), w.resolveRoomID(), formatToolCall(evt), "")
		}
	}

	w.mu.Lock()
	w.pi = pi
	w.mu.Unlock()

	return pi, nil
}

func (w *Worker) stopPi() {
	w.mu.Lock()
	pi := w.pi
	w.pi = nil
	w.mu.Unlock()

	if pi != nil {
		slog.Info("worker: stopping pi process")
		pi.Kill()
	}
}

func (w *Worker) resolveRoomID() string {
	w.mu.Lock()
	if w.roomID != "" {
		id := w.roomID
		w.mu.Unlock()

		return id
	}
	w.mu.Unlock()

	path := filepath.Join(w.piCfg.SessionDir, ".room_id")

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	id := strings.TrimSpace(string(data))

	w.mu.Lock()
	w.roomID = id
	w.mu.Unlock()

	return id
}

func (w *Worker) readHeartbeatContent() string {
	heartbeatPath := filepath.Join(w.piCfg.SessionDir, "HEARTBEAT.md")

	heartbeatContent, err := os.ReadFile(heartbeatPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("worker: failed to read HEARTBEAT.md", "path", heartbeatPath, "error", err)
	}

	return strings.TrimSpace(string(heartbeatContent))
}

func (w *Worker) retryEmptyResponse(ctx context.Context, pi *PiProcess) string {
	slog.Warn("worker: empty response, re-prompting for summary")

	reply, err := pi.sendAndWait(ctx, "You just completed a task but your response contained no text for the user. Please briefly summarize what you did or respond to the user's message.")
	if err != nil {
		slog.Error("worker: re-prompt failed", "error", err)

		return "(I completed some actions but failed to generate a summary.)"
	}

	if reply == "" {
		return "(empty response)"
	}

	return reply
}

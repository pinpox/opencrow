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
	"sync/atomic"
	"time"

	"github.com/pinpox/opencrow/backend"
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
	inbox *InboxStore
	piCfg PiConfig
	app   *App
	be    Backend

	roomID atomic.Value // string, resolved lazily from .room_id file

	// config
	hbPrompt      string
	triggerPrompt string

	// mu protects pi, lastUse, compactResult, currentPriority, currentCancel, freshStart.
	mu              sync.Mutex
	pi              *PiProcess
	freshStart      bool // next ensurePi spawns without --continue
	lastUse         time.Time
	currentPriority int64
	currentCancel   context.CancelFunc
	compactResult   chan compactOutcome

	// wake is signalled (non-blocking) on every Notify call so the
	// worker can poll the DB for the highest-priority item.
	wake chan struct{}
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
	MarkdownFlavor() backend.MarkdownFlavor
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

// Restart kills the current pi process and marks the next spawn to
// skip --continue. Without the flag, the next ensurePi would resume
// the same on-disk session (pi persists sessions as jsonl files that
// --continue picks up), so a user stuck at a 429 context-limit wall
// would restart straight back into it.
func (w *Worker) Restart() {
	w.mu.Lock()
	w.freshStart = true
	w.mu.Unlock()

	w.stopPi()
}

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
	w.roomID.Store(roomID)
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

		if item.Source == sourceUser {
			item = w.mergeUserItems(ctx, item)
		}

		if w.processItem(ctx, item) {
			return
		}
	}
}

// mergeUserItems folds any additional queued user messages into item so
// the agent sees one combined prompt instead of N separate turns.
func (w *Worker) mergeUserItems(ctx context.Context, item Inbox) Inbox {
	extra, err := w.inbox.DequeueUserBatch(ctx)
	if err != nil {
		slog.Error("worker: failed to dequeue user batch", "error", err)

		return item
	}

	if len(extra) == 0 {
		return item
	}

	slog.Info("worker: merging user messages", "count", 1+len(extra))

	var sb strings.Builder

	sb.WriteString(item.Content)

	for _, e := range extra {
		sb.WriteByte('\n')
		sb.WriteString(e.Content)
	}

	item.Content = sb.String()
	item.ReplyTo = extra[len(extra)-1].ReplyTo

	return item
}

// processItem handles one inbox item. Returns true if drainOnce should
// stop looping (e.g. an item was requeued and should wait for a future
// Notify rather than spinning).
func (w *Worker) processItem(ctx context.Context, item Inbox) bool {
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

		return false
	}

	return w.processPrompt(itemCtx, item)
}

// processPrompt handles a user/trigger/heartbeat item. Returns true if
// the caller should stop draining (item was requeued).
func (w *Worker) processPrompt(ctx context.Context, item Inbox) bool {
	prompt, ok := w.buildPrompt(item)
	if !ok {
		return false
	}

	convID := w.resolveRoomID()
	if convID == "" {
		return w.handleNoRoomID(item) //nolint:contextcheck // requeue uses context.Background intentionally
	}

	w.be.SetTyping(ctx, convID, true)
	defer w.be.SetTyping(context.Background(), convID, false) //nolint:contextcheck // must clear typing even after preemption

	taskStart := time.Now()

	pi, reply, err := w.sendWithRetry(ctx, prompt)
	if err != nil {
		killPi := pi != nil

		return w.handlePiError(ctx, item, convID, "pi prompt failed", err, killPi)
	}

	w.mu.Lock()
	w.lastUse = time.Now()
	w.mu.Unlock()

	if item.Source == sourceUser && reply == "" {
		reply = w.retryEmptyResponse(ctx, pi)
	}

	if shouldSuppressReply(reply, item.Source) {
		return false
	}

	if w.piCfg.DebugTiming {
		reply += fmt.Sprintf("\n\n⏱ %s", time.Since(taskStart).Round(time.Millisecond))
	}

	w.app.sendReplyWithFiles(ctx, convID, reply, item.ReplyTo)

	return false
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

// handleNoRoomID requeues triggers (room may appear later) and drops
// everything else. Returns true if the item was requeued, signalling
// drainOnce to stop looping and wait for the next Notify.
func (w *Worker) handleNoRoomID(item Inbox) bool {
	if item.Source == sourceTrigger {
		if err := w.inbox.Requeue(context.Background(), item); err != nil {
			slog.Error("worker: failed to requeue trigger (item lost)", "error", err)
		} else {
			slog.Info("worker: no room ID for trigger, requeued")
		}

		return true
	}

	slog.Error("worker: no room ID available, dropping item", "source", item.Source)

	return false
}

// handlePiError handles errors from ensurePi or sendAndWait. Preempted
// items are requeued; terminal failures kill the process and notify the
// user. Returns true if drainOnce should stop.
func (w *Worker) handlePiError(ctx context.Context, item Inbox, convID, label string, err error, killPi bool) bool {
	if wasPreempted(ctx, err) {
		return w.requeuePreempted(item) //nolint:contextcheck // item ctx is cancelled; requeue uses background ctx
	}

	slog.Error("worker: "+label, "source", item.Source, "error", err)

	if killPi {
		w.stopPi()
	}

	if item.Source == sourceUser {
		w.be.SendMessage(ctx, convID, fmt.Sprintf("Error: %v", err), "")
	}

	return false
}

// requeuePreempted re-inserts a preempted item (except heartbeats).
// Returns true so drainOnce stops and re-enters via the new Notify.
func (w *Worker) requeuePreempted(item Inbox) bool {
	slog.Info("worker: preempted", "source", item.Source)

	if item.Source == sourceHeartbeat {
		return true
	}

	if err := w.inbox.Requeue(context.Background(), item); err != nil {
		slog.Error("worker: failed to requeue after preemption (item lost)", "source", item.Source, "error", err)
	}

	return true
}

func (w *Worker) processCompact(ctx context.Context) {
	w.mu.Lock()
	ch := w.compactResult
	w.compactResult = nil
	pi := w.pi
	w.mu.Unlock()

	if ch == nil {
		slog.Warn("worker: compact item but no result channel")

		return
	}

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

// sendWithRetry sends a prompt to pi, retrying once with a fresh
// process if the first attempt fails due to a stale/crashed process.
func (w *Worker) sendWithRetry(ctx context.Context, prompt string) (*PiProcess, string, error) {
	pi, err := w.ensurePi(ctx)
	if err != nil {
		return nil, "", err
	}

	reply, err := pi.sendAndWait(ctx, prompt)
	if err == nil {
		return pi, reply, nil
	}

	// Don't retry on context cancellation (preemption/shutdown).
	if ctx.Err() != nil {
		return pi, "", err
	}

	slog.Info("worker: pi exited, starting fresh process")
	w.stopPi()

	pi, err = w.ensurePi(ctx)
	if err != nil {
		return nil, "", err
	}

	reply, err = pi.sendAndWait(ctx, prompt)

	return pi, reply, err
}

func (w *Worker) ensurePi(ctx context.Context) (*PiProcess, error) {
	w.mu.Lock()
	if w.pi != nil && w.pi.IsAlive() {
		pi := w.pi
		w.mu.Unlock()

		return pi, nil
	}

	fresh := w.freshStart
	w.freshStart = false

	w.mu.Unlock()

	roomID := w.resolveRoomID()

	pi, err := StartPi(ctx, w.piCfg, roomID, fresh)
	if err != nil {
		return nil, err
	}

	if w.piCfg.ShowToolCalls {
		flavor := w.be.MarkdownFlavor()
		pi.onToolCall = func(evt ToolCallEvent) { //nolint:contextcheck // fire-and-forget notification, no parent ctx
			w.be.SendMessage(context.Background(), w.resolveRoomID(), formatToolCall(evt, flavor), "")
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
	if id, ok := w.roomID.Load().(string); ok && id != "" {
		return id
	}

	path := filepath.Join(w.piCfg.SessionDir, ".room_id")

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	id := strings.TrimSpace(string(data))
	w.roomID.Store(id)

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

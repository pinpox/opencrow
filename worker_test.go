package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinpox/opencrow/backend"
)

type stubBackend struct{}

func (stubBackend) SetTyping(context.Context, string, bool)                    {}
func (stubBackend) SendMessage(context.Context, string, string, string) string { return "" }
func (stubBackend) MarkdownFlavor() backend.MarkdownFlavor                     { return backend.MarkdownNone }

// newFakePiWorker builds a Worker wired to the bash fake-pi stub.
// Cleanup stops the spawned process.
func newFakePiWorker(t *testing.T) *Worker {
	t.Helper()

	dir := t.TempDir()

	script, err := filepath.Abs("testdata/fake-pi")
	if err != nil {
		t.Fatal(err)
	}

	w := NewWorker(newTestInbox(t.Context(), t), PiConfig{
		BinaryPath: "bash",
		BinaryArgs: []string{script},
		SessionDir: dir,
		WorkingDir: dir,
	}, "", "")
	w.SetBackend(stubBackend{})
	w.SetRoomID("room")

	t.Cleanup(w.stopPi)

	return w
}

// Regression for the "No active session to compact" bug seen on eve:
// pi was spawned with exec.CommandContext bound to the per-item ctx,
// which is cancelled the moment processItem returns. Go's CommandContext
// then SIGKILLs pi, so by the time the next message (or !compact)
// arrives the worker reports IsActive() == false. Journald showed
// "pi: process exited" immediately after every "agent finished".
//
// The pi process lifetime is owned by the worker (idle reaper / Restart
// / Run shutdown via stopPi), not by an individual prompt's context.
func TestWorker_PiSurvivesItemContext(t *testing.T) {
	t.Parallel()

	w := newFakePiWorker(t)

	// Mirror processItem: a per-item ctx that is cancelled as soon as
	// the prompt completes.
	itemCtx, cancel := context.WithCancel(context.Background())

	pi, reply, err := w.sendWithRetry(itemCtx, "hello")
	if err != nil || reply != "ok" {
		t.Fatalf("sendWithRetry = (%q, %v), want (ok, nil)", reply, err)
	}

	cancel() // processItem's defer cancel()

	// CommandContext kills via a watcher goroutine after <-ctx.Done(),
	// and IsAlive() reads p.done which a second goroutine closes after
	// cmd.Wait(). Neither has run immediately after cancel() returns,
	// so wait briefly for the bug to manifest if it's going to.
	select {
	case <-pi.done:
		t.Fatal("pi process died after item ctx cancel; it must outlive individual prompts")
	case <-time.After(500 * time.Millisecond):
	}

	// A follow-up prompt must reuse the existing process, not respawn.
	// On eve the dead process triggered sendWithRetry's "pi exited,
	// starting fresh process" path on every message.
	pi2, _, err := w.sendWithRetry(t.Context(), "again")
	if err != nil {
		t.Fatalf("second sendWithRetry: %v", err)
	}

	if pi2 != pi {
		t.Fatal("second prompt spawned a new pi process; expected reuse")
	}

	// And the user-facing symptom: !compact must find a live session.
	cr, err := pi2.Compact(t.Context())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if cr.Summary != "s" || cr.TokensBefore != 1 {
		t.Fatalf("compact result = %+v", cr)
	}
}

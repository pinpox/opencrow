package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// newFakePiProcess creates a PiProcess backed by a long-running subprocess
// whose stdin/stdout we control via pipes. The returned writer sends
// JSON lines that the PiProcess reads as RPC events.
func newFakePiProcess(t *testing.T) (*PiProcess, *os.File) {
	t.Helper()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	// Start a real process so cmd.Process is non-nil and Kill() works.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, "sleep", "60")
	cmd.Stdin = stdinR

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	stdinR.Close()

	done := make(chan struct{})

	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, scannerBufSize), scannerBufSize)

	p := &PiProcess{
		cmd:     cmd,
		stdin:   stdinW,
		stdout:  scanner,
		done:    done,
		lastUse: time.Now(),
	}

	t.Cleanup(func() {
		p.Kill()
		stdoutW.Close()
		stdoutR.Close()
	})

	return p, stdoutW
}

func writeRPCEvent(t *testing.T, w *os.File, evt any) {
	t.Helper()

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshaling RPC event: %v", err)
	}

	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		t.Fatalf("writing RPC event: %v", err)
	}
}

func setupTriggerTest(t *testing.T) (*PiProcess, *os.File, *PiPool, *TriggerPipeManager, string) {
	t.Helper()

	pi, stdoutW := newFakePiProcess(t)

	cfg := PiConfig{
		SessionDir: t.TempDir(),
		BinaryPath: "sleep",
	}
	pool := NewPiPool(cfg)

	roomID := "!testroom"

	pool.mu.Lock()
	pool.processes[roomID] = pi
	pool.mu.Unlock()

	sendReply := func(_ context.Context, _ string, _ string) {}
	setTyping := func(_ context.Context, _ string, _ bool) {}

	triggerMgr := NewTriggerPipeManager(pool, cfg, "trigger prompt",
		sendReply, setTyping,
	)

	return pi, stdoutW, pool, triggerMgr, roomID
}

// TestTriggerAbortDoesNotKillProcess reproduces the scenario where a user
// message aborts a running trigger, and the trigger's error handler
// must not kill the pi process — otherwise the user's prompt writes to
// a closed stdin pipe.
func TestTriggerAbortDoesNotKillProcess(t *testing.T) {
	t.Parallel()

	pi, stdoutW, pool, triggerMgr, roomID := setupTriggerTest(t)

	triggerDone := make(chan struct{})

	go func() {
		defer close(triggerDone)
		triggerMgr.processTrigger(context.Background(), roomID, "calendar update")
	}()

	// Wait for the trigger to acquire the lock and block on reading stdout.
	time.Sleep(50 * time.Millisecond)

	// User message arrives — Prompt() will TryLock, fail, Abort the trigger,
	// then Lock and try to send a prompt.
	userDone := make(chan error, 1)

	go func() {
		_, err := pi.Prompt(context.Background(), "user says no")
		userDone <- err
	}()

	time.Sleep(50 * time.Millisecond)

	// Send agent_end for the aborted trigger so the read goroutine unblocks.
	writeRPCEvent(t, stdoutW, map[string]any{
		"type": "agent_end",
		"messages": []map[string]any{
			{"role": "assistant", "content": "trigger response"},
		},
	})

	select {
	case <-triggerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("trigger did not finish in time")
	}

	if !pi.IsAlive() {
		t.Fatal("pi process was killed by trigger error handler; user prompt will fail with broken pipe")
	}

	// Send agent_end for the user's prompt.
	writeRPCEvent(t, stdoutW, map[string]any{
		"type": "agent_end",
		"messages": []map[string]any{
			{"role": "assistant", "content": "thumbs up"},
		},
	})

	select {
	case err := <-userDone:
		if err != nil {
			t.Fatalf("user prompt failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("user prompt did not finish in time")
	}

	if p := pool.GetExisting(roomID); p == nil {
		t.Error("pi process was removed from pool")
	}
}

// TestTriggerRealErrorStillKillsProcess verifies that genuine failures
// still cause the process to be removed from the pool.
func TestTriggerRealErrorStillKillsProcess(t *testing.T) {
	t.Parallel()

	_, stdoutW, pool, triggerMgr, roomID := setupTriggerTest(t)

	// Close stdout to simulate a pi process crash.
	stdoutW.Close()

	triggerMgr.processTrigger(context.Background(), roomID, "some trigger")

	if p := pool.GetExisting(roomID); p != nil {
		t.Error("pi process should have been removed from pool after a real error")
	}
}

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const scannerBufSize = 1 << 20 // 1 MB

// ToolCallEvent contains information about a tool invocation relayed from pi.
type ToolCallEvent struct {
	ToolName string
	Args     map[string]any
}

// PiProcess manages a single pi --mode rpc subprocess.
// The caller (Worker) is responsible for serializing access — only one
// goroutine calls sendAndWait at a time, so no mutex is needed.
//
// A single persistent reader goroutine owns stdout and pushes parsed
// events into the events channel. Command waiters (sendAndWait,
// Compact) consume from this channel until they see their terminal
// event. This avoids scanner races from per-command reader goroutines.
//
// All stdin writes happen in the caller goroutine. The stdout reader
// goroutine never writes to stdin directly; extension UI requests are
// handled by the caller when it processes events.
type PiProcess struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	done       chan struct{}
	events     <-chan rpcParsed    // single persistent reader feeds all waiters
	onToolCall func(ToolCallEvent) // optional callback for tool_execution_start events
}

// StartPi spawns a pi --mode rpc subprocess for the given room.
// fresh=true omits --continue so pi creates a new session file
// instead of resuming the most recent one from SessionDir.
func StartPi(ctx context.Context, cfg PiConfig, roomID string, fresh bool) (*PiProcess, error) {
	if err := os.MkdirAll(cfg.SessionDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}

	// Persist the current room ID so the heartbeat scheduler can identify the room.
	roomIDPath := filepath.Join(cfg.SessionDir, ".room_id")
	if err := os.WriteFile(roomIDPath, []byte(roomID), 0o600); err != nil {
		return nil, fmt.Errorf("writing room ID file: %w", err)
	}

	// Create the trigger FIFO so external processes can write to it immediately.
	if err := ensureFIFO(TriggerPipePath(cfg.SessionDir)); err != nil {
		return nil, fmt.Errorf("creating trigger FIFO: %w", err)
	}

	args := buildPiArgs(cfg, fresh)

	cmd := exec.CommandContext(ctx, cfg.BinaryPath, args...) //nolint:gosec // binary path is from trusted config
	cmd.Dir = cfg.WorkingDir
	cmd.Env = os.Environ()

	return startPiProcess(cmd, cfg.SessionDir)
}

// Kill terminates the pi process.
func (p *PiProcess) Kill() {
	if p.cmd.Process == nil {
		return
	}

	_ = p.stdin.Close()
	_ = p.cmd.Process.Signal(os.Interrupt)

	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
		slog.Warn("pi: process did not exit after SIGINT, sending SIGKILL")

		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// IsAlive returns true if the pi process is still running.
func (p *PiProcess) IsAlive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func startPiProcess(cmd *exec.Cmd, sessionDir string) (*PiProcess, error) {
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()

		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		stdinPipe.Close()
		stdoutPipe.Close()

		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting pi: %w", err)
	}

	slog.Info("pi: process started", "pid", cmd.Process.Pid, "session_dir", sessionDir)

	// Log stderr in background
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			slog.Debug("pi: stderr", "line", scanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, scannerBufSize), scannerBufSize)

	done := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(done)

		slog.Info("pi: process exited")
	}()

	// Start a single persistent reader goroutine that owns the
	// scanner for the lifetime of the process. All command waiters
	// read from this channel, avoiding concurrent scanner access.
	eventCh := make(chan rpcParsed, 4)

	go readEvents(scanner, eventCh)

	return &PiProcess{
		cmd:    cmd,
		stdin:  stdinPipe,
		done:   done,
		events: eventCh,
	}, nil
}

func buildPiArgs(cfg PiConfig, fresh bool) []string {
	args := []string{"--mode", "rpc", "--session-dir", cfg.SessionDir}
	if !fresh {
		args = append(args, "--continue")
	}

	if cfg.Provider != "" {
		args = append(args, "--provider", cfg.Provider)
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	if cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.SystemPrompt)
	}

	for _, skill := range cfg.Skills {
		args = append(args, "--skill", skill)
	}

	return args
}

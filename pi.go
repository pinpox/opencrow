package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const scannerBufSize = 1 << 20 // 1 MB

// PiProcess manages a single pi --mode rpc subprocess.
type PiProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Scanner
	done    chan struct{}
	mu      sync.Mutex
	lastUse time.Time
	roomID  string
}

// StartPi spawns a pi --mode rpc subprocess for the given room.
func StartPi(ctx context.Context, cfg PiConfig, roomID string) (*PiProcess, error) {
	if err := os.MkdirAll(cfg.SessionDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}

	// Persist the current room ID so the heartbeat scheduler can identify the room.
	roomIDPath := filepath.Join(cfg.SessionDir, ".room_id")
	if err := os.WriteFile(roomIDPath, []byte(roomID), 0o644); err != nil {
		return nil, fmt.Errorf("writing room ID file: %w", err)
	}

	// Create the trigger FIFO so external processes can write to it immediately.
	if err := ensureFIFO(TriggerPipePath(cfg.SessionDir)); err != nil {
		return nil, fmt.Errorf("creating trigger FIFO: %w", err)
	}

	args := buildPiArgs(cfg, cfg.SessionDir)

	cmd := exec.CommandContext(ctx, cfg.BinaryPath, args...) //nolint:gosec // binary path is from trusted config
	cmd.Dir = cfg.WorkingDir
	cmd.Env = os.Environ()

	return startPiProcess(cmd, roomID, cfg.SessionDir)
}

func startPiProcess(cmd *exec.Cmd, roomID, sessionDir string) (*PiProcess, error) {
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

	slog.Info("pi process started", "room", roomID, "pid", cmd.Process.Pid, "session_dir", sessionDir)

	// Log stderr in background
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			slog.Debug("pi stderr", "room", roomID, "line", scanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, scannerBufSize), scannerBufSize)

	done := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(done)

		slog.Info("pi process exited", "room", roomID)
	}()

	return &PiProcess{
		cmd:     cmd,
		stdin:   stdinPipe,
		stdout:  scanner,
		done:    done,
		lastUse: time.Now(),
		roomID:  roomID,
	}, nil
}

func buildPiArgs(cfg PiConfig, sessionDir string) []string {
	args := []string{"--mode", "rpc", "--session-dir", sessionDir, "--continue"}

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

// rpcEvent represents a JSON event from pi's stdout.
type rpcEvent struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Command string `json:"command,omitempty"`
	Success *bool  `json:"success,omitempty"`
	Error   string `json:"error,omitempty"`

	// agent_end fields
	Messages json.RawMessage `json:"messages,omitempty"`

	// extension_ui_request fields
	Method string `json:"method,omitempty"`
}

// agentMessage represents a message in an agent_end event.
type agentMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlock represents a content block in an assistant message.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Prompt sends a message to the pi process and waits for the agent to complete.
// Returns the assistant's text response.
func (p *PiProcess) Prompt(ctx context.Context, message string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.lastUse = time.Now()

	if !p.IsAlive() {
		return "", errors.New("pi process is not alive")
	}

	if err := p.sendPromptCommand(message); err != nil {
		return "", err
	}

	return p.waitForResult(ctx)
}

// PromptNoTouch is like Prompt but does not update lastUse.
// Used for heartbeat prompts so idle reaping still works.
func (p *PiProcess) PromptNoTouch(ctx context.Context, message string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.IsAlive() {
		return "", errors.New("pi process is not alive")
	}

	if err := p.sendPromptCommand(message); err != nil {
		return "", err
	}

	return p.waitForResult(ctx)
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
		slog.Warn("pi process did not exit after SIGINT, sending SIGKILL", "room", p.roomID)
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

// LastUse returns the time of the last prompt.
func (p *PiProcess) LastUse() time.Time {
	return p.lastUse
}

func (p *PiProcess) sendPromptCommand(message string) error {
	cmd := map[string]string{
		"type":    "prompt",
		"message": message,
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshaling prompt: %w", err)
	}

	data = append(data, '\n')

	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("writing to pi stdin: %w", err)
	}

	return nil
}

func (p *PiProcess) waitForResult(ctx context.Context) (string, error) {
	type result struct {
		text string
		err  error
	}

	resultCh := make(chan result, 1)

	go func() {
		text, err := p.readUntilAgentEnd()
		resultCh <- result{text, err}
	}()

	select {
	case <-ctx.Done():
		p.sendAbort()

		// Still wait for the read goroutine to finish
		<-resultCh

		return "", fmt.Errorf("context cancelled: %w", ctx.Err())
	case r := <-resultCh:
		return r.text, r.err
	}
}

func (p *PiProcess) sendAbort() {
	abort := map[string]string{"type": "abort"}

	abortData, err := json.Marshal(abort)
	if err != nil {
		slog.Warn("failed to marshal abort command", "room", p.roomID, "error", err)

		return
	}

	abortData = append(abortData, '\n')

	_, _ = p.stdin.Write(abortData)
}

// readUntilAgentEnd reads JSON events from stdout until agent_end is received.
func (p *PiProcess) readUntilAgentEnd() (string, error) {
	for p.stdout.Scan() {
		line := p.stdout.Text()

		if line == "" {
			continue
		}

		var evt rpcEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			slog.Warn("malformed JSON from pi", "room", p.roomID, "error", err, "line", line)

			continue
		}

		slog.Debug("pi rpc event", "room", p.roomID, "type", evt.Type)

		switch evt.Type {
		case "agent_end":
			text := extractLastAssistantText(evt.Messages)
			if text == "" {
				slog.Warn("agent_end contained no assistant text", "room", p.roomID, "messages_len", len(evt.Messages))
			}

			return text, nil

		case "extension_ui_request":
			// Auto-cancel dialog requests
			p.autoRespondExtensionUI(evt)

		case "response":
			// Check for prompt rejection
			if evt.Success != nil && !*evt.Success {
				return "", fmt.Errorf("pi rejected command %q: %s", evt.Command, evt.Error)
			}
		}
	}

	if err := p.stdout.Err(); err != nil {
		return "", fmt.Errorf("reading pi stdout: %w", err)
	}

	return "", errors.New("pi process closed stdout (EOF)")
}

// autoRespondExtensionUI sends a cancellation response for dialog-type extension UI requests.
func (p *PiProcess) autoRespondExtensionUI(evt rpcEvent) {
	switch evt.Method {
	case "select", "confirm", "input", "editor":
		resp := map[string]any{
			"type":      "extension_ui_response",
			"id":        evt.ID,
			"cancelled": true,
		}

		data, err := json.Marshal(resp)
		if err != nil {
			slog.Warn("failed to marshal extension_ui_response", "room", p.roomID, "error", err)

			return
		}

		data = append(data, '\n')

		if _, err := p.stdin.Write(data); err != nil {
			slog.Warn("failed to send extension_ui_response", "room", p.roomID, "error", err)
		}
	}
	// Fire-and-forget methods (notify, setStatus, setWidget, setTitle, set_editor_text) are ignored.
}

// extractLastAssistantText finds the last assistant message in an agent_end event
// and joins its text content blocks.
func extractLastAssistantText(messagesRaw json.RawMessage) string {
	if len(messagesRaw) == 0 {
		return ""
	}

	var messages []agentMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		slog.Warn("failed to parse agent_end messages", "error", err)

		return ""
	}

	// Walk backwards through assistant messages to find one with text content.
	// The last assistant message might be tool-use only (no text), so keep looking.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}

		// Content can be a string or array of content blocks
		var text string
		if err := json.Unmarshal(messages[i].Content, &text); err == nil {
			if text != "" {
				return text
			}

			continue
		}

		var blocks []contentBlock
		if err := json.Unmarshal(messages[i].Content, &blocks); err != nil {
			slog.Warn("failed to parse assistant content blocks", "error", err)

			continue
		}

		var parts []string

		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}

		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	return ""
}


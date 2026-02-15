package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	done   chan struct{}
	mu     sync.Mutex
	lastUse time.Time
	roomID string
}

// StartPi spawns a pi --mode rpc subprocess for the given room.
func StartPi(ctx context.Context, cfg PiConfig, roomID string) (*PiProcess, error) {
	sessionDir := filepath.Join(cfg.SessionDir, sanitizeRoomID(roomID))
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}

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

	cmd := exec.CommandContext(ctx, cfg.BinaryPath, args...)
	cmd.Dir = cfg.WorkingDir
	cmd.Env = os.Environ()

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

// rpcEvent represents a JSON event from pi's stdout.
type rpcEvent struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Command string          `json:"command,omitempty"`
	Success *bool           `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`

	// agent_end fields
	Messages json.RawMessage `json:"messages,omitempty"`

	// extension_ui_request fields
	Method string `json:"method,omitempty"`
}

// agentMessage represents a message in an agent_end event.
type agentMessage struct {
	Role    string               `json:"role"`
	Content json.RawMessage      `json:"content"`
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
		return "", fmt.Errorf("pi process is not alive")
	}

	// Send prompt command
	cmd := map[string]string{
		"type":    "prompt",
		"message": message,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return "", fmt.Errorf("marshaling prompt: %w", err)
	}
	data = append(data, '\n')

	if _, err := p.stdin.Write(data); err != nil {
		return "", fmt.Errorf("writing to pi stdin: %w", err)
	}

	// Read events until agent_end or error
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
		// Send abort
		abort := map[string]string{"type": "abort"}
		abortData, _ := json.Marshal(abort)
		abortData = append(abortData, '\n')
		_, _ = p.stdin.Write(abortData)
		// Still wait for the read goroutine to finish
		r := <-resultCh
		if r.err != nil {
			return "", ctx.Err()
		}
		return "", ctx.Err()
	case r := <-resultCh:
		return r.text, r.err
	}
}

// PromptNoTouch is like Prompt but does not update lastUse.
// Used for heartbeat prompts so idle reaping still works.
func (p *PiProcess) PromptNoTouch(ctx context.Context, message string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.IsAlive() {
		return "", fmt.Errorf("pi process is not alive")
	}

	cmd := map[string]string{
		"type":    "prompt",
		"message": message,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return "", fmt.Errorf("marshaling prompt: %w", err)
	}
	data = append(data, '\n')

	if _, err := p.stdin.Write(data); err != nil {
		return "", fmt.Errorf("writing to pi stdin: %w", err)
	}

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
		abort := map[string]string{"type": "abort"}
		abortData, _ := json.Marshal(abort)
		abortData = append(abortData, '\n')
		_, _ = p.stdin.Write(abortData)
		<-resultCh
		return "", ctx.Err()
	case r := <-resultCh:
		return r.text, r.err
	}
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

		switch evt.Type {
		case "agent_end":
			return extractLastAssistantText(evt.Messages), nil

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
	return "", fmt.Errorf("pi process closed stdout (EOF)")
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
		data, _ := json.Marshal(resp)
		data = append(data, '\n')
		if _, err := p.stdin.Write(data); err != nil {
			slog.Warn("failed to send extension_ui_response", "room", p.roomID, "error", err)
		}
	}
	// Fire-and-forget methods (notify, setStatus, setWidget, setTitle, set_editor_text) are ignored.
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

	// Walk backwards to find the last assistant message
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}

		// Content can be a string or array of content blocks
		var text string
		if err := json.Unmarshal(messages[i].Content, &text); err == nil {
			return text
		}

		var blocks []contentBlock
		if err := json.Unmarshal(messages[i].Content, &blocks); err != nil {
			slog.Warn("failed to parse assistant content blocks", "error", err)
			return ""
		}

		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// sanitizeRoomID converts a Matrix room ID into a filesystem-safe directory name.
// e.g. "!abc123:matrix.org" -> "abc123-matrix.org"
func sanitizeRoomID(roomID string) string {
	s := strings.TrimPrefix(roomID, "!")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, "/", "-")
	return s
}

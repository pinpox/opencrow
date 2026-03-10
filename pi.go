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
	"slices"
	"strings"
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

// rpcParsed is sent from the persistent stdout reader to the caller.
// On normal events err is nil; on scanner errors err is set and event
// is meaningless. EOF is signalled by closing the channel.
type rpcParsed struct {
	event rpcEvent
	err   error
}

// StartPi spawns a pi --mode rpc subprocess for the given room.
func StartPi(ctx context.Context, cfg PiConfig, roomID string) (*PiProcess, error) {
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

	args := buildPiArgs(cfg, cfg.SessionDir)

	cmd := exec.CommandContext(ctx, cfg.BinaryPath, args...) //nolint:gosec // binary path is from trusted config
	cmd.Dir = cfg.WorkingDir
	cmd.Env = os.Environ()

	return startPiProcess(cmd, cfg.SessionDir)
}

// Compact sends a compact command to reduce context token usage.
func (p *PiProcess) Compact(ctx context.Context) (*CompactResult, error) {
	if !p.IsAlive() {
		return nil, errors.New("pi process is not alive")
	}

	if err := p.sendCommand(map[string]string{"type": "compact"}); err != nil {
		return nil, err
	}

	return p.waitForCompactResponse(ctx)
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
		slog.Warn("pi process did not exit after SIGINT, sending SIGKILL")

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

// rpcEvent represents a JSON event from pi's stdout.
type rpcEvent struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Command string `json:"command,omitempty"`
	Success *bool  `json:"success,omitempty"`
	Error   string `json:"error,omitempty"`

	// response data (used by compact, get_session_stats, etc.)
	Data json.RawMessage `json:"data,omitempty"`

	// agent_end fields
	Messages json.RawMessage `json:"messages,omitempty"`

	// extension_ui_request fields
	Method string `json:"method,omitempty"`

	// tool_execution_start fields — camelCase is dictated by the pi protocol.
	ToolName string         `json:"toolName,omitempty"` //nolint:tagliatelle // pi protocol uses camelCase
	Args     map[string]any `json:"args,omitempty"`
}

// CompactResult holds the data returned by a successful compact command.
type CompactResult struct {
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore,omitempty"` //nolint:tagliatelle // pi protocol uses camelCase
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

	slog.Info("pi process started", "pid", cmd.Process.Pid, "session_dir", sessionDir)

	// Log stderr in background
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			slog.Debug("pi stderr", "line", scanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, scannerBufSize), scannerBufSize)

	done := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(done)

		slog.Info("pi process exited")
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

// sendAndWait sends a prompt command and waits for the agent to finish.
// The caller must ensure only one goroutine calls this at a time.
// If ctx is cancelled, an abort command is sent to pi and the response
// is drained before returning.
func (p *PiProcess) sendAndWait(ctx context.Context, message string) (string, error) {
	if !p.IsAlive() {
		return "", errors.New("pi process is not alive")
	}

	if err := p.sendPromptCommand(message); err != nil {
		return "", err
	}

	return p.waitForResult(ctx)
}

func (p *PiProcess) sendCommand(cmd any) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshaling command: %w", err)
	}

	data = append(data, '\n')

	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("writing to pi stdin: %w", err)
	}

	return nil
}

func (p *PiProcess) sendPromptCommand(message string) error {
	return p.sendCommand(map[string]string{
		"type":    "prompt",
		"message": message,
	})
}

func (p *PiProcess) sendAbort() {
	if err := p.sendCommand(map[string]string{"type": "abort"}); err != nil {
		slog.Warn("failed to send abort command", "error", err)
	}
}

// readEvents scans pi's stdout line by line, parses each JSON event,
// and sends it to ch. Started once per process in startPiProcess;
// the goroutine owns the scanner for the process lifetime. Closes ch
// when stdout is closed (EOF) or a scanner error occurs.
func readEvents(scanner *bufio.Scanner, ch chan<- rpcParsed) {
	defer close(ch)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var evt rpcEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			slog.Warn("malformed JSON from pi", "error", err, "line", line)

			continue
		}

		slog.Debug("pi rpc event", "type", evt.Type)

		ch <- rpcParsed{event: evt}
	}

	if err := scanner.Err(); err != nil {
		ch <- rpcParsed{err: fmt.Errorf("reading pi stdout: %w", err)}
	}
}

// drainEvents runs the caller-side event loop: it reads parsed events
// from the persistent reader, handles side effects (extension UI cancel,
// tool call notifications), and calls handleFn for each event.
// handleFn returns true when the desired termination event has been
// seen. On context cancellation an abort is sent; drainEvents
// continues calling handleFn so it can detect the terminal event
// (e.g. agent_end) and return promptly instead of blocking until EOF.
func (p *PiProcess) drainEvents(ctx context.Context, handleFn func(rpcEvent) (bool, error)) error {
	aborted := false

	for parsed := range p.events {
		if parsed.err != nil {
			return parsed.err
		}

		// Check for cancellation before processing.
		if !aborted && ctx.Err() != nil {
			p.sendAbort()

			aborted = true
		}

		// Always process the event even after abort so we detect the
		// terminal event (agent_end / compact response) and return
		// promptly. Without this the loop would block until EOF,
		// hanging when pi stays alive after acknowledging the abort.
		if err := p.handleSideEffects(parsed.event); err != nil {
			return err
		}

		done, err := handleFn(parsed.event)
		if err != nil {
			return err
		}

		if done {
			return nil
		}
	}

	// Channel closed = stdout EOF.
	return errors.New("pi process closed stdout (EOF)")
}

// handleSideEffects processes events that are common to all commands:
// extension UI auto-cancel and tool call notifications.
func (p *PiProcess) handleSideEffects(evt rpcEvent) error {
	switch evt.Type {
	case "extension_ui_request":
		p.autoRespondExtensionUI(evt)

	case "tool_execution_start":
		if p.onToolCall != nil {
			p.onToolCall(ToolCallEvent{
				ToolName: evt.ToolName,
				Args:     evt.Args,
			})
		}

	case "response":
		if evt.Success != nil && !*evt.Success {
			return fmt.Errorf("pi rejected command %q: %s", evt.Command, evt.Error)
		}
	}

	return nil
}

func (p *PiProcess) waitForResult(ctx context.Context) (string, error) {
	var reply string

	err := p.drainEvents(ctx, func(evt rpcEvent) (bool, error) {
		if evt.Type != "agent_end" {
			return false, nil
		}

		reply = extractLastAssistantText(evt.Messages)
		if reply == "" {
			slog.Warn("agent_end contained no assistant text", "messages_len", len(evt.Messages))
		}

		return true, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("context cancelled: %w", ctx.Err())
		}

		return "", err
	}

	return reply, nil
}

func (p *PiProcess) waitForCompactResponse(ctx context.Context) (*CompactResult, error) {
	var result *CompactResult

	err := p.drainEvents(ctx, func(evt rpcEvent) (bool, error) {
		if evt.Type != "response" || evt.Command != "compact" {
			return false, nil
		}

		// Failed responses are already caught by handleSideEffects.
		var cr CompactResult
		if err := json.Unmarshal(evt.Data, &cr); err != nil {
			return false, fmt.Errorf("parsing compact result: %w", err)
		}

		result = &cr

		return true, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context cancelled: %w", ctx.Err())
		}

		return nil, err
	}

	return result, nil
}

func (p *PiProcess) autoRespondExtensionUI(evt rpcEvent) {
	switch evt.Method {
	case "select", "confirm", "input", "editor":
		if err := p.sendCommand(map[string]any{
			"type":      "extension_ui_response",
			"id":        evt.ID,
			"cancelled": true,
		}); err != nil {
			slog.Warn("failed to send extension_ui_response", "error", err)
		}
	}
}

func extractLastAssistantText(messagesRaw json.RawMessage) string {
	if len(messagesRaw) == 0 {
		return ""
	}

	var messages []agentMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		slog.Warn("failed to parse agent_end messages", "error", err)

		return ""
	}

	for _, msg := range slices.Backward(messages) {
		if msg.Role != "assistant" {
			continue
		}

		if text := parseAssistantContent(msg.Content); text != "" {
			return text
		}
	}

	return ""
}

func parseAssistantContent(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}

	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
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

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// rpcParsed is sent from the persistent stdout reader to the caller.
// On normal events err is nil; on scanner errors err is set and event
// is meaningless. EOF is signalled by closing the channel.
type rpcParsed struct {
	event rpcEvent
	err   error
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

	// auto_retry_start fields — camelCase is dictated by the pi protocol.
	Attempt      int    `json:"attempt,omitempty"`
	MaxAttempts  int    `json:"maxAttempts,omitempty"`  //nolint:tagliatelle // pi protocol uses camelCase
	DelayMs      int    `json:"delayMs,omitempty"`      //nolint:tagliatelle // pi protocol uses camelCase
	ErrorMessage string `json:"errorMessage,omitempty"` //nolint:tagliatelle // pi protocol uses camelCase

	// auto_retry_end fields — camelCase is dictated by the pi protocol.
	FinalError string `json:"finalError,omitempty"` //nolint:tagliatelle // pi protocol uses camelCase

	// compaction_start fields
	Reason string `json:"reason,omitempty"`

	// extension_error fields
	ExtensionPath string `json:"extensionPath,omitempty"` //nolint:tagliatelle // pi protocol uses camelCase
	Event         string `json:"event,omitempty"`

	// message_update fields — camelCase is dictated by the pi protocol.
	AssistantMessageEvent *assistantMessageEvent `json:"assistantMessageEvent,omitempty"` //nolint:tagliatelle // pi protocol uses camelCase
}

// assistantMessageEvent is the streaming delta payload inside a message_update event.
type assistantMessageEvent struct {
	Type  string `json:"type"`            // "text_delta", "thinking_delta", "text_start", etc.
	Delta string `json:"delta,omitempty"` // incremental text content
}

// CompactResult holds the data returned by a successful compact command.
type CompactResult struct {
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore,omitempty"` //nolint:tagliatelle // pi protocol uses camelCase
}

// agentMessage represents a message in an agent_end event.
type agentMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason,omitempty"`   //nolint:tagliatelle // pi protocol uses camelCase
	ErrorMessage string          `json:"errorMessage,omitempty"` //nolint:tagliatelle // pi protocol uses camelCase
}

// contentBlock represents a content block in an assistant message.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// RPC event type constants for events referenced in code.
// The pi protocol may add new event types at any time;
// unknown types are handled by default branches.
const (
	rpcTypeResponse            = "response"
	rpcTypeAgentStart          = "agent_start"
	rpcTypeAgentEnd            = "agent_end"
	rpcTypeMessageUpdate       = "message_update"
	rpcTypeToolExecutionStart  = "tool_execution_start"
	rpcTypeToolExecutionEnd    = "tool_execution_end"
	rpcTypeToolExecutionUpdate = "tool_execution_update"
	rpcTypeAutoRetryStart      = "auto_retry_start"
	rpcTypeAutoRetryEnd        = "auto_retry_end"
	rpcTypeExtensionError      = "extension_error"
	rpcTypeExtensionUIRequest  = "extension_ui_request"
)

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

// sendAndWait sends a prompt command and waits for the agent to finish.
// The caller must ensure only one goroutine calls this at a time.
// If ctx is cancelled, an abort command is sent to pi and the response
// is drained before returning.
// onDelta, if non-nil, is called with each text delta during generation.
func (p *PiProcess) sendAndWait(ctx context.Context, message string, onDelta func(string)) (string, error) {
	if !p.IsAlive() {
		return "", errors.New("pi process is not alive")
	}

	if err := p.sendPromptCommand(message); err != nil {
		return "", err
	}

	return p.waitForResult(ctx, onDelta)
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

		logRPCEvent(evt)

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

	for {
		parsed, err := p.nextEvent()
		if err != nil {
			return err
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
}

// nextEvent blocks for the next parsed stdout event or returns an
// error if the process exits first. The select on p.done is the load-
// bearing part: cmd.Wait() racing the stdout reader is a documented
// footgun, and node extensions that fork grandchildren keep the pipe
// FD open past Wait(), so the events channel may never close. Without
// this select, drainEvents hangs forever on a dead process.
func (p *PiProcess) nextEvent() (rpcParsed, error) {
	select {
	case parsed, ok := <-p.events:
		if !ok {
			return rpcParsed{}, errors.New("pi process closed stdout (EOF)")
		}

		if parsed.err != nil {
			return rpcParsed{}, parsed.err
		}

		return parsed, nil
	case <-p.done:
		return rpcParsed{}, errors.New("pi process exited")
	}
}

// handleSideEffects processes events that are common to all commands:
// extension UI auto-cancel and tool call notifications.
func (p *PiProcess) handleSideEffects(evt rpcEvent) error {
	switch evt.Type {
	case rpcTypeExtensionUIRequest:
		p.autoRespondExtensionUI(evt)

	case rpcTypeToolExecutionStart:
		if p.onToolCall != nil {
			p.onToolCall(ToolCallEvent{
				ToolName: evt.ToolName,
				Args:     evt.Args,
			})
		}

	case rpcTypeResponse:
		if evt.Success != nil && !*evt.Success {
			return fmt.Errorf("pi rejected command %q: %s", evt.Command, evt.Error)
		}
	}

	return nil
}

// resultWaiter tracks state across the agent_end / auto_retry_* event
// sequence. Pi's retry loop emits agent_end for each failed attempt
// before starting the next:
//
//	agent_end(stopReason=error) → auto_retry_start → agent_start → … → auto_retry_end
//
// Returning on the first error agent_end is wrong twice over: we miss
// the eventual outcome, and any prompt sent while pi is still looping
// bounces with "already processing". But an error agent_end *may* be
// final — if retry is disabled or the error isn't in pi's retryable
// set, no auto_retry_* events follow and pi just goes idle with
// nothing further on the wire. We must therefore commit the error
// immediately on agent_end and let auto_retry_start/compaction_start
// rescind it if they arrive next.
type resultWaiter struct {
	reply    string
	finalErr string
	onDelta  func(string) // called with each text delta during generation
}

// continuesTurn returns true for events that mean pi is still working
// on the current prompt after an error agent_end — retry backoff or
// overflow-triggered auto-compaction. Both are followed by a fresh
// agent_start/end cycle, so the error that preceded them wasn't final.
func continuesTurn(t string) bool {
	return t == rpcTypeAutoRetryStart || t == "compaction_start"
}

func (w *resultWaiter) handle(evt rpcEvent) (bool, error) {
	switch evt.Type {
	case rpcTypeAutoRetryEnd:
		// success=true fires from the message handler *before* the
		// successful agent_end, so keep draining. success=false:
		// retries exhausted, commit finalError.
		if evt.Success == nil || !*evt.Success {
			w.finalErr = evt.FinalError

			return true, nil
		}

	case rpcTypeMessageUpdate:
		if w.onDelta != nil && evt.AssistantMessageEvent != nil &&
			evt.AssistantMessageEvent.Type == "text_delta" &&
			evt.AssistantMessageEvent.Delta != "" {
			w.onDelta(evt.AssistantMessageEvent.Delta)
		}

	case rpcTypeAgentEnd:
		w.handleAgentEnd(evt)

		return true, nil
	}

	return false, nil
}

// graceDrain peeks at the events channel for up to errGraceWindow,
// looking for an auto_retry_start or compaction_start that would
// rescind a just-committed error. Returns true if one arrived (caller
// should keep draining), false if the window elapsed or the channel
// closed. Reads bypass drainEvents so we don't re-run side effects.
func (p *PiProcess) graceDrain(ctx context.Context, w *resultWaiter) bool {
	timer := time.NewTimer(errGraceWindow)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-p.done:
			return false
		case parsed, ok := <-p.events:
			if !ok || parsed.err != nil {
				return false
			}

			if continuesTurn(parsed.event.Type) {
				w.finalErr = ""

				return true
			}
			// Swallow anything else in the window (turn_end etc).
			// The retry loop starts fresh with agent_start so
			// nothing of value is lost here.
		}
	}
}

func (w *resultWaiter) handleAgentEnd(evt rpcEvent) {
	last := extractLastAssistant(evt.Messages)
	w.reply = last.text

	if last.stopReason == "error" {
		// Commit immediately — if pi deems the error non-retryable
		// it goes idle with nothing further on the wire, so waiting
		// for a follow-up would hang. graceDrain gives a subsequent
		// auto_retry_start or compaction_start a short window
		// to rescind this.
		w.finalErr = last.errorMessage
		slog.Warn("agent_end: provider error", "error", last.errorMessage)

		return
	}

	if w.reply == "" {
		slog.Warn("agent_end contained no assistant text", "messages_len", len(evt.Messages))
	}
}

// errGraceWindow is how long we wait after an error agent_end for a
// rescinding auto_retry_start or compaction_start before treating
// the error as final. Pi emits the follow-up within milliseconds
// (same event-queue tick), so 200ms is generous without adding
// perceptible latency to the rare "pi gave up and went idle" path.
const errGraceWindow = 200 * time.Millisecond

func (p *PiProcess) waitForResult(ctx context.Context, onDelta func(string)) (string, error) {
	var w resultWaiter

	w.onDelta = onDelta

	// The loop may run more than once: drainEvents returns when
	// handle() says "done", but an error agent_end is only
	// *tentatively* done — a short grace window lets auto_retry_start
	// rescind it, at which point we re-enter and keep draining.
	for {
		if err := p.drainEvents(ctx, w.handle); err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("context cancelled: %w", ctx.Err())
			}

			return "", err
		}

		if w.finalErr == "" {
			break // non-error agent_end: definitively done
		}

		if !p.graceDrain(ctx, &w) {
			break // grace window elapsed without rescind: error stands
		}
		// Rescinded — pi is retrying or compacting. Go around again.
	}

	// Empty reply + committed error means the provider refused every
	// attempt, not that the model chose to say nothing. Surface the
	// error so the user sees "429 … long context" and knows to
	// !compact instead of staring at "(empty response)" from
	// retryEmptyResponse re-prompting into the same wall.
	if w.reply == "" && w.finalErr != "" {
		return fmt.Sprintf("Request failed: %s\n\nTry `!compact` to shrink the context, or `!restart` for a clean session.",
			w.finalErr), nil
	}

	return w.reply, nil
}

func (p *PiProcess) waitForCompactResponse(ctx context.Context) (*CompactResult, error) {
	var result *CompactResult

	err := p.drainEvents(ctx, func(evt rpcEvent) (bool, error) {
		if evt.Type != rpcTypeResponse || evt.Command != "compact" {
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

// assistantResult is the subset of an assistant message that
// waitForResult needs to decide whether an agent_end is terminal.
// stopReason "error" means the provider rejected the turn (429, 5xx,
// overloaded); pi's retry machinery *may* follow up, so the caller
// shouldn't treat that agent_end as final until it knows whether
// auto_retry_start arrives.
type assistantResult struct {
	text         string
	stopReason   string
	errorMessage string
}

func extractLastAssistant(messagesRaw json.RawMessage) assistantResult {
	if len(messagesRaw) == 0 {
		return assistantResult{}
	}

	var messages []agentMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		slog.Warn("failed to parse agent_end messages", "error", err)

		return assistantResult{}
	}

	for _, msg := range slices.Backward(messages) {
		if msg.Role != "assistant" {
			continue
		}

		return assistantResult{
			text:         parseAssistantContent(msg.Content),
			stopReason:   msg.StopReason,
			errorMessage: msg.ErrorMessage,
		}
	}

	return assistantResult{}
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

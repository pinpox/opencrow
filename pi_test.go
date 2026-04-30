package main

import (
	"encoding/json"
	"testing"
)

// Event sequences taken from opencrow's journald on eve while Janet's
// session was wedged past the long-context limit. The bug this pins
// down: returning on the first error agent_end meant we re-prompted
// while pi was still in its retry loop, hitting "already processing"
// and surfacing "(empty response)" to the user.
type waiterCase struct {
	name     string
	events   []rpcEvent
	reply    string
	finalErr string
}

const err429 = `429 Extra usage is required for long context requests.`

func waiterCases() []waiterCase {
	return []waiterCase{
		{
			name:   "happy path",
			events: []rpcEvent{agentEnd("end_turn", "", "hello")},
			reply:  "hello",
		},
		{
			// Tool-only turn: empty, non-error. Must terminate so
			// retryEmptyResponse can re-prompt for a summary.
			name:   "tool-only turn",
			events: []rpcEvent{agentEnd("end_turn", "", "")},
		},
		{
			// The hang this fixes: if pi deems the error
			// non-retryable it emits agent_end(error) and goes idle
			// with nothing further on the wire. We must commit the
			// error immediately, not wait for a follow-up event
			// that never comes.
			name:     "non-retryable error commits immediately",
			events:   []rpcEvent{agentEnd("error", "invalid_api_key", "")},
			finalErr: "invalid_api_key",
		},
		{
			// auto_retry_end(failed) overrides a prior commit with
			// the final aggregated error.
			name: "retries exhausted",
			events: []rpcEvent{
				{Type: rpcTypeAutoRetryEnd, Success: ptr(false), FinalError: err429},
			},
			finalErr: err429,
		},
	}
}

func TestResultWaiter(t *testing.T) {
	t.Parallel()

	for _, tc := range waiterCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var w resultWaiter

			for i, e := range tc.events {
				done, err := w.handle(e)
				if err != nil {
					t.Fatalf("event %d: %v", i, err)
				}

				if done {
					break
				}

				if i == len(tc.events)-1 {
					t.Fatal("waiter never terminated")
				}
			}

			if w.reply != tc.reply || w.finalErr != tc.finalErr {
				t.Fatalf("got reply=%q finalErr=%q, want %q/%q",
					w.reply, w.finalErr, tc.reply, tc.finalErr)
			}
		})
	}
}

// The retry-rescind path goes through graceDrain, which reads the
// events channel directly. Test via a real PiProcess channel.
func TestWaitForResult_RetryRescindsError(t *testing.T) {
	t.Parallel()

	ch := make(chan rpcParsed, 8)
	p := &PiProcess{events: ch, done: make(chan struct{})}

	// The sequence we saw on eve: error agent_end, then
	// auto_retry_start within the same second. graceDrain's 200ms
	// window must catch it and keep draining for the eventual
	// success. Pre-fill the buffered channel so there's no goroutine
	// scheduling race in the test.
	for _, e := range []rpcEvent{
		agentEnd("error", err429, ""),
		{Type: rpcTypeAutoRetryStart},
		{Type: rpcTypeAgentStart},
		agentEnd("end_turn", "", "recovered"),
	} {
		ch <- rpcParsed{event: e}
	}

	reply, err := p.waitForResult(t.Context(), nil)
	if err != nil {
		t.Fatalf("waitForResult: %v", err)
	}

	if reply != "recovered" {
		t.Fatalf("reply = %q, want recovered", reply)
	}
}

func agentEnd(stop, errMsg, text string) rpcEvent {
	msg := agentMessage{
		Role:         "assistant",
		Content:      json.RawMessage(`[{"type":"text","text":"` + text + `"}]`),
		StopReason:   stop,
		ErrorMessage: errMsg,
	}

	msgs, err := json.Marshal([]agentMessage{msg})
	if err != nil {
		panic(err)
	}

	return rpcEvent{Type: rpcTypeAgentEnd, Messages: msgs}
}

func ptr[T any](v T) *T { return &v }

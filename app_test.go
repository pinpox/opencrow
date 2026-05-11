package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinpox/opencrow/backend"
)

const testRoom = "!room1"

// mockBackend records calls for testing. It also implements
// modelsBroadcaster so handlers that opportunistically push model
// updates (handleModel) can be observed.
type mockBackend struct {
	mu                    sync.Mutex
	sentMessages          []sentMessage
	sentFiles             []sentFile
	typingCalls           []typingCall
	resetCalls            []string
	broadcastCtxs         []context.Context
	systemPromptExtraText string
	markdownFlavor        backend.MarkdownFlavor
}

type sentMessage struct {
	conversationID string
	text           string
}

type sentFile struct {
	conversationID string
	filePath       string
}

type typingCall struct {
	conversationID string
	typing         bool
}

func (m *mockBackend) Run(_ context.Context) error { return nil }
func (m *mockBackend) Stop()                       {}
func (m *mockBackend) Close() error                { return nil }

func (m *mockBackend) SendMessage(_ context.Context, conversationID string, text string, _ string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sentMessages = append(m.sentMessages, sentMessage{conversationID, text})

	return ""
}

func (m *mockBackend) SendFile(_ context.Context, conversationID string, filePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sentFiles = append(m.sentFiles, sentFile{conversationID, filePath})

	return nil
}

func (m *mockBackend) SetTyping(_ context.Context, conversationID string, typing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.typingCalls = append(m.typingCalls, typingCall{conversationID, typing})
}

func (m *mockBackend) ResetConversation(_ context.Context, conversationID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.resetCalls = append(m.resetCalls, conversationID)
}

func (m *mockBackend) SystemPromptExtra() string {
	return m.systemPromptExtraText
}

func (m *mockBackend) MarkdownFlavor() backend.MarkdownFlavor {
	return m.markdownFlavor
}

// BroadcastModels satisfies modelsBroadcaster. It records the ctx so
// tests can assert the call happened and was passed a real context.
func (m *mockBackend) BroadcastModels(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.broadcastCtxs = append(m.broadcastCtxs, ctx)
}

// newTestApp creates a mockBackend + App wired together for testing.
func newTestApp(t *testing.T) (*App, *mockBackend) {
	t.Helper()

	return newTestAppWithBackend(t, &mockBackend{})
}

func newTestAppWithBackend(t *testing.T, mb *mockBackend) (*App, *mockBackend) {
	t.Helper()

	ctx := context.Background()
	db := newTestDB(ctx, t)

	inbox := newTestInboxWithDB(ctx, t, db)

	worker := NewWorker(inbox, PiConfig{SessionDir: t.TempDir()}, "", "")
	worker.SetBackend(mb)

	app := NewApp(mb, worker, inbox, db)
	worker.SetApp(app)

	return app, mb
}

// sendCommand sends a command message from a default user to testRoom.
func sendCommand(app *App, command string) {
	app.HandleMessage(context.Background(), backend.Message{
		ConversationID: testRoom,
		SenderID:       "@user:example.com",
		Text:           command,
	})
}

// TestApp_Commands covers the !-commands that reply with a single message.
// Each case only differs in the input command and what substrings the
// reply must contain, so a table avoids repeating the setup/assert
// boilerplate five times.
func TestApp_Commands(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		command      string
		wantContains []string
		wantReset    bool
	}{
		{"stop no session", "!stop", []string{"No active session"}, false},
		{"compact no session", "!compact", []string{"No active session"}, false},
		{"compact trailing whitespace", "!compact ", []string{"No active session"}, false},
		{"help trailing newline", "!help\n", []string{"!help", "!restart"}, false},
		{"help", "!help", []string{"!help", "!restart", "!stop", "!compact", "!skills"}, false},
		{"restart", "!restart", []string{"Session restarted"}, true},
		{"skills", "!skills", []string{"No skills loaded"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, mb := newTestApp(t)
			sendCommand(app, tc.command)

			mb.mu.Lock()
			defer mb.mu.Unlock()

			if len(mb.sentMessages) != 1 {
				t.Fatalf("sent %d messages, want 1", len(mb.sentMessages))
			}

			msg := mb.sentMessages[0]
			if msg.conversationID != testRoom {
				t.Errorf("sent to %q, want %q", msg.conversationID, testRoom)
			}

			for _, want := range tc.wantContains {
				if !strings.Contains(msg.text, want) {
					t.Errorf("reply %q missing %q", msg.text, want)
				}
			}

			if tc.wantReset {
				if len(mb.resetCalls) != 1 || mb.resetCalls[0] != testRoom {
					t.Errorf("ResetConversation calls = %v, want [%s]", mb.resetCalls, testRoom)
				}
			} else if len(mb.resetCalls) != 0 {
				t.Errorf("ResetConversation calls = %v, want none", mb.resetCalls)
			}
		})
	}
}

func TestApp_PromptEnqueuesInbox(t *testing.T) {
	t.Parallel()

	app, _ := newTestApp(t)

	app.HandleMessage(context.Background(), backend.Message{
		ConversationID: testRoom,
		SenderID:       "@user:example.com",
		Text:           "hello world",
		MessageID:      "msg-1",
	})

	ctx := context.Background()

	count, err := app.inbox.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if count != 1 {
		t.Fatalf("inbox count = %d, want 1", count)
	}

	item, err := app.inbox.Dequeue(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if item.Source != "user" {
		t.Errorf("Source = %q, want %q", item.Source, "user")
	}

	if item.Priority != PriorityUser {
		t.Errorf("Priority = %d, want %d", item.Priority, PriorityUser)
	}

	if item.Content != "hello world" {
		t.Errorf("Content = %q, want %q", item.Content, "hello world")
	}
}

func TestApp_BuildPromptText_ReplyToUserMessage(t *testing.T) {
	t.Parallel()

	app, _ := newTestApp(t)

	ctx := context.Background()

	// Record a user message as HandleMessage would.
	app.outbox.Put(ctx, "conv1", "user-msg-123", "original question")

	// Now simulate the user replying to their own message.
	replyMsg := backend.Message{
		ConversationID: "conv1",
		SenderID:       "user1",
		Text:           "follow-up",
		MessageID:      "user-msg-456",
		ReplyToID:      "user-msg-123",
	}

	got := app.buildPromptText(ctx, replyMsg)
	want := `[user replied to message: "original question"]
follow-up`

	if got != want {
		t.Errorf("buildPromptText = %q, want %q", got, want)
	}
}

func TestApp_SystemPrompt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"with extra", "You are in a Nostr DM.", "Base prompt\n\nYou are in a Nostr DM."},
		{"no extra", "", "Base prompt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, _ := newTestAppWithBackend(t, &mockBackend{systemPromptExtraText: tc.extra})

			if got := app.systemPrompt("Base prompt"); got != tc.want {
				t.Errorf("systemPrompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatToolCall(t *testing.T) {
	t.Parallel()

	bash := ToolCallEvent{ToolName: "bash", Args: map[string]any{"command": "ls -la"}}
	read := ToolCallEvent{ToolName: "read", Args: map[string]any{"path": "/etc/hosts"}}

	check := func(evt ToolCallEvent, flavor backend.MarkdownFlavor, want string) {
		t.Helper()

		if got := formatToolCall(evt, flavor); got != want {
			t.Errorf("formatToolCall(%s, %d) = %q, want %q", evt.ToolName, flavor, got, want)
		}
	}

	check(bash, backend.MarkdownFull, "🔧\n```sh\nls -la\n```")
	check(bash, backend.MarkdownBasic, "🔧\n```\nls -la\n```")
	check(bash, backend.MarkdownNone, "🔧 ls -la")
	check(read, backend.MarkdownBasic, "📄 reading `/etc/hosts`")
	check(read, backend.MarkdownNone, "📄 reading /etc/hosts")
}

// newFakePiApp wires App + a fake-pi-backed Worker on a shared in-memory
// DB and starts worker.Run in a goroutine. Handlers that go through the
// inbox (ListModels, SetModel, prompt) all dispatch against the same
// store. Cleanup cancels Run and stops pi.
func newFakePiApp(t *testing.T) (*App, *mockBackend) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	db := newTestDB(ctx, t)
	inbox := newTestInboxWithDB(ctx, t, db)

	script, err := filepath.Abs("testdata/fake-pi")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	worker := NewWorker(inbox, PiConfig{
		BinaryPath: "bash",
		BinaryArgs: []string{script},
		SessionDir: dir,
		WorkingDir: dir,
	}, "", "")
	t.Cleanup(worker.stopPi)

	mb := &mockBackend{}
	worker.SetBackend(mb)
	worker.SetRoomID(testRoom)

	app := NewApp(mb, worker, inbox, db)
	worker.SetApp(app)

	go worker.Run(ctx)

	return app, mb
}

// TestApp_ModelsCommand exercises the !models chat command end-to-end
// against the fake-pi stub: the worker cold-spawns pi, ListModels merges
// get_state into get_available_models, and the App formats the result
// as a single text reply. This is the path matrix/signal/nostr clients
// take — they have no structured 'models' event, only chat text.
func TestApp_ModelsCommand(t *testing.T) {
	t.Parallel()

	app, mb := newFakePiApp(t)
	sendCommand(app, "!models")

	msg := waitForReply(t, mb)

	for _, want := range []string{"local/qwen3", "local/gpt-oss", "* local/qwen3"} {
		if !strings.Contains(msg.text, want) {
			t.Errorf("reply %q missing %q", msg.text, want)
		}
	}
}

// TestApp_ModelCommand covers !model dispatch: argument parsing, the
// success path (response echoes the new active model), and the two
// usage-error paths that short-circuit before touching the worker.
func TestApp_ModelCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		command      string
		wantContains string
		// hitsWorker is true when the case is expected to round-trip
		// through pi (success path). False cases reject in-process.
		hitsWorker bool
	}{
		{"no arg", "!model", "Usage: !model", false},
		{"missing slash", "!model qwen3", "Invalid model spec", false},
		{"empty provider", "!model /qwen3", "Invalid model spec", false},
		{"empty id", "!model local/", "Invalid model spec", false},
		{"success", "!model local/gpt-oss", "Model switched to local/gpt-oss", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, mb := newFakePiApp(t)
			sendCommand(app, tc.command)

			msg := waitForReply(t, mb)

			if !strings.Contains(msg.text, tc.wantContains) {
				t.Errorf("reply %q missing %q", msg.text, tc.wantContains)
			}

			_ = tc.hitsWorker // documents intent; no further assert needed.
		})
	}
}

// waitForReply blocks until mockBackend has received at least one
// SendMessage and returns it. Model commands go through the inbox so
// the reply lands asynchronously after worker.Run dispatches.
func waitForReply(t *testing.T, mb *mockBackend) sentMessage {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mb.mu.Lock()
		n := len(mb.sentMessages)
		mb.mu.Unlock()

		if n > 0 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.sentMessages) == 0 {
		t.Fatal("no reply received within deadline")
	}

	return mb.sentMessages[0]
}

// waitForBroadcastCount blocks until mockBackend has recorded `want`
// BroadcastModels calls or a deadline expires. Used to synchronize
// against the worker's async post-spawn broadcast goroutine.
func waitForBroadcastCount(t *testing.T, mb *mockBackend, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mb.mu.Lock()
		n := len(mb.broadcastCtxs)
		mb.mu.Unlock()

		if n >= want {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()

	t.Fatalf("BroadcastModels count = %d, want >= %d", len(mb.broadcastCtxs), want)
}

// TestApp_ModelCommand_BroadcastsOnSuccess covers the multi-client GUI
// case: after a successful !model switch, the App pushes a fresh model
// list to other connected clients so dropdowns reconcile without a
// manual reopen. The mockBackend satisfies modelsBroadcaster; matrix /
// signal / nostr don't, and for them the call is silently skipped (a
// separate test isn't necessary because not implementing the interface
// is the absence of behavior).
func TestApp_ModelCommand_BroadcastsOnSuccess(t *testing.T) {
	t.Parallel()

	app, mb := newFakePiApp(t)

	// Warm pi up first. The worker's ensurePi hook fires an async
	// broadcast on cold-spawn (existing behaviour from the socket feature
	// commit). Wait for that to land before snapshotting, so the count we
	// observe after !model isolates the broadcast handleModel itself
	// triggers.
	sendCommand(app, "!models")

	_ = waitForReply(t, mb)

	waitForBroadcastCount(t, mb, 1)

	mb.mu.Lock()
	before := len(mb.broadcastCtxs)
	mb.sentMessages = nil
	mb.mu.Unlock()

	sendCommand(app, "!model local/qwen3")

	// The confirmation reply is sent after BroadcastModels, so observing
	// the reply means handleModel's broadcast already happened. Pi is
	// already alive at this point so ensurePi does NOT fire its own
	// spawn broadcast — anything past `before` came from handleModel.
	_ = waitForReply(t, mb)

	mb.mu.Lock()
	defer mb.mu.Unlock()

	gained := len(mb.broadcastCtxs) - before
	if gained != 1 {
		t.Fatalf("handleModel triggered %d BroadcastModels calls, want 1 (before=%d, after=%d)",
			gained, before, len(mb.broadcastCtxs))
	}

	if mb.broadcastCtxs[len(mb.broadcastCtxs)-1] == nil {
		t.Fatal("BroadcastModels received a nil context")
	}
}

// TestApp_ModelCommand_NoBroadcastOnFailure asserts the broadcast is
// skipped when set-model fails (bad provider/id, pi rejects, etc.) —
// pushing stale state to all clients on a failed switch would be worse
// than the original problem.
func TestApp_ModelCommand_NoBroadcastOnFailure(t *testing.T) {
	t.Parallel()

	app, mb := newFakePiApp(t)
	// Invalid spec rejects in-process before worker.SetModel is called.
	sendCommand(app, "!model qwen3")

	_ = waitForReply(t, mb)

	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.broadcastCtxs) != 0 {
		t.Fatalf("BroadcastModels called %d times on failure, want 0", len(mb.broadcastCtxs))
	}
}

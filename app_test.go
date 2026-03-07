package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/pinpox/opencrow/backend"
)

const testRoom = "!room1"

// mockBackend records calls for testing.
type mockBackend struct {
	mu                    sync.Mutex
	sentMessages          []sentMessage
	sentFiles             []sentFile
	typingCalls           []typingCall
	resetCalls            []string
	systemPromptExtraText string
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

func TestApp_Compact_NoSession(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{}
	pool := NewPiPool(PiConfig{SessionDir: t.TempDir()})
	app := NewApp(mb, pool, nil, t.TempDir())

	ctx := context.Background()
	app.HandleMessage(ctx, backend.Message{
		ConversationID: testRoom,
		SenderID:       "@user:example.com",
		Text:           "!compact",
	})

	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.sentMessages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(mb.sentMessages))
	}

	if !strings.Contains(mb.sentMessages[0].text, "No active session") {
		t.Errorf("expected 'No active session' message, got %q", mb.sentMessages[0].text)
	}
}

func TestApp_Help(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{}
	pool := NewPiPool(PiConfig{SessionDir: t.TempDir()})
	app := NewApp(mb, pool, nil, t.TempDir())

	ctx := context.Background()
	app.HandleMessage(ctx, backend.Message{
		ConversationID: testRoom,
		SenderID:       "@user:example.com",
		Text:           "!help",
	})

	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.sentMessages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(mb.sentMessages))
	}

	msg := mb.sentMessages[0]
	if msg.conversationID != testRoom {
		t.Errorf("sent to %q, want %q", msg.conversationID, testRoom)
	}

	for _, cmd := range []string{"!help", "!restart", "!stop", "!compact", "!skills"} {
		if !strings.Contains(msg.text, cmd) {
			t.Errorf("help text missing %q", cmd)
		}
	}
}

func TestApp_Restart(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{}
	pool := NewPiPool(PiConfig{SessionDir: t.TempDir()})
	app := NewApp(mb, pool, nil, t.TempDir())

	ctx := context.Background()
	app.HandleMessage(ctx, backend.Message{
		ConversationID: testRoom,
		SenderID:       "@user:example.com",
		Text:           "!restart",
	})

	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.resetCalls) != 1 || mb.resetCalls[0] != testRoom {
		t.Errorf("ResetConversation calls = %v, want [!room1]", mb.resetCalls)
	}

	if len(mb.sentMessages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(mb.sentMessages))
	}

	if mb.sentMessages[0].conversationID != testRoom {
		t.Errorf("sent to %q, want %q", mb.sentMessages[0].conversationID, testRoom)
	}
}

func TestApp_Skills(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{}
	pool := NewPiPool(PiConfig{SessionDir: t.TempDir()})
	app := NewApp(mb, pool, nil, t.TempDir())

	ctx := context.Background()
	app.HandleMessage(ctx, backend.Message{
		ConversationID: testRoom,
		SenderID:       "@user:example.com",
		Text:           "!skills",
	})

	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.sentMessages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(mb.sentMessages))
	}
}

func TestApp_BuildPromptText_ReplyToUserMessage(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{}
	pool := NewPiPool(PiConfig{SessionDir: t.TempDir()})
	app := NewApp(mb, pool, nil, t.TempDir())

	// Record a user message as HandleMessage would.
	app.sent.Put("conv1", "user-msg-123", "original question")

	// Now simulate the user replying to their own message.
	replyMsg := backend.Message{
		ConversationID: "conv1",
		SenderID:       "user1",
		Text:           "follow-up",
		MessageID:      "user-msg-456",
		ReplyToID:      "user-msg-123",
	}

	got := app.buildPromptText(replyMsg)
	want := `[user replied to message: "original question"]
follow-up`

	if got != want {
		t.Errorf("buildPromptText = %q, want %q", got, want)
	}
}

func TestApp_SystemPrompt(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{systemPromptExtraText: "You are in a Nostr DM."}
	pool := NewPiPool(PiConfig{SessionDir: t.TempDir()})
	app := NewApp(mb, pool, nil, t.TempDir())

	got := app.systemPrompt("Base prompt")

	want := "Base prompt\n\nYou are in a Nostr DM."
	if got != want {
		t.Errorf("systemPrompt = %q, want %q", got, want)
	}
}

func TestApp_SystemPrompt_NoExtra(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{}
	pool := NewPiPool(PiConfig{SessionDir: t.TempDir()})
	app := NewApp(mb, pool, nil, t.TempDir())

	got := app.systemPrompt("Base prompt")
	if got != "Base prompt" {
		t.Errorf("systemPrompt = %q, want %q", got, "Base prompt")
	}
}

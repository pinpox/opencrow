package socket

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinpox/opencrow/backend"
)

func TestNew_RequiresSocketPath(t *testing.T) {
	t.Parallel()

	_, err := New(Config{}, func(_ context.Context, _ backend.Message) {})
	if err == nil {
		t.Fatal("expected error for empty SocketPath")
	}
}

func TestNew_DefaultName(t *testing.T) {
	t.Parallel()

	b, err := New(Config{SocketPath: "/tmp/test.sock"}, func(_ context.Context, _ backend.Message) {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if b.cfg.Name != "OpenCrow" {
		t.Errorf("Name = %q, want OpenCrow", b.cfg.Name)
	}
}

// startBackend starts a socket backend in a goroutine, returning the
// backend and a cleanup function. The socket is created in t.TempDir().
func startBackend(t *testing.T, handler backend.MessageHandler) (*Backend, string, context.CancelFunc) {
	t.Helper()

	sockPath := filepath.Join(t.TempDir(), "test.sock")

	b, err := New(Config{SocketPath: sockPath, Name: "TestBot"}, handler)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		_ = b.Run(ctx)
	}()

	// Wait for socket to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var d net.Dialer
		if conn, err := d.DialContext(context.Background(), "unix", sockPath); err == nil {
			conn.Close()

			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	return b, sockPath, cancel
}

func dial(t *testing.T, sockPath string) net.Conn {
	t.Helper()

	var d net.Dialer

	conn, err := d.DialContext(context.Background(), "unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	return conn
}

func readEvent(t *testing.T, conn net.Conn) event {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 4096)

	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var ev event
	if err := json.Unmarshal(buf[:n], &ev); err != nil {
		t.Fatalf("unmarshal event: %v (raw: %s)", err, buf[:n])
	}

	return ev
}

func sendCommand(t *testing.T, conn net.Conn, cmd command) {
	t.Helper()

	line, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	line = append(line, '\n')

	if _, err := conn.Write(line); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestReplay_ReturnsStatus(t *testing.T) {
	t.Parallel()

	_, sockPath, cancel := startBackend(t, func(_ context.Context, _ backend.Message) {})
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	sendCommand(t, conn, command{Cmd: cmdReplay})

	ev := readEvent(t, conn)
	if ev.Kind != evStatus {
		t.Fatalf("Kind = %q, want %q", ev.Kind, evStatus)
	}

	if ev.Name != "TestBot" {
		t.Errorf("Name = %q, want TestBot", ev.Name)
	}

	if !ev.Streaming {
		t.Error("Streaming should be true")
	}
}

func TestSend_DeliversToHandler(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		received []backend.Message
	)

	handler := func(_ context.Context, msg backend.Message) {
		mu.Lock()

		received = append(received, msg)

		mu.Unlock()
	}

	_, sockPath, cancel := startBackend(t, handler)
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	sendCommand(t, conn, command{Cmd: cmdSend, Text: "hello"})

	// Read the echo event (outgoing message).
	ev := readEvent(t, conn)
	if ev.Kind != evMsg {
		t.Fatalf("Kind = %q, want %q", ev.Kind, evMsg)
	}

	if ev.Msg.Dir != dirOut {
		t.Errorf("Dir = %q, want %q", ev.Msg.Dir, dirOut)
	}

	if ev.Msg.Content != "hello" {
		t.Errorf("Content = %q, want hello", ev.Msg.Content)
	}

	// Check handler received the message.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("handler got %d messages, want 1", len(received))
	}

	if received[0].Text != "hello" {
		t.Errorf("handler Text = %q, want hello", received[0].Text)
	}

	if received[0].ConversationID != conversationID {
		t.Errorf("ConversationID = %q, want %q", received[0].ConversationID, conversationID)
	}
}

func TestSend_EmptyTextIgnored(t *testing.T) {
	t.Parallel()

	called := false

	handler := func(_ context.Context, _ backend.Message) {
		called = true
	}

	_, sockPath, cancel := startBackend(t, handler)
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	sendCommand(t, conn, command{Cmd: cmdSend, Text: ""})
	time.Sleep(50 * time.Millisecond)

	if called {
		t.Error("handler should not be called for empty text")
	}
}

func TestSendMessage_PushesToClients(t *testing.T) {
	t.Parallel()

	b, sockPath, cancel := startBackend(t, func(_ context.Context, _ backend.Message) {})
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	// SendMessage from the backend side (simulating a bot reply).
	id := b.SendMessage(context.Background(), "local", "bot reply", "")

	if id == "" {
		t.Fatal("SendMessage returned empty id")
	}

	ev := readEvent(t, conn)
	if ev.Kind != evMsg {
		t.Fatalf("Kind = %q, want %q", ev.Kind, evMsg)
	}

	if ev.Msg.Dir != dirIn {
		t.Errorf("Dir = %q, want %q", ev.Msg.Dir, dirIn)
	}

	if ev.Msg.Content != "bot reply" {
		t.Errorf("Content = %q, want 'bot reply'", ev.Msg.Content)
	}
}

func TestSendFile_PushesImageEvent(t *testing.T) {
	t.Parallel()

	b, sockPath, cancel := startBackend(t, func(_ context.Context, _ backend.Message) {})
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	err := b.SendFile(context.Background(), "local", "/tmp/chart.png")
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	ev := readEvent(t, conn)
	if ev.Kind != evMsg {
		t.Fatalf("Kind = %q, want %q", ev.Kind, evMsg)
	}

	if ev.Msg.Image != "/tmp/chart.png" {
		t.Errorf("Image = %q, want /tmp/chart.png", ev.Msg.Image)
	}
}

func TestSendFile_Command(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		received []backend.Message
	)

	handler := func(_ context.Context, msg backend.Message) {
		mu.Lock()

		received = append(received, msg)

		mu.Unlock()
	}

	_, sockPath, cancel := startBackend(t, handler)
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	sendCommand(t, conn, command{Cmd: cmdSendFile, Path: "/tmp/photo.jpg"})

	// Read the echo event.
	ev := readEvent(t, conn)
	if ev.Msg.Image != "/tmp/photo.jpg" {
		t.Errorf("Image = %q, want /tmp/photo.jpg", ev.Msg.Image)
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("handler got %d messages, want 1", len(received))
	}

	if received[0].Text == "" {
		t.Error("handler Text should contain attachment text")
	}
}

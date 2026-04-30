package socket

import (
	"bufio"
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

// connScanner wraps a net.Conn with a bufio.Scanner for NDJSON reading.
// Reuse across readEvent calls to avoid losing buffered data.
type connScanner struct {
	sc *bufio.Scanner
}

func newConnScanner(conn net.Conn) *connScanner {
	return &connScanner{sc: bufio.NewScanner(conn)}
}

func (cs *connScanner) readEvent(t *testing.T, conn net.Conn) event {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	if !cs.sc.Scan() {
		t.Fatalf("read: %v", cs.sc.Err())
	}

	var ev event
	if err := json.Unmarshal(cs.sc.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal event: %v (raw: %s)", err, cs.sc.Text())
	}

	return ev
}

// readEvent is a convenience wrapper for tests that only read one event.
func readEvent(t *testing.T, conn net.Conn) event {
	t.Helper()

	return newConnScanner(conn).readEvent(t, conn)
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

func TestSendDelta_StreamsToClients(t *testing.T) {
	t.Parallel()

	b, sockPath, cancel := startBackend(t, func(_ context.Context, _ backend.Message) {})
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	cs := newConnScanner(conn)

	// Send two deltas.
	b.SendDelta(context.Background(), "local", "stream-1", "Hello")
	b.SendDelta(context.Background(), "local", "stream-1", " world")

	ev1 := cs.readEvent(t, conn)
	if ev1.Kind != "delta" {
		t.Fatalf("Kind = %q, want delta", ev1.Kind)
	}

	if ev1.Target != "stream-1" {
		t.Errorf("Target = %q, want stream-1", ev1.Target)
	}

	if ev1.Text != "Hello" {
		t.Errorf("Text = %q, want Hello", ev1.Text)
	}

	ev2 := cs.readEvent(t, conn)
	if ev2.Text != " world" {
		t.Errorf("Text = %q, want ' world'", ev2.Text)
	}
}

func TestBackend_ImplementsStreamer(t *testing.T) {
	t.Parallel()

	b, _, cancel := startBackend(t, func(_ context.Context, _ backend.Message) {})
	defer cancel()

	// Verify the socket backend implements backend.Streamer.
	var _ backend.Streamer = b
}

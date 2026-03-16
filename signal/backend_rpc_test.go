package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinpox/opencrow/backend"
)

// fakeSignalDaemon is a minimal JSON-RPC server that mimics signal-cli's
// daemon mode.  It listens on a Unix socket and lets the test push "receive"
// notifications and respond to RPC requests (send, subscribeReceive, …).
//
// The approach mirrors the Nostr backend tests: exercise Run() end-to-end
// with a real socket, real message flow, real handler callback.
type fakeSignalDaemon struct {
	t        *testing.T
	listener net.Listener
	sockPath string

	connMu sync.Mutex
	conn   net.Conn

	// requestCh exposes every incoming RPC request to the test.
	requestCh chan rpcRequest

	done chan struct{}
}

func newFakeSignalDaemon(t *testing.T) *fakeSignalDaemon {
	t.Helper()

	// macOS temp paths are too long for Unix sockets (104-byte limit).
	sockDir, err := os.MkdirTemp("/tmp", "sig-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}

	t.Cleanup(func() { os.RemoveAll(sockDir) })

	sockPath := filepath.Join(sockDir, "s.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	f := &fakeSignalDaemon{
		t:         t,
		listener:  ln,
		sockPath:  sockPath,
		requestCh: make(chan rpcRequest, 64),
		done:      make(chan struct{}),
	}

	go f.acceptLoop()

	t.Cleanup(func() {
		_ = ln.Close()
		<-f.done
	})

	return f
}

func (f *fakeSignalDaemon) acceptLoop() {
	defer close(f.done)

	conn, err := f.listener.Accept()
	if err != nil {
		return
	}

	f.connMu.Lock()
	f.conn = conn
	f.connMu.Unlock()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		f.requestCh <- req
	}
}

func (f *fakeSignalDaemon) respond(id string, result any) {
	f.t.Helper()

	f.connMu.Lock()
	conn := f.conn
	f.connMu.Unlock()

	if conn == nil {
		f.t.Fatal("no connection yet")
	}

	resultJSON, _ := json.Marshal(result)

	_, err := fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":"%s","result":%s}`+"\n", id, resultJSON)
	if err != nil {
		f.t.Fatalf("write response: %v", err)
	}
}

func (f *fakeSignalDaemon) waitConnected(timeout time.Duration) {
	f.t.Helper()

	deadline := time.After(timeout)

	for {
		f.connMu.Lock()
		c := f.conn
		f.connMu.Unlock()

		if c != nil {
			return
		}

		select {
		case <-deadline:
			f.t.Fatal("timed out waiting for connection")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (f *fakeSignalDaemon) pushNotification(method string, params any) {
	f.t.Helper()

	f.waitConnected(3 * time.Second)

	f.connMu.Lock()
	conn := f.conn
	f.connMu.Unlock()

	paramsJSON, _ := json.Marshal(params)

	_, err := fmt.Fprintf(conn, `{"jsonrpc":"2.0","method":"%s","params":%s}`+"\n", method, paramsJSON)
	if err != nil {
		f.t.Fatalf("write notification: %v", err)
	}
}

func (f *fakeSignalDaemon) waitRequest(timeout time.Duration) (rpcRequest, bool) {
	select {
	case req := <-f.requestCh:
		return req, true
	case <-time.After(timeout):
		return rpcRequest{}, false
	}
}

// autoRespond runs in a goroutine and replies to subscribeReceive (and
// optionally unsubscribeReceive) so the backend's Run() can proceed.
// It also handles "send" calls by recording them. Stops when ctx is done.
func (f *fakeSignalDaemon) autoRespond(ctx context.Context, sends *sendRecorder) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-f.requestCh:
			switch req.Method {
			case "subscribeReceive":
				f.respond(req.ID, 0)
			case "unsubscribeReceive":
				f.respond(req.ID, nil)
			case "send":
				if sends != nil {
					sends.record(req)
				}

				f.respond(req.ID, map[string]any{"timestamp": time.Now().UnixMilli()})
			default:
				f.respond(req.ID, nil)
			}
		}
	}
}

// --- helpers shared by all integration tests ---

type messageCollector struct {
	mu       sync.Mutex
	messages []backend.Message
}

func (c *messageCollector) handler(_ context.Context, msg backend.Message) {
	c.mu.Lock()
	c.messages = append(c.messages, msg)
	c.mu.Unlock()
}

func (c *messageCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.messages)
}

func (c *messageCollector) get() []backend.Message {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]backend.Message, len(c.messages))
	copy(out, c.messages)

	return out
}

type sendRecorder struct {
	mu    sync.Mutex
	calls []rpcRequest
}

func (s *sendRecorder) record(req rpcRequest) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()
}

func (s *sendRecorder) get() []rpcRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]rpcRequest, len(s.calls))
	copy(out, s.calls)

	return out
}

func waitForMessages(t *testing.T, c *messageCollector, n int) {
	t.Helper()

	deadline := time.After(3 * time.Second)

	for {
		if c.count() >= n {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d message(s), got %d", n, c.count())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func makeEnvelope(sender, text string, ts int64, extras map[string]any) map[string]any {
	dm := map[string]any{
		"timestamp": ts,
		"message":   text,
	}

	for k, v := range extras {
		dm[k] = v
	}

	return map[string]any{
		"envelope": map[string]any{
			"sourceNumber": sender,
			"timestamp":    ts,
			"dataMessage":  dm,
		},
	}
}

// writeFakeSignalCLI creates a shell script that acts as a fake signal-cli:
// it ignores all CLI flags, just opens the --socket path and listens.
// This lets us test the real Run() path including process management.
func writeFakeSignalCLI(t *testing.T, sockPath string) string {
	t.Helper()

	// The script simply creates the socket and echoes back JSON-RPC
	// responses for subscribeReceive. It exits on stdin close / signal.
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not found: %v", err)
	}

	script := fmt.Sprintf(`#!%s
set -euo pipefail

# Parse --socket= from args
SOCK=""
for arg in "$@"; do
  case "$arg" in
    --socket=*) SOCK="${arg#--socket=}" ;;
  esac
done

if [ -z "$SOCK" ]; then
  SOCK=%q
fi

# Use socat to create a Unix socket server that auto-responds to JSON-RPC.
# For each line: if it contains subscribeReceive, reply with subscription id.
exec socat UNIX-LISTEN:"$SOCK",fork SYSTEM:'
while IFS= read -r line; do
  id=$(echo "$line" | grep -oP "\"id\"\\s*:\\s*\"\\K[^\"]+")
  method=$(echo "$line" | grep -oP "\"method\"\\s*:\\s*\"\\K[^\"]+")
  case "$method" in
    subscribeReceive) echo "{\"jsonrpc\":\"2.0\",\"id\":\"$id\",\"result\":0}" ;;
    unsubscribeReceive) echo "{\"jsonrpc\":\"2.0\",\"id\":\"$id\",\"result\":null}" ;;
    send) echo "{\"jsonrpc\":\"2.0\",\"id\":\"$id\",\"result\":{\"timestamp\":1700000099999}}" ;;
    *) echo "{\"jsonrpc\":\"2.0\",\"id\":\"$id\",\"result\":{}}" ;;
  esac
done
'
`, bashPath, sockPath)

	dir := t.TempDir()
	binPath := filepath.Join(dir, "signal-cli")

	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return binPath
}

// newTestBackend wires up a Backend that connects to the given fake daemon
// socket. It bypasses the real daemon startup by directly connecting.
func newTestBackend(t *testing.T, fake *fakeSignalDaemon, account string, allowedUsers map[string]struct{}, handler backend.MessageHandler) *Backend {
	t.Helper()

	b := &Backend{
		cfg: Config{
			Account:    account,
			SocketPath: fake.sockPath,
		},
		handler:        handler,
		allowedUsers:   allowedUsers,
		subscriptionID: -1,
	}

	conn, err := net.Dial("unix", fake.sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	client := newJSONRPCClient(conn)
	b.setRPC(client)

	// Wait until the fake daemon has accepted the connection so writes
	// (pushNotification, autoRespond) can proceed immediately.
	fake.waitConnected(3 * time.Second)

	t.Cleanup(func() { b.clearRPC() })

	return b
}

// runReceiveLoop mirrors the core of Run(): subscribe, then select on
// notifications/errors/ctx. This lets integration tests exercise the
// full message flow without needing a real signal-cli binary.
func runReceiveLoop(ctx context.Context, b *Backend) error {
	client := b.getRPC()

	if err := b.subscribeReceive(ctx, client); err != nil {
		return err
	}

	defer b.unsubscribeReceive(ctx, client)

	notifications := client.Notifications()
	errs := client.Errors()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			if err != nil && ctx.Err() == nil {
				return err
			}
		case n := <-notifications:
			if n.Method != "receive" {
				continue
			}

			msg, ok, err := decodeReceiveNotification(n.Params, b.cfg.ConfigDir)
			if err != nil || !ok {
				continue
			}

			if msg.SenderID == b.cfg.Account {
				continue
			}

			if !b.isAllowed(msg.SenderID) {
				continue
			}

			if !b.claimConversation(msg.ConversationID) {
				continue
			}

			b.handler(ctx, *msg)
		}
	}
}

// --- Integration Tests ---

func TestRun_ReceivesDirectMessage(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	fake.pushNotification("receive", makeEnvelope("+49222", "hello bot", 1700000000100, nil))
	waitForMessages(t, mc, 1)

	cancel()
	<-runDone

	msgs := mc.get()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	if msgs[0].ConversationID != "+49222" {
		t.Errorf("ConversationID = %q, want +49222", msgs[0].ConversationID)
	}

	if msgs[0].Text != "hello bot" {
		t.Errorf("Text = %q, want %q", msgs[0].Text, "hello bot")
	}

	if msgs[0].SenderID != "+49222" {
		t.Errorf("SenderID = %q, want +49222", msgs[0].SenderID)
	}

	if msgs[0].MessageID != "1700000000100" {
		t.Errorf("MessageID = %q, want 1700000000100", msgs[0].MessageID)
	}
}

func TestRun_ReceivesGroupMessage(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	fake.pushNotification("receive", makeEnvelope("+49222", "group msg", 1700000000200, map[string]any{
		"groupInfo": map[string]any{"groupId": "GRP123="},
	}))
	waitForMessages(t, mc, 1)

	cancel()
	<-runDone

	msgs := mc.get()
	if msgs[0].ConversationID != "signal-group:GRP123=" {
		t.Errorf("ConversationID = %q, want signal-group:GRP123=", msgs[0].ConversationID)
	}
}

func TestRun_DropsDisallowedUser(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	allowed := map[string]struct{}{"+49222": {}}
	b := newTestBackend(t, fake, "+49111", allowed, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	fake.pushNotification("receive", makeEnvelope("+49333", "should be dropped", 1700000000300, nil))
	time.Sleep(200 * time.Millisecond)

	fake.pushNotification("receive", makeEnvelope("+49222", "should arrive", 1700000000301, nil))
	waitForMessages(t, mc, 1)
	time.Sleep(200 * time.Millisecond)

	cancel()
	<-runDone

	msgs := mc.get()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	if msgs[0].Text != "should arrive" {
		t.Errorf("Text = %q, want %q", msgs[0].Text, "should arrive")
	}
}

func TestRun_DropsSelfMessages(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	fake.pushNotification("receive", makeEnvelope("+49111", "self echo", 1700000000400, nil))
	time.Sleep(300 * time.Millisecond)

	cancel()
	<-runDone

	if mc.count() != 0 {
		t.Fatalf("got %d messages, want 0 (self-messages dropped)", mc.count())
	}
}

func TestRun_SingleActiveConversation(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	// First message claims conversation with +49222.
	fake.pushNotification("receive", makeEnvelope("+49222", "from A", 1700000000500, nil))
	waitForMessages(t, mc, 1)

	// Second message from different user — should be dropped.
	fake.pushNotification("receive", makeEnvelope("+49333", "from B", 1700000000501, nil))
	time.Sleep(200 * time.Millisecond)

	// Third from same conversation — should arrive.
	fake.pushNotification("receive", makeEnvelope("+49222", "from A again", 1700000000502, nil))
	waitForMessages(t, mc, 2)

	cancel()
	<-runDone

	msgs := mc.get()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	if msgs[0].Text != "from A" || msgs[1].Text != "from A again" {
		t.Errorf("texts = [%q, %q]", msgs[0].Text, msgs[1].Text)
	}
}

func TestRun_ResetConversationUnlocks(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	// Claim +49222.
	fake.pushNotification("receive", makeEnvelope("+49222", "first", 1700000000600, nil))
	waitForMessages(t, mc, 1)

	// Reset.
	b.ResetConversation(ctx, "+49222")

	// Now +49333 should succeed.
	fake.pushNotification("receive", makeEnvelope("+49333", "second", 1700000000601, nil))
	waitForMessages(t, mc, 2)

	cancel()
	<-runDone

	msgs := mc.get()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	if msgs[1].SenderID != "+49333" {
		t.Errorf("second SenderID = %q, want +49333", msgs[1].SenderID)
	}
}

func TestRun_ReceivesAttachments(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)
	b.cfg.ConfigDir = "/var/lib/signal"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	fake.pushNotification("receive", makeEnvelope("+49222", "check this", 1700000000700, map[string]any{
		"attachments": []any{
			map[string]any{"filename": "photo.jpg", "contentType": "image/jpeg"},
		},
	}))
	waitForMessages(t, mc, 1)

	cancel()
	<-runDone

	msgs := mc.get()
	if !strings.Contains(msgs[0].Text, "check this") {
		t.Errorf("missing message body in %q", msgs[0].Text)
	}

	if !strings.Contains(msgs[0].Text, "/var/lib/signal/photo.jpg") {
		t.Errorf("missing resolved path in %q", msgs[0].Text)
	}

	if !strings.Contains(msgs[0].Text, "[User sent a file") {
		t.Errorf("missing attachment info in %q", msgs[0].Text)
	}
}

func TestRun_AttachmentOnlyMessage(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)
	b.cfg.ConfigDir = "/data"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	// No "message" field — attachment only.
	fake.pushNotification("receive", makeEnvelope("+49222", "", 1700000000800, map[string]any{
		"attachments": []any{
			map[string]any{"filename": "voice.ogg", "contentType": "audio/ogg"},
		},
	}))
	waitForMessages(t, mc, 1)

	cancel()
	<-runDone

	if !strings.Contains(mc.get()[0].Text, "voice.ogg") {
		t.Errorf("expected attachment text, got %q", mc.get()[0].Text)
	}
}

func TestRun_WrappedSubscriptionPayload(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	// Signal-cli wraps subscription notifications in {"subscription":N,"result":{...}}.
	wrapped := map[string]any{
		"subscription": 0,
		"result": map[string]any{
			"envelope": map[string]any{
				"sourceNumber": "+49222",
				"timestamp":    1700000000900,
				"dataMessage": map[string]any{
					"timestamp": 1700000000900,
					"message":   "wrapped msg",
				},
			},
		},
	}
	fake.pushNotification("receive", wrapped)
	waitForMessages(t, mc, 1)

	cancel()
	<-runDone

	if mc.get()[0].Text != "wrapped msg" {
		t.Errorf("Text = %q, want %q", mc.get()[0].Text, "wrapped msg")
	}
}

func TestRun_QuoteReply(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	mc := &messageCollector{}
	b := newTestBackend(t, fake, "+49111", nil, mc.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	runDone := make(chan error, 1)
	go func() { runDone <- runReceiveLoop(ctx, b) }()

	time.Sleep(100 * time.Millisecond)

	fake.pushNotification("receive", makeEnvelope("+49222", "replying", 1700000001000, map[string]any{
		"quote": map[string]any{"id": 1699999999000},
	}))
	waitForMessages(t, mc, 1)

	cancel()
	<-runDone

	if mc.get()[0].ReplyToID != "1699999999000" {
		t.Errorf("ReplyToID = %q, want 1699999999000", mc.get()[0].ReplyToID)
	}
}

func TestSendMessage_DirectAndGroup(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	sends := &sendRecorder{}
	b := newTestBackend(t, fake, "+49111", nil, func(_ context.Context, _ backend.Message) {})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, sends)

	// Send a DM.
	result := b.SendMessage(ctx, "+49222", "hello", "")
	if result == "" {
		t.Error("expected non-empty timestamp")
	}

	// Send to a group.
	b.SendMessage(ctx, "signal-group:GRP=", "hi group", "")

	// Send with quote.
	b.SendMessage(ctx, "+49222", "reply", "1700000000999")

	time.Sleep(100 * time.Millisecond)

	calls := sends.get()
	if len(calls) != 3 {
		t.Fatalf("got %d send calls, want 3", len(calls))
	}

	// Check DM params.
	p0 := marshalParams(t, calls[0])
	if p0["message"] != "hello" {
		t.Errorf("call 0 message = %v", p0["message"])
	}

	if recipients, ok := p0["recipient"].([]any); !ok || len(recipients) != 1 || recipients[0] != "+49222" {
		t.Errorf("call 0 recipient = %v", p0["recipient"])
	}

	// Check group params.
	p1 := marshalParams(t, calls[1])
	if p1["groupId"] != "GRP=" {
		t.Errorf("call 1 groupId = %v", p1["groupId"])
	}

	if _, has := p1["recipient"]; has {
		t.Error("group call should not have recipient")
	}

	// Check quote params.
	p2 := marshalParams(t, calls[2])
	if qt, ok := p2["quoteTimestamp"].(float64); !ok || int64(qt) != 1700000000999 {
		t.Errorf("call 2 quoteTimestamp = %v", p2["quoteTimestamp"])
	}
}

func TestSendFile(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	sends := &sendRecorder{}
	b := newTestBackend(t, fake, "+49111", nil, func(_ context.Context, _ backend.Message) {})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, sends)

	if err := b.SendFile(ctx, "+49222", "/tmp/photo.jpg"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	calls := sends.get()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}

	p := marshalParams(t, calls[0])
	if p["attachment"] != "/tmp/photo.jpg" {
		t.Errorf("attachment = %v", p["attachment"])
	}
}

func TestSendMessage_EmptyTextNoop(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalDaemon(t)
	b := newTestBackend(t, fake, "+49111", nil, func(_ context.Context, _ backend.Message) {})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go fake.autoRespond(ctx, nil)

	result := b.SendMessage(ctx, "+49222", "   ", "")
	if result != "" {
		t.Errorf("expected empty result for blank message, got %q", result)
	}
}

func TestSendMessage_RPCDisconnected(t *testing.T) {
	t.Parallel()

	b := &Backend{
		cfg:            Config{Account: "+49111"},
		handler:        func(_ context.Context, _ backend.Message) {},
		subscriptionID: -1,
	}

	result := b.SendMessage(context.Background(), "+49222", "hello", "")
	if result != "" {
		t.Errorf("expected empty result when disconnected, got %q", result)
	}
}

// TestRun_RealProcessLifecycle tests with a real fake signal-cli binary
// (shell script) to exercise the full Run() path including process
// management, if socat is available.
func TestRun_RealProcessLifecycle(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat not available, skipping process lifecycle test")
	}

	sockDir, err := os.MkdirTemp("/tmp", "sig-lifecycle-*")
	if err != nil {
		t.Fatal(err)
	}

	defer os.RemoveAll(sockDir)

	sockPath := filepath.Join(sockDir, "s.sock")
	binPath := writeFakeSignalCLI(t, sockPath)

	mc := &messageCollector{}

	b, err := New(Config{
		BinaryPath: binPath,
		Account:    "+49111",
		SocketPath: sockPath,
		ConfigDir:  sockDir,
	}, mc.handler)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runDone := make(chan error, 1)

	go func() { runDone <- b.Run(ctx) }()

	// Give the fake daemon time to start and the backend to connect + subscribe.
	time.Sleep(3 * time.Second)

	// Verify the backend connected successfully by checking RPC is set.
	if b.getRPC() == nil {
		t.Fatal("backend did not establish RPC connection")
	}

	cancel()

	select {
	case err := <-runDone:
		if err != nil && !strings.Contains(err.Error(), "context") {
			t.Logf("Run() error (may be expected): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not exit after cancel")
	}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()

	_, err := New(Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "account") {
		t.Fatalf("expected account error, got %v", err)
	}

	_, err = New(Config{Account: "+49111"}, nil)
	if err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("expected socket error, got %v", err)
	}

	b, err := New(Config{Account: "+49111", ConfigDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if b.cfg.BinaryPath != "signal-cli" {
		t.Errorf("BinaryPath = %q, want signal-cli", b.cfg.BinaryPath)
	}

	if !strings.HasSuffix(b.cfg.SocketPath, "opencrow-jsonrpc.sock") {
		t.Errorf("SocketPath = %q", b.cfg.SocketPath)
	}
}

func TestSystemPromptExtra(t *testing.T) {
	t.Parallel()

	b, err := New(Config{Account: "+49111", ConfigDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	extra := b.SystemPromptExtra()
	if !strings.Contains(extra, "Signal") {
		t.Errorf("missing Signal mention in %q", extra)
	}

	if !strings.Contains(extra, "<sendfile>") {
		t.Errorf("missing sendfile mention in %q", extra)
	}
}

// marshalParams round-trips RPC params through JSON to get a map.
func marshalParams(t *testing.T, req rpcRequest) map[string]any {
	t.Helper()

	raw, err := json.Marshal(req.Params)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	return m
}

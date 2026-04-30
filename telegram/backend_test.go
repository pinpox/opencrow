package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinpox/opencrow/backend"
)

// fakeAPI is an httptest server that replays Telegram Bot API JSON envelopes.
type fakeAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	updates  []update
	sendLog  []map[string]any
	files    map[string][]byte // path -> body for /file/bot<token>/<path>
	tokenSeg string

	// failHTML, when true, makes sendMessage fail the first call with
	// parse_mode=HTML and succeed when parse_mode is absent — exercising
	// the plain-text fallback path.
	failHTML bool

	called atomic.Int32
}

func newFakeAPI(t *testing.T, token string) *fakeAPI {
	t.Helper()

	f := &fakeAPI{
		tokenSeg: "/bot" + token + "/",
		files:    map[string][]byte{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.srv = httptest.NewServer(mux)

	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.called.Add(1)

	// File downloads.
	if strings.HasPrefix(r.URL.Path, "/file/bot") {
		f.mu.Lock()
		body, ok := f.files[r.URL.Path]
		f.mu.Unlock()

		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write(body)

		return
	}

	method := strings.TrimPrefix(r.URL.Path, f.tokenSeg)

	body, _ := io.ReadAll(r.Body)

	switch method {
	case "getUpdates":
		f.mu.Lock()
		updates := f.updates
		f.updates = nil
		f.mu.Unlock()

		writeOK(w, updates)

	case "sendMessage":
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)

		f.mu.Lock()
		f.sendLog = append(f.sendLog, payload)
		failHTML := f.failHTML && payload["parse_mode"] == "HTML"
		f.mu.Unlock()

		if failHTML {
			writeErr(w, "Bad Request: can't parse entities")

			return
		}

		writeOK(w, map[string]any{"message_id": 42})

	case "sendChatAction":
		writeOK(w, true)

	case "getFile":
		var payload map[string]string
		_ = json.Unmarshal(body, &payload)
		writeOK(w, map[string]any{
			"file_id":   payload["file_id"],
			"file_path": "documents/" + payload["file_id"] + ".bin",
		})

	default:
		writeOK(w, nil)
	}
}

func (f *fakeAPI) addUpdate(u update) {
	f.mu.Lock()
	f.updates = append(f.updates, u)
	f.mu.Unlock()
}

func (f *fakeAPI) addFile(path string, body []byte) {
	f.mu.Lock()
	f.files[path] = body
	f.mu.Unlock()
}

func writeOK(w http.ResponseWriter, result any) {
	resp := map[string]any{"ok": true, "result": result}
	_ = json.NewEncoder(w).Encode(resp)
}

func writeErr(w http.ResponseWriter, description string) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": description})
}

func TestBackend_New_RejectsEmptyToken(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{}, nil); err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestBackend_SendMessage(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, "tok")

	b, err := New(Config{Token: "tok", APIBase: api.srv.URL}, func(context.Context, backend.Message) {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := b.SendMessage(context.Background(), "100", "hello", "55")
	if id != "42" {
		t.Errorf("message id = %q, want 42", id)
	}

	api.mu.Lock()
	defer api.mu.Unlock()

	if len(api.sendLog) != 1 {
		t.Fatalf("expected 1 send call, got %d", len(api.sendLog))
	}

	got := api.sendLog[0]
	if got["text"] != "hello" {
		t.Errorf("text = %v, want hello", got["text"])
	}

	// JSON unmarshals numbers as float64.
	if got["chat_id"].(float64) != 100 {
		t.Errorf("chat_id = %v, want 100", got["chat_id"])
	}

	rp, ok := got["reply_parameters"].(map[string]any)
	if !ok {
		t.Fatalf("reply_parameters missing or wrong type: %v", got["reply_parameters"])
	}

	if rp["message_id"].(float64) != 55 {
		t.Errorf("reply_parameters.message_id = %v, want 55", rp["message_id"])
	}
}

func TestBackend_SendMessage_RendersMarkdownAsHTML(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, "tok")

	b, err := New(Config{Token: "tok", APIBase: api.srv.URL}, func(context.Context, backend.Message) {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if id := b.SendMessage(context.Background(), "100", "this is **bold** and `code`", ""); id == "" {
		t.Fatal("expected non-empty id")
	}

	api.mu.Lock()
	defer api.mu.Unlock()

	if len(api.sendLog) != 1 {
		t.Fatalf("expected 1 send call, got %d", len(api.sendLog))
	}

	got := api.sendLog[0]

	if got["parse_mode"] != "HTML" {
		t.Errorf("parse_mode = %v, want HTML", got["parse_mode"])
	}

	want := "this is <b>bold</b> and <code>code</code>"
	if got["text"] != want {
		t.Errorf("text = %q, want %q", got["text"], want)
	}
}

func TestBackend_SendMessage_FallsBackToPlainOnHTMLError(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, "tok")
	api.failHTML = true

	b, err := New(Config{Token: "tok", APIBase: api.srv.URL}, func(context.Context, backend.Message) {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if id := b.SendMessage(context.Background(), "100", "weird **broken markdown", ""); id == "" {
		t.Fatal("expected fallback to succeed")
	}

	api.mu.Lock()
	defer api.mu.Unlock()

	if len(api.sendLog) != 2 {
		t.Fatalf("expected 2 send calls (HTML attempt + plain fallback), got %d", len(api.sendLog))
	}

	if api.sendLog[1]["parse_mode"] != nil {
		t.Errorf("fallback should not set parse_mode, got %v", api.sendLog[1]["parse_mode"])
	}

	if api.sendLog[1]["text"] != "weird **broken markdown" {
		t.Errorf("fallback text = %q, want raw original", api.sendLog[1]["text"])
	}
}

func TestBackend_SendMessage_EmptyTextSkipsCall(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, "tok")

	b, err := New(Config{Token: "tok", APIBase: api.srv.URL}, func(context.Context, backend.Message) {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if id := b.SendMessage(context.Background(), "100", "   \n", ""); id != "" {
		t.Errorf("expected empty id for whitespace-only text, got %q", id)
	}

	if api.called.Load() != 0 {
		t.Errorf("expected no API calls for whitespace text, got %d", api.called.Load())
	}
}

func TestBackend_RunDeliversMessage(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, "tok")
	api.addUpdate(update{
		UpdateID: 7,
		Message: &message{
			MessageID: 11,
			From:      &user{ID: 123, Username: "alice"},
			Chat:      chat{ID: 456, Type: "private"},
			Text:      "hi",
		},
	})

	received := make(chan backend.Message, 1)

	b, err := New(Config{
		Token:       "tok",
		APIBase:     api.srv.URL,
		PollTimeout: time.Second,
	}, func(_ context.Context, m backend.Message) {
		received <- m
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = b.Run(ctx) }()

	select {
	case msg := <-received:
		if msg.ConversationID != "456" {
			t.Errorf("ConversationID = %q, want 456", msg.ConversationID)
		}

		if msg.SenderID != "123" {
			t.Errorf("SenderID = %q, want 123", msg.SenderID)
		}

		if msg.Text != "hi" {
			t.Errorf("Text = %q, want hi", msg.Text)
		}

		if msg.MessageID != "11" {
			t.Errorf("MessageID = %q, want 11", msg.MessageID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive message within 3s")
	}

	b.Stop()
}

func TestBackend_AllowlistDropsForeignSender(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, "tok")
	api.addUpdate(update{
		UpdateID: 7,
		Message: &message{
			MessageID: 11,
			From:      &user{ID: 999, Username: "stranger"},
			Chat:      chat{ID: 456},
			Text:      "hi",
		},
	})

	received := make(chan backend.Message, 1)

	b, err := New(Config{
		Token:        "tok",
		APIBase:      api.srv.URL,
		PollTimeout:  time.Second,
		AllowedUsers: map[string]struct{}{"123": {}},
	}, func(_ context.Context, m backend.Message) {
		received <- m
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = b.Run(ctx) }()

	select {
	case msg := <-received:
		t.Fatalf("received message from non-allowed sender: %+v", msg)
	case <-time.After(500 * time.Millisecond):
		// expected: no delivery
	}

	b.Stop()
}

func TestBackend_DownloadAttachment(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, "tok")
	api.addFile("/file/bottok/documents/file-xyz.bin", []byte("BINARY"))
	api.addUpdate(update{
		UpdateID: 8,
		Message: &message{
			MessageID: 12,
			From:      &user{ID: 1},
			Chat:      chat{ID: 2},
			Document:  &document{FileID: "file-xyz", FileName: "report.pdf"},
		},
	})

	received := make(chan backend.Message, 1)

	dir := t.TempDir()

	b, err := New(Config{
		Token:          "tok",
		APIBase:        api.srv.URL,
		SessionBaseDir: dir,
		PollTimeout:    time.Second,
	}, func(_ context.Context, m backend.Message) {
		received <- m
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = b.Run(ctx) }()

	select {
	case msg := <-received:
		if !strings.Contains(msg.Text, "[User sent a file") {
			t.Errorf("expected attachment marker in text, got %q", msg.Text)
		}

		if !strings.Contains(msg.Text, filepath.Join(dir, "attachments")) {
			t.Errorf("expected path under attachments dir, got %q", msg.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive attachment message within 3s")
	}

	b.Stop()
}

func TestBackend_MarkdownFlavor(t *testing.T) {
	t.Parallel()

	b, err := New(Config{Token: "x"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := b.MarkdownFlavor(); got != backend.MarkdownFull {
		t.Errorf("flavor = %d, want %d", got, backend.MarkdownFull)
	}
}

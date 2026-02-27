package nostr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/pinpox/opencrow/backend"
)

func TestSendFile_UploadsToBlossom(t *testing.T) {
	t.Parallel()

	var receivedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/upload" {
			http.Error(w, "not found", http.StatusNotFound)

			return
		}

		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://blossom.example.com/abc123"})
	}))
	defer srv.Close()

	botSK := gonostr.Generate()

	b, err := NewBackend(Config{
		PrivateKey:     botSK.Hex(),
		Relays:         []string{},
		BlossomServers: []string{srv.URL},
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: t.TempDir(),
	}, func(context.Context, backend.Message) {})
	if err != nil {
		t.Fatal(err)
	}

	// Create a test file
	tmpFile := filepath.Join(t.TempDir(), "test.png")
	if err := os.WriteFile(tmpFile, []byte("fake image data"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url, err := b.uploadToBlossomImpl(ctx, tmpFile)
	if err != nil {
		t.Fatalf("uploadToBlossom: %v", err)
	}

	if url != "https://blossom.example.com/abc123" {
		t.Errorf("url = %q, want %q", url, "https://blossom.example.com/abc123")
	}

	if string(receivedBody) != "fake image data" {
		t.Errorf("server received %q, want %q", receivedBody, "fake image data")
	}
}

func TestSendFile_BlossomFallback(t *testing.T) {
	t.Parallel()

	// First server fails
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv1.Close()

	// Second server succeeds
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://blossom2.example.com/def456"})
	}))
	defer srv2.Close()

	botSK := gonostr.Generate()

	b, err := NewBackend(Config{
		PrivateKey:     botSK.Hex(),
		Relays:         []string{},
		BlossomServers: []string{srv1.URL, srv2.URL},
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: t.TempDir(),
	}, func(context.Context, backend.Message) {})
	if err != nil {
		t.Fatal(err)
	}

	tmpFile := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(tmpFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url, err := b.uploadToBlossomImpl(ctx, tmpFile)
	if err != nil {
		t.Fatalf("uploadToBlossom: %v", err)
	}

	if url != "https://blossom2.example.com/def456" {
		t.Errorf("url = %q, want fallback URL", url)
	}
}

func TestReceive_URLAttachmentDownload(t *testing.T) {
	t.Parallel()

	// Start a test HTTP server serving an image
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fake png data"))
	}))
	defer srv.Close()

	sessionDir := t.TempDir()
	conversationID := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	ctx := context.Background()

	localPath, err := downloadURL(ctx, srv.URL+"/photo.png", sessionDir, conversationID)
	if err != nil {
		t.Fatalf("downloadURL: %v", err)
	}

	// Verify file exists
	data, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}

	if string(data) != "fake png data" {
		t.Errorf("file content = %q, want %q", data, "fake png data")
	}

	// Verify path is under per-conversation attachments dir
	wantDir := filepath.Join(sessionDir, conversationID, "attachments")
	if filepath.Dir(localPath) != wantDir {
		t.Errorf("file dir = %q, want %q", filepath.Dir(localPath), wantDir)
	}
}

func TestDownloadURL_ExceedsMaxSize(t *testing.T) {
	t.Parallel()

	// Serve a response larger than maxDownloadSize.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		// Write maxDownloadSize + 1 bytes to trigger the limit.
		buf := make([]byte, 32*1024)

		written := 0
		for written <= maxDownloadSize {
			n := len(buf)
			if written+n > maxDownloadSize+1 {
				n = maxDownloadSize + 1 - written
			}

			_, _ = w.Write(buf[:n])
			written += n
		}
	}))
	defer srv.Close()

	sessionDir := t.TempDir()
	conversationID := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	ctx := context.Background()

	_, err := downloadURL(ctx, srv.URL+"/huge.bin", sessionDir, conversationID)
	if err == nil {
		t.Fatal("expected error for oversized download, got nil")
	}

	if got := err.Error(); got != "download exceeds maximum size of 52428800 bytes" {
		t.Errorf("unexpected error: %s", got)
	}

	// Verify the oversized file was cleaned up.
	entries, _ := os.ReadDir(filepath.Join(sessionDir, "attachments"))
	if len(entries) != 0 {
		t.Errorf("expected attachments dir to be empty after cleanup, got %d files", len(entries))
	}
}

func TestSendFile_AllBlossomsFail(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	botSK := gonostr.Generate()

	b, err := NewBackend(Config{
		PrivateKey:     botSK.Hex(),
		Relays:         []string{},
		BlossomServers: []string{srv.URL},
		AllowedUsers:   make(map[string]struct{}),
		SessionBaseDir: t.TempDir(),
	}, func(context.Context, backend.Message) {})
	if err != nil {
		t.Fatal(err)
	}

	tmpFile := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(tmpFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = b.uploadToBlossomImpl(ctx, tmpFile)
	if err == nil {
		t.Fatal("expected error when all servers fail")
	}
}

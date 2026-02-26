package nostr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gonostr "fiatjaf.com/nostr"
)

// uploadToBlossom uploads a file to the configured Blossom servers with fallback.
// Returns the URL on success.
func (b *Backend) uploadToBlossomImpl(ctx context.Context, filePath string) (string, error) {
	if len(b.cfg.BlossomServers) == 0 {
		return "", fmt.Errorf("no blossom servers configured")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}

	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])

	contentType := http.DetectContentType(data)

	// Build kind 24242 auth event
	authEvt, err := buildBlossomAuthEvent(hashHex, b.keys)
	if err != nil {
		return "", fmt.Errorf("building auth event: %w", err)
	}

	evtJSON, err := json.Marshal(authEvt)
	if err != nil {
		return "", fmt.Errorf("marshaling auth event: %w", err)
	}
	authHeader := "Nostr " + base64.StdEncoding.EncodeToString(evtJSON)

	// Try each server in order
	var lastErr error
	for _, server := range b.cfg.BlossomServers {
		url, err := uploadToServer(ctx, server, data, contentType, authHeader, hashHex)
		if err != nil {
			slog.Warn("nostr: blossom upload failed", "server", server, "error", err)
			lastErr = err
			continue
		}
		slog.Info("nostr: uploaded to blossom", "server", server, "url", url)
		return url, nil
	}

	return "", fmt.Errorf("all blossom servers failed, last error: %w", lastErr)
}

func uploadToServer(ctx context.Context, server string, data []byte, contentType, authHeader, hashHex string) (string, error) {
	uploadURL := strings.TrimRight(server, "/") + "/upload"

	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", contentType)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var respData struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &respData); err != nil || respData.URL == "" {
		respData.URL = strings.TrimRight(server, "/") + "/" + hashHex
	}

	return respData.URL, nil
}

func buildBlossomAuthEvent(hashHex string, keys Keys) (gonostr.Event, error) {
	expiration := time.Now().Add(5 * time.Minute).Unix()
	evt := gonostr.Event{
		Kind:      24242,
		CreatedAt: gonostr.Now(),
		Tags: gonostr.Tags{
			{"t", "upload"},
			{"x", hashHex},
			{"expiration", fmt.Sprintf("%d", expiration)},
		},
	}
	if err := evt.Sign(keys.SK); err != nil {
		return evt, err
	}
	return evt, nil
}

// downloadURL downloads a URL to the per-conversation attachments dir.
// Returns the local file path.
func downloadURL(ctx context.Context, rawURL, sessionBaseDir, conversationID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	// Extract filename from URL, stripping query string and unsafe characters.
	filename := "attachment"
	if parsed, parseErr := url.Parse(rawURL); parseErr == nil {
		filename = path.Base(parsed.Path)
	}
	filename = sanitizeFilename(filename)

	downloadDir := filepath.Join(sessionBaseDir, "attachments")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return "", fmt.Errorf("creating attachments dir: %w", err)
	}

	// Use os.CreateTemp for atomic unique file creation — avoids collisions
	// from concurrent downloads of the same filename.
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	pattern := base + "_*" + ext
	f, err := os.CreateTemp(downloadDir, pattern)
	if err != nil {
		return "", fmt.Errorf("creating file: %w", err)
	}
	defer f.Close()
	destPath := f.Name()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}

	return destPath, nil
}

// unsafeFilenameChars matches characters that are unsafe in filenames across
// common filesystems (NTFS, ext4, HFS+, etc.).
var unsafeFilenameChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// sanitizeFilename removes filesystem-unsafe characters, trims the result,
// caps length, and falls back to "attachment" if nothing useful remains.
func sanitizeFilename(name string) string {
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.TrimSpace(name)

	// Cap at a reasonable length to avoid filesystem limits.
	const maxLen = 200
	if len(name) > maxLen {
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		if len(base) > maxLen-len(ext) {
			base = base[:maxLen-len(ext)]
		}
		name = base + ext
	}

	if name == "" || name == "." || name == "/" {
		name = "attachment"
	}
	return name
}

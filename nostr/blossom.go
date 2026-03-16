package nostr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	gonostr "fiatjaf.com/nostr"
)

// blossomUpload holds the result of an encrypted file upload to Blossom.
type blossomUpload struct {
	URL          string         // Blossom URL of the ciphertext blob
	MIMEType     string         // MIME type of the original plaintext file
	XHex         string         // SHA-256 of the ciphertext (NIP-17 "x" tag)
	Enc          *encryptedFile // encryption parameters for the recipient
}

// uploadToBlossomImpl encrypts a file with AES-256-GCM and uploads the
// ciphertext to the configured Blossom servers. The plaintext never leaves
// the machine; only the recipient (who receives the key inside the encrypted
// kind 15 rumor) can decrypt it.
func (b *Backend) uploadToBlossomImpl(ctx context.Context, filePath string) (*blossomUpload, error) {
	if len(b.cfg.BlossomServers) == 0 {
		return nil, errors.New("no blossom servers configured")
	}

	plaintext, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	mimeType := detectContentType(filePath, plaintext)

	enc, err := encryptFileForUpload(plaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypting file: %w", err)
	}

	hash := sha256.Sum256(enc.Ciphertext)
	hashHex := hex.EncodeToString(hash[:])

	// Build kind 24242 auth event
	authEvt, err := buildBlossomAuthEvent(hashHex, b.keys)
	if err != nil {
		return nil, fmt.Errorf("building auth event: %w", err)
	}

	evtJSON, err := json.Marshal(authEvt)
	if err != nil {
		return nil, fmt.Errorf("marshaling auth event: %w", err)
	}

	authHeader := "Nostr " + base64.StdEncoding.EncodeToString(evtJSON)

	// Try each server in order
	var lastErr error

	for _, server := range b.cfg.BlossomServers {
		url, err := uploadToServer(ctx, server, enc.Ciphertext, mimeType, authHeader, hashHex)
		if err != nil {
			slog.Warn("nostr: blossom upload failed", "server", server, "error", err)
			lastErr = err

			continue
		}

		slog.Info("nostr: uploaded to blossom", "server", server, "url", url)

		return &blossomUpload{URL: url, MIMEType: mimeType, XHex: hashHex, Enc: enc}, nil
	}

	return nil, fmt.Errorf("all blossom servers failed, last error: %w", lastErr)
}

// detectContentType determines the MIME type for a file from its extension,
// falling back to http.DetectContentType for byte sniffing.
func detectContentType(filePath string, data []byte) string {
	if ext := filepath.Ext(filePath); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}

	return http.DetectContentType(data)
}

func uploadToServer(ctx context.Context, server string, data []byte, contentType, authHeader, hashHex string) (string, error) {
	uploadURL := strings.TrimRight(server, "/") + "/upload"

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("creating upload request: %w", err)
	}

	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", contentType)

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("uploading to %s: %w", server, err)
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
			{"expiration", strconv.FormatInt(expiration, 10)},
		},
	}
	if err := evt.Sign(keys.SK); err != nil {
		return evt, fmt.Errorf("signing blossom auth event: %w", err)
	}

	return evt, nil
}

// maxDownloadSize caps the amount of data downloadURL will save to disk.
// 50 MiB is generous for images and voice memos while preventing abuse from
// multi-gigabyte payloads that could exhaust disk space or memory.
const maxDownloadSize = 50 << 20 // 50 MiB

// downloadURL downloads a URL to the per-conversation attachments dir.
// Returns the local file path.
func downloadURL(ctx context.Context, rawURL, sessionBaseDir, conversationID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
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

	downloadDir := filepath.Join(sessionBaseDir, conversationID, "attachments")
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

	// Limit download size to prevent disk exhaustion from oversized payloads.
	limited := io.LimitReader(resp.Body, maxDownloadSize+1)

	n, err := io.Copy(f, limited)
	if err != nil {
		// Remove the partial file so failed downloads don't leak on disk.
		f.Close()
		os.Remove(destPath)

		return "", fmt.Errorf("writing file: %w", err)
	}

	if n > maxDownloadSize {
		f.Close()
		os.Remove(destPath)

		return "", fmt.Errorf("download exceeds maximum size of %d bytes", maxDownloadSize)
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

package nostr

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	gonostr "fiatjaf.com/nostr"
)

// KindFileMessage is the NIP-17 kind for file messages (not yet in go-nostr).
const KindFileMessage gonostr.Kind = 15

var mediaURLRe = regexp.MustCompile(`(?i)https?://\S+\.(?:png|jpg|jpeg|gif|webp|pdf|mp3|mp4|wav|ogg|svg|bmp|tiff|zip)(?:\?\S*)?`)

// handleFileMessage processes a NIP-17 kind 15 file message. The rumor's
// content is the file URL and tags carry metadata (file-type, dimensions, etc.).
// The file is downloaded and the message text is rewritten to reference the
// local path so pi can read it with its tools.
func (b *Backend) handleFileMessage(ctx context.Context, rumor gonostr.Event, conversationID string) string {
	fileURL := strings.TrimSpace(rumor.Content)
	if fileURL == "" {
		slog.Warn("nostr: kind 15 file message with empty content", "sender", conversationID)

		return "[User sent a file message with no URL]"
	}

	mimeType := tagValue(rumor.Tags, "file-type")

	slog.Info("nostr: downloading file message", "url", fileURL, "mime", mimeType, "sender", conversationID)

	localPath, err := downloadURL(ctx, fileURL, b.cfg.SessionBaseDir, conversationID)
	if err != nil {
		slog.Warn("nostr: failed to download file message", "url", fileURL, "error", err)

		return fmt.Sprintf("[User sent a file but download failed: %s]", fileURL)
	}

	// Decrypt if the file was encrypted (NIP-17 kind 15 AES-GCM).
	if algo := tagValue(rumor.Tags, "encryption-algorithm"); algo != "" {
		if err := decryptFileInPlace(localPath, rumor.Tags); err != nil {
			slog.Warn("nostr: failed to decrypt file message", "url", fileURL, "error", err)
			os.Remove(localPath)

			return "[User sent an encrypted file but decryption failed]"
		}
	}

	// If the downloaded file has no extension, try to add one from the mime type.
	localPath = maybeAddExtension(localPath, mimeType)

	desc := descFromMIME(mimeType)

	return fmt.Sprintf("[User sent a %s: %s]\nUse the read tool to view it.", desc, localPath)
}

// maybeAddExtension renames a file to include a proper extension when the
// downloaded filename has none and the mime type is known.
func maybeAddExtension(path string, mimeType string) string {
	if mimeType == "" || filepath.Ext(path) != "" {
		return path
	}

	ext := mimeToExt(mimeType)
	if ext == "" {
		return path
	}

	newPath := path + ext
	if err := os.Rename(path, newPath); err != nil {
		slog.Warn("nostr: failed to rename file with extension", "from", path, "to", newPath, "error", err)

		return path
	}

	return newPath
}

// descFromMIME returns a human-readable file type description for the
// attachment message text.
func descFromMIME(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	default:
		return "file"
	}
}

// mimeToExt returns a file extension for a MIME type using the system MIME
// database (via mime.ExtensionsByType). Returns "" for unknown types.
func mimeToExt(mimeType string) string {
	exts, err := mime.ExtensionsByType(mimeType)
	if err != nil || len(exts) == 0 {
		return ""
	}

	return exts[0]
}

// rewriteMediaURLs finds media URLs in text, downloads them, and rewrites the
// text with the local file path in the standard attachment format.
func (b *Backend) rewriteMediaURLs(ctx context.Context, text, conversationID string) string {
	urls := mediaURLRe.FindAllString(text, -1)
	if len(urls) == 0 {
		return text
	}

	processed := make(map[string]bool, len(urls))
	for _, rawURL := range urls {
		if processed[rawURL] {
			continue
		}

		localPath, err := downloadURL(ctx, rawURL, b.cfg.SessionBaseDir, conversationID)
		if err != nil {
			slog.Warn("nostr: failed to download media URL", "url", rawURL, "error", err)

			continue
		}

		slog.Debug("nostr: downloaded media URL", "url", rawURL, "localPath", localPath)
		processed[rawURL] = true
		replacement := fmt.Sprintf("[User sent a file (no caption): %s]\nUse the read tool to view it.", localPath)
		text = strings.ReplaceAll(text, rawURL, replacement)
	}

	return text
}

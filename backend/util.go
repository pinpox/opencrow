package backend

import "fmt"

// AttachmentText returns the canonical "[User sent a file ...]" line that
// backends inject into the conversation when an attachment is received.
// caption may be empty; path may be empty if the file could not be
// resolved to a local filesystem location.
func AttachmentText(caption, path string) string {
	if caption == "" {
		caption = "no caption"
	}

	if path == "" {
		return fmt.Sprintf("[User sent a file (%s)]", caption)
	}

	return fmt.Sprintf("[User sent a file (%s): %s]\nUse the read tool to view it.", caption, path)
}

// IsAllowed reports whether senderID is permitted by the allowlist.
// An empty or nil allowlist permits all senders.
func IsAllowed(allowlist map[string]struct{}, senderID string) bool {
	if len(allowlist) == 0 {
		return true
	}

	_, ok := allowlist[senderID]

	return ok
}

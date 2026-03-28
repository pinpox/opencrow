package backend

import (
	"fmt"
	"sync"
)

// ActiveConversation tracks a single active conversation ID. Backends that
// only serve one conversation at a time embed this to share the claim/reset
// logic instead of reimplementing the same mutex-guarded string.
type ActiveConversation struct {
	mu sync.Mutex
	id string
}

// Claim attempts to set id as the active conversation. It succeeds if no
// conversation is active yet or if id is already the active one. Returns
// false when a different conversation is already claimed.
func (a *ActiveConversation) Claim(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.id == "" {
		a.id = id

		return true
	}

	return a.id == id
}

// Reset clears the active conversation if it matches id.
func (a *ActiveConversation) Reset(id string) {
	a.mu.Lock()
	if a.id == id {
		a.id = ""
	}
	a.mu.Unlock()
}

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

// Canceler stores a context.CancelFunc behind a mutex so Run can install
// it and Stop can fire it from another goroutine. Backends that implement
// Stop by cancelling Run's context embed this instead of each carrying
// their own mutex/func pair.
type Canceler struct {
	mu sync.Mutex
	fn func()
}

// Set installs fn as the cancel function.
func (c *Canceler) Set(fn func()) {
	c.mu.Lock()
	c.fn = fn
	c.mu.Unlock()
}

// Cancel invokes the stored function if present.
func (c *Canceler) Cancel() {
	c.mu.Lock()
	fn := c.fn
	c.mu.Unlock()

	if fn != nil {
		fn()
	}
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

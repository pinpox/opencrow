// Package backend defines the messaging transport interface that all
// backends (Matrix, Nostr, etc.) must implement.
package backend

import "context"

// Backend abstracts a messaging transport. Each method is documented with
// its expected semantics so implementations stay consistent.
type Backend interface {
	// Run starts the event loop (blocking). Returns on ctx cancellation or fatal error.
	Run(ctx context.Context) error
	// Stop signals the backend to shut down gracefully.
	Stop()
	// Close releases resources (crypto DB, relay connections).
	Close() error
	// SendMessage sends a text message to a conversation. Errors are logged
	// but do not propagate — the caller treats message sending as
	// fire-and-forget.
	SendMessage(ctx context.Context, conversationID string, text string)
	// SendFile uploads and sends a file to a conversation.
	// Returns an error so the caller can append failure info to the reply.
	SendFile(ctx context.Context, conversationID string, filePath string) error
	// SetTyping sets the typing indicator for a conversation.
	SetTyping(ctx context.Context, conversationID string, typing bool)
	// ResetConversation clears backend-side state for a conversation
	// (e.g., active room tracking, active DM pubkey).
	ResetConversation(ctx context.Context, conversationID string)
	// SystemPromptExtra returns backend-specific text to append to the
	// system prompt.
	SystemPromptExtra() string
}

// Message represents a transport-agnostic inbound message.
type Message struct {
	ConversationID string // room ID, DM pubkey, channel ID — opaque to the core
	SenderID       string // user ID / pubkey
	Text           string // message text (or synthesized "[User sent file: ...]")
}

// MessageHandler is a callback invoked by the backend for each inbound user message.
type MessageHandler func(ctx context.Context, msg Message)

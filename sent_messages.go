package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const (
	maxSentMessagesPerConversation = 100
	sentMessagesFile               = ".sent_messages.json"
)

// sentMessageStore tracks outgoing message text keyed by backend-specific
// message ID, so we can resolve reply-to references from users. Entries are
// per-conversation and bounded to avoid unbounded growth. The store is
// persisted to disk so reply context survives bot restarts.
type sentMessageStore struct {
	mu      sync.Mutex
	store   map[string]*conversationMessages // conversationID → messages
	dataDir string                           // directory for the JSON file
}

type conversationMessages struct {
	ByID  map[string]string `json:"by_id"`
	Order []string          `json:"order"`
}

func newSentMessageStore(dataDir string) *sentMessageStore {
	s := &sentMessageStore{
		store:   make(map[string]*conversationMessages),
		dataDir: dataDir,
	}

	s.load()

	return s
}

// Put records a sent message. If the conversation exceeds the max, the
// oldest entry is evicted. The store is persisted after each mutation.
func (s *sentMessageStore) Put(conversationID, messageID, text string) {
	if messageID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cm, ok := s.store[conversationID]
	if !ok {
		cm = &conversationMessages{
			ByID: make(map[string]string),
		}
		s.store[conversationID] = cm
	}

	if _, exists := cm.ByID[messageID]; !exists {
		cm.Order = append(cm.Order, messageID)
	}

	cm.ByID[messageID] = text

	for len(cm.Order) > maxSentMessagesPerConversation {
		oldest := cm.Order[0]
		cm.Order = cm.Order[1:]
		delete(cm.ByID, oldest)
	}

	s.saveLocked()
}

// Get returns the text of a previously sent message, or "" if unknown.
func (s *sentMessageStore) Get(conversationID, messageID string) string {
	if messageID == "" {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cm, ok := s.store[conversationID]
	if !ok {
		return ""
	}

	return cm.ByID[messageID]
}

func (s *sentMessageStore) load() {
	if s.dataDir == "" {
		return
	}

	data, err := os.ReadFile(filepath.Join(s.dataDir, sentMessagesFile))
	if err != nil {
		return
	}

	if err := json.Unmarshal(data, &s.store); err != nil {
		slog.Warn("failed to parse sent messages store", "error", err)
	}
}

func (s *sentMessageStore) saveLocked() {
	if s.dataDir == "" {
		return
	}

	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		slog.Warn("failed to create dir for sent messages", "error", err)

		return
	}

	data, err := json.Marshal(s.store)
	if err != nil {
		slog.Warn("failed to marshal sent messages", "error", err)

		return
	}

	dir := s.dataDir

	tmpFile, err := os.CreateTemp(dir, ".sent_messages_*.tmp")
	if err != nil {
		slog.Warn("failed to create temp file for sent messages", "error", err)

		return
	}

	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		slog.Warn("failed to write sent messages temp file", "error", err)

		return
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		slog.Warn("failed to close sent messages temp file", "error", err)

		return
	}

	finalPath := filepath.Join(dir, sentMessagesFile)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		slog.Warn("failed to rename sent messages into place", "error", err)
	}
}

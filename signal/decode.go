package signal

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pinpox/opencrow/backend"
)

func addRecipientParams(params map[string]any, conversationID string) {
	if groupID, ok := parseGroupConversationID(conversationID); ok {
		params["groupId"] = groupID

		return
	}

	params["recipient"] = []string{conversationID}
}

func parseGroupConversationID(conversationID string) (string, bool) {
	groupID, ok := strings.CutPrefix(conversationID, groupConversationPrefix)

	return groupID, ok && groupID != ""
}

type sendResult struct {
	Timestamp int64 `json:"timestamp"`
}

type receiveLine struct {
	Envelope *receiveEnvelope `json:"envelope"`
}

type receiveNotificationWrapped struct {
	Result *receiveLine `json:"result"`
}

type receiveEnvelope struct {
	Source       string          `json:"source"`
	SourceNumber string          `json:"sourceNumber"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
	SourceUUID   string          `json:"sourceUuid"`   //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
	Timestamp    int64           `json:"timestamp"`
	DataMessage  *receiveDataMsg `json:"dataMessage"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
}

type receiveDataMsg struct {
	Timestamp   int64               `json:"timestamp"`
	Message     string              `json:"message"`
	Quote       *receiveQuote       `json:"quote"`
	GroupInfo   *receiveGroupInfo   `json:"groupInfo"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
	Attachments []receiveAttachment `json:"attachments"`
}

type receiveQuote struct {
	ID int64 `json:"id"`
}

type receiveGroupInfo struct {
	GroupID string `json:"groupId"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
}

type receiveAttachment struct {
	Filename    string `json:"filename"`
	Caption     string `json:"caption"`
	ContentType string `json:"contentType"` //nolint:tagliatelle // signal-cli JSON uses camelCase keys.
	ID          string `json:"id"`
}

func decodeReceiveNotification(params json.RawMessage, configDir string) (*backend.Message, bool, error) {
	msg, ok, err := decodeReceiveMessage(params, configDir)
	if err != nil {
		return nil, false, err
	}

	if ok {
		return msg, true, nil
	}

	var wrapped receiveNotificationWrapped
	if err := json.Unmarshal(params, &wrapped); err != nil {
		return nil, false, fmt.Errorf("decoding wrapped signal receive payload: %w", err)
	}

	if wrapped.Result == nil {
		return nil, false, nil
	}

	inner, err := json.Marshal(wrapped.Result)
	if err != nil {
		return nil, false, fmt.Errorf("encoding wrapped receive payload: %w", err)
	}

	return decodeReceiveMessage(inner, configDir)
}

//nolint:cyclop // straightforward mapping from signal-cli JSON to backend message.
func decodeReceiveMessage(payload []byte, configDir string) (*backend.Message, bool, error) {
	var line receiveLine
	if err := json.Unmarshal(payload, &line); err != nil {
		return nil, false, fmt.Errorf("decoding signal receive payload: %w", err)
	}

	env := line.Envelope
	if env == nil || env.DataMessage == nil {
		return nil, false, nil
	}

	sender := firstNonEmpty(env.SourceNumber, env.SourceUUID, env.Source)
	if sender == "" {
		return nil, false, nil
	}

	conversationID := sender
	if env.DataMessage.GroupInfo != nil && env.DataMessage.GroupInfo.GroupID != "" {
		conversationID = groupConversationPrefix + env.DataMessage.GroupInfo.GroupID
	}

	text := strings.TrimSpace(env.DataMessage.Message)
	if attachmentText := formatAttachmentText(env.DataMessage.Attachments, configDir); attachmentText != "" {
		if text != "" {
			text += "\n"
		}

		text += attachmentText
	}

	if text == "" {
		return nil, false, nil
	}

	var messageID string
	if ts := cmp.Or(env.DataMessage.Timestamp, env.Timestamp); ts != 0 {
		messageID = strconv.FormatInt(ts, 10)
	}

	var replyTo string
	if env.DataMessage.Quote != nil && env.DataMessage.Quote.ID != 0 {
		replyTo = strconv.FormatInt(env.DataMessage.Quote.ID, 10)
	}

	return &backend.Message{
		ConversationID: conversationID,
		SenderID:       sender,
		Text:           text,
		MessageID:      messageID,
		ReplyToID:      replyTo,
	}, true, nil
}

func formatAttachmentText(attachments []receiveAttachment, configDir string) string {
	if len(attachments) == 0 {
		return ""
	}

	lines := make([]string, 0, len(attachments))

	for _, a := range attachments {
		caption := strings.TrimSpace(a.Caption)
		if caption == "" {
			filename := strings.TrimSpace(a.Filename)
			if filename != "" {
				caption = filepath.Base(filename)
			}
		}

		if caption == "" {
			caption = firstNonEmpty(a.ContentType, a.ID, "attachment")
		}

		filePath := resolveAttachmentPath(a.Filename, configDir)

		lines = append(lines, backend.AttachmentText(caption, filePath))
	}

	return strings.Join(lines, "\n")
}

func resolveAttachmentPath(path, configDir string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	if filepath.IsAbs(path) || configDir == "" {
		return path
	}

	return filepath.Join(configDir, path)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}

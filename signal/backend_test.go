//nolint:revive // package name intentionally mirrors backend name.
package signal

import (
	"encoding/json"
	"testing"
)

func TestDecodeReceiveMessage_DirectText(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"envelope":{"source":"+4911111","sourceNumber":"+4911111","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hello"}}}`)

	msg, ok, err := decodeReceiveMessage(payload, "")
	if err != nil {
		t.Fatalf("decodeReceiveMessage: %v", err)
	}

	if !ok || msg == nil {
		t.Fatal("msg is nil")
	}

	if msg.ConversationID != "+4911111" {
		t.Fatalf("ConversationID = %q, want +4911111", msg.ConversationID)
	}

	if msg.SenderID != "+4911111" {
		t.Fatalf("SenderID = %q, want +4911111", msg.SenderID)
	}

	if msg.Text != "hello" {
		t.Fatalf("Text = %q, want hello", msg.Text)
	}

	if msg.MessageID != "1700000000123" {
		t.Fatalf("MessageID = %q, want 1700000000123", msg.MessageID)
	}
}

func TestDecodeReceiveNotification_WrappedSubscription(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"subscription":0,"result":{"envelope":{"sourceNumber":"+4911111","timestamp":1700000000999,"dataMessage":{"timestamp":1700000001000,"message":"see file","quote":{"id":1699999999000},"groupInfo":{"groupId":"ABCD123="},"attachments":[{"filename":"attachments/abc.png","caption":"diagram"}]}}}}`)

	msg, ok, err := decodeReceiveNotification(json.RawMessage(payload), "/var/lib/signal-cli")
	if err != nil {
		t.Fatalf("decodeReceiveNotification: %v", err)
	}

	if !ok || msg == nil {
		t.Fatal("msg is nil")
	}

	if msg.ConversationID != "signal-group:ABCD123=" {
		t.Fatalf("ConversationID = %q, want signal-group:ABCD123=", msg.ConversationID)
	}

	if msg.ReplyToID != "1699999999000" {
		t.Fatalf("ReplyToID = %q, want 1699999999000", msg.ReplyToID)
	}

	wantText := "see file\n[User sent a file (diagram): /var/lib/signal-cli/attachments/abc.png]\nUse the read tool to view it."
	if msg.Text != wantText {
		t.Fatalf("Text = %q, want %q", msg.Text, wantText)
	}
}

func TestDecodeReceiveMessage_IgnoresNonDataMessages(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"envelope":{"sourceNumber":"+4911111","timestamp":1700000000123}}`)

	msg, ok, err := decodeReceiveMessage(payload, "")
	if err != nil {
		t.Fatalf("decodeReceiveMessage: %v", err)
	}

	if ok || msg != nil {
		t.Fatalf("expected ignored message, got %+v", msg)
	}
}

func TestAddRecipientParams_Group(t *testing.T) {
	t.Parallel()

	params := map[string]any{}
	addRecipientParams(params, "signal-group:ABCD=")

	if got := params["groupId"]; got != "ABCD=" {
		t.Fatalf("groupId = %#v, want ABCD=", got)
	}

	if _, ok := params["recipient"]; ok {
		t.Fatal("recipient should not be set for group message")
	}
}

func TestIsAllowed_AllowedUsers(t *testing.T) {
	t.Parallel()

	b := &Backend{allowedUsers: map[string]struct{}{"+491234": {}}}

	if !b.isAllowed("+491234") {
		t.Fatal("allowed user should be accepted")
	}

	if b.isAllowed("+499999") {
		t.Fatal("non-allowed user should be rejected")
	}
}

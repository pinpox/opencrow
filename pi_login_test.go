package main

import (
	"strings"
	"testing"
	"time"
)

// waitForMessage polls mb until a sent message contains want.
func waitForMessage(t *testing.T, mb *mockBackend, want string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		mb.mu.Lock()
		for _, m := range mb.sentMessages {
			if strings.Contains(m.text, want) {
				mb.mu.Unlock()

				return
			}
		}
		mb.mu.Unlock()

		time.Sleep(20 * time.Millisecond)
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()

	t.Fatalf("no message containing %q; sent: %+v", want, mb.sentMessages)
}

// TestWorker_LoginOverChat drives !login end to end against fake-pi's
// paste-code flow: the URL reaches the chat, the user's reply answers pi's
// input request, and the login state is released afterwards.
func TestWorker_LoginOverChat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer string
		want   string
	}{
		{"accepted", "good-code", "Logged in to anthropic."},
		{"rejected", "bad-code", "Login failed: login rejected: invalid code"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := newFakePiWorker(t)
			mb := &mockBackend{}
			w.SetBackend(mb)
			w.piCfg.Provider = "anthropic"

			go w.Run(t.Context())

			if w.AnswerLogin("stray") {
				t.Fatal("AnswerLogin accepted text with no login pending")
			}

			provider, err := w.StartLogin(t.Context(), "", testRoom)
			if err != nil || provider != "anthropic" {
				t.Fatalf("StartLogin = (%q, %v), want (anthropic, nil)", provider, err)
			}

			if _, err := w.StartLogin(t.Context(), "", testRoom); err == nil {
				t.Fatal("second StartLogin succeeded while a login is pending")
			}

			waitForMessage(t, mb, "https://auth.example/authorize")
			waitForMessage(t, mb, "Paste the authorization code:")

			if !w.AnswerLogin(tc.answer) {
				t.Fatal("AnswerLogin rejected the answer during a pending login")
			}

			waitForMessage(t, mb, tc.want)

			// The finished login no longer captures messages.
			deadline := time.Now().Add(5 * time.Second)
			for w.AnswerLogin("after") {
				if time.Now().After(deadline) {
					t.Fatal("login still pending after it finished")
				}

				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

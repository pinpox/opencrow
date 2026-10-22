package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// errLoginRejected marks a login that pi itself finished with a failure
// response; pi is idle again afterwards. Any other Login error leaves pi
// possibly still waiting on the OAuth flow.
var errLoginRejected = errors.New("login rejected")

// Login runs pi's OAuth login for provider over RPC. say posts a message to
// the user; answers carries the user's replies.
//
// pi announces the authorization URL as an open_url UI request, then asks
// for the authorization code or redirect URL with an input UI request: its
// loopback callback server runs on the bot's machine, which the user's
// browser cannot reach. The user's next reply answers that request.
func (p *PiProcess) Login(ctx context.Context, provider string, say func(string), answers <-chan string) error {
	if !p.IsAlive() {
		return errors.New("pi process is not alive")
	}

	if err := p.sendCommand(map[string]string{"type": "login", "providerId": provider}); err != nil {
		return err
	}

	flow := &loginFlow{p: p, say: say}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("login cancelled: %w", ctx.Err())

		case <-p.done:
			return errors.New("pi process exited")

		case parsed, open := <-p.events:
			if done, err := flow.handleParsed(parsed, open); done {
				return err
			}

		case answer := <-answers:
			if err := flow.handleAnswer(answer); err != nil {
				return err
			}
		}
	}
}

// loginFlow is the state of one Login call: the input request, if any,
// that the next answer fills.
type loginFlow struct {
	p       *PiProcess
	say     func(string)
	inputID string
}

// handleParsed unwraps a reader result: a closed channel or reader error
// ends the login, events go to handleEvent.
func (f *loginFlow) handleParsed(parsed rpcParsed, open bool) (bool, error) {
	if !open {
		return true, errors.New("pi process closed stdout (EOF)")
	}

	if parsed.err != nil {
		return true, parsed.err
	}

	return f.handleEvent(parsed.event)
}

// handleEvent processes one pi event. done reports the login response;
// err is its failure.
func (f *loginFlow) handleEvent(evt rpcEvent) (bool, error) {
	switch evt.Type {
	case rpcTypeResponse:
		if evt.Command != "login" {
			return false, nil
		}

		if evt.Success != nil && *evt.Success {
			return true, nil
		}

		return true, fmt.Errorf("%w: %s", errLoginRejected, evt.Error)

	case rpcTypeExtensionUIRequest:
		f.handleUIRequest(evt)
	}

	return false, nil
}

func (f *loginFlow) handleUIRequest(evt rpcEvent) {
	switch evt.Method {
	case "open_url":
		msg := "Open this link and sign in:\n\n" + evt.URL
		if evt.Instructions != "" {
			msg += "\n\n" + evt.Instructions
		}

		f.say(msg)
	case "input":
		f.inputID = evt.ID
		f.say(evt.Title)
	case "notify":
		var msg string
		if json.Unmarshal(evt.Message, &msg) == nil && msg != "" {
			f.say(msg)
		}
	case "cancel":
		if evt.TargetID == f.inputID {
			f.inputID = ""
		}
	default:
		f.p.autoRespondExtensionUI(evt)
	}
}

// handleAnswer answers the pending input request with a user reply. Only
// failing to write to pi is fatal.
func (f *loginFlow) handleAnswer(answer string) error {
	if f.inputID == "" {
		f.say("The login is not waiting for input yet.")

		return nil
	}

	if err := f.p.sendCommand(map[string]any{
		"type":  "extension_ui_response",
		"id":    f.inputID,
		"value": answer,
	}); err != nil {
		return err
	}

	f.inputID = ""

	return nil
}

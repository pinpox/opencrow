package socket

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"

	"github.com/pinpox/opencrow/backend"
)

// Test fixtures used across model tests.
const (
	testProvider   = "local"
	testModelID    = "qwen3"
	testAltModelID = "gpt-oss"
)

// stubModelService is a test double for the ModelService interface.
// SetModel is stateful: it updates the Active flag in `models` so a
// subsequent ListModels (e.g. from BroadcastModels after a successful
// switch) returns a list whose active marker matches what was just set.
type stubModelService struct {
	models     []backend.ModelInfo
	listErr    error
	setErr     error
	setCallArg struct {
		provider string
		modelID  string
	}
}

func (s *stubModelService) ListModels(_ context.Context) ([]backend.ModelInfo, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}

	return s.models, nil
}

func (s *stubModelService) SetModel(_ context.Context, provider, modelID string) (*backend.ModelInfo, error) {
	s.setCallArg.provider = provider
	s.setCallArg.modelID = modelID

	if s.setErr != nil {
		return nil, s.setErr
	}

	var active *backend.ModelInfo

	for i := range s.models {
		match := s.models[i].Provider == provider && s.models[i].ID == modelID
		s.models[i].Active = match

		if match {
			active = &s.models[i]
		}
	}

	if active != nil {
		return active, nil
	}

	return &backend.ModelInfo{Provider: provider, ID: modelID, ContextWindow: 4096, Active: true}, nil
}

func TestListModels_ReturnsModels(t *testing.T) {
	t.Parallel()

	svc := &stubModelService{
		models: []backend.ModelInfo{
			{Provider: testProvider, ID: testModelID, ContextWindow: 32768, Reasoning: false, Active: true},
			{Provider: testProvider, ID: testAltModelID, ContextWindow: 131072, Reasoning: true},
		},
	}

	_, sockPath, cancel := startBackend(t, nil, svc)
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	sendCommand(t, conn, command{Cmd: cmdListModels})

	ev := readEvent(t, conn)
	if ev.Kind != evModels {
		t.Fatalf("kind = %q, want %q", ev.Kind, evModels)
	}

	want := []backend.ModelInfo{
		{Provider: testProvider, ID: testModelID, ContextWindow: 32768, Reasoning: false, Active: true},
		{Provider: testProvider, ID: testAltModelID, ContextWindow: 131072, Reasoning: true},
	}

	if !reflect.DeepEqual(ev.Models, want) {
		t.Fatalf("models = %+v, want %+v", ev.Models, want)
	}
}

// TestListModels_ServiceErrorEmitsError: when the service rejects the
// request (pi failed to spawn, RPC error, etc.) the client gets an
// explicit error event so the UI can surface the failure.
func TestListModels_ServiceErrorEmitsError(t *testing.T) {
	t.Parallel()

	svc := &stubModelService{listErr: errors.New("pi crash")}

	_, sockPath, cancel := startBackend(t, nil, svc)
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	sendCommand(t, conn, command{Cmd: cmdListModels})

	ev := readEvent(t, conn)
	if ev.Kind != evError {
		t.Fatalf("kind = %q, want %q", ev.Kind, evError)
	}

	if ev.Text == "" {
		t.Fatal("error event should carry a non-empty message")
	}
}

// TestSetModel_ForwardsToService asserts the success path: the service
// is called with the requested provider+id, and the resulting broadcast
// carries the full model list with Active=true only on the matching
// entry. A single-entry partial update would leave clients that joined
// before list-models replied with a one-entry view; we explicitly want
// the full list so every dropdown can reconcile.
func TestSetModel_ForwardsToService(t *testing.T) {
	t.Parallel()

	svc := &stubModelService{
		models: []backend.ModelInfo{
			{Provider: testProvider, ID: testModelID, ContextWindow: 32768, Active: true},
			{Provider: testProvider, ID: testAltModelID, ContextWindow: 131072, Reasoning: true},
			{Provider: testProvider, ID: "smollm", ContextWindow: 4096},
		},
	}

	_, sockPath, cancel := startBackend(t, nil, svc)
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	// Switch to a model that is not currently active so the broadcast
	// has to flip the marker on two entries.
	sendCommand(t, conn, command{Cmd: cmdSetModel, Provider: testProvider, ModelID: testAltModelID})

	ev := readEvent(t, conn)
	if ev.Kind != evModels {
		t.Fatalf("kind = %q, want %q", ev.Kind, evModels)
	}

	if len(ev.Models) != len(svc.models) {
		t.Fatalf("broadcast carried %d models, want full list of %d: %+v",
			len(ev.Models), len(svc.models), ev.Models)
	}

	for _, m := range ev.Models {
		wantActive := m.Provider == testProvider && m.ID == testAltModelID
		if m.Active != wantActive {
			t.Errorf("model %s/%s: Active=%t, want %t", m.Provider, m.ID, m.Active, wantActive)
		}
	}

	if svc.setCallArg.provider != testProvider || svc.setCallArg.modelID != testAltModelID {
		t.Fatalf("service called with %+v, want provider=%s modelID=gpt-oss",
			svc.setCallArg, testProvider)
	}
}

// TestSetModel_ServiceErrorEmitsError: when the service rejects the
// requested model (unknown, invalid, pi RPC error) the requesting
// client gets an error event carrying the underlying message.
func TestSetModel_ServiceErrorEmitsError(t *testing.T) {
	t.Parallel()

	svc := &stubModelService{setErr: errors.New("model not found")}

	_, sockPath, cancel := startBackend(t, nil, svc)
	defer cancel()

	conn := dial(t, sockPath)
	defer conn.Close()

	sendCommand(t, conn, command{Cmd: cmdSetModel, Provider: testProvider, ModelID: "nonexistent"})

	ev := readEvent(t, conn)
	if ev.Kind != evError {
		t.Fatalf("kind = %q, want %q", ev.Kind, evError)
	}

	if ev.Text == "" {
		t.Fatal("error event should carry a non-empty message")
	}
}

// TestBroadcastModels_PushesToAllClients: BroadcastModels sends the
// current list to every connected client, not just one. Used by the
// worker after a fresh pi spawn so dropdowns sync without polling.
func TestBroadcastModels_PushesToAllClients(t *testing.T) {
	t.Parallel()

	svc := &stubModelService{
		models: []backend.ModelInfo{
			{Provider: testProvider, ID: testModelID, ContextWindow: 32768, Active: true},
			{Provider: testProvider, ID: "smollm"},
		},
	}

	b, sockPath, cancel := startBackend(t, nil, svc)
	defer cancel()

	// Two clients, both must observe the broadcast.
	conn1 := dialSynced(t, sockPath)
	defer conn1.Close()

	conn2 := dialSynced(t, sockPath)
	defer conn2.Close()

	b.BroadcastModels(t.Context())

	for i, conn := range []net.Conn{conn1, conn2} {
		ev := readEvent(t, conn)
		if ev.Kind != evModels {
			t.Fatalf("client %d: kind = %q, want %q", i, ev.Kind, evModels)
		}

		if len(ev.Models) != 2 {
			t.Fatalf("client %d: expected 2 models, got %+v", i, ev.Models)
		}
	}
}

// TestSetModel_BroadcastsToAllClients covers the multi-client GUI case:
// one client switches model, every other connected client receives the
// same full-list event so their dropdowns reconcile without a manual
// reopen. This is the whole point of going through BroadcastModels
// instead of pushing a one-entry partial update.
func TestSetModel_BroadcastsToAllClients(t *testing.T) {
	t.Parallel()

	svc := &stubModelService{
		models: []backend.ModelInfo{
			{Provider: testProvider, ID: testModelID, ContextWindow: 32768, Active: true},
			{Provider: testProvider, ID: testAltModelID, ContextWindow: 131072, Reasoning: true},
		},
	}

	_, sockPath, cancel := startBackend(t, nil, svc)
	defer cancel()

	conn1 := dialSynced(t, sockPath)
	defer conn1.Close()

	conn2 := dialSynced(t, sockPath)
	defer conn2.Close()

	// Switch from conn1; both conn1 and conn2 must observe the broadcast.
	sendCommand(t, conn1, command{Cmd: cmdSetModel, Provider: testProvider, ModelID: testAltModelID})

	for i, conn := range []net.Conn{conn1, conn2} {
		ev := readEvent(t, conn)
		if ev.Kind != evModels {
			t.Fatalf("client %d: kind = %q, want %q", i, ev.Kind, evModels)
		}

		if len(ev.Models) != len(svc.models) {
			t.Fatalf("client %d: expected full list of %d models, got %+v",
				i, len(svc.models), ev.Models)
		}

		for _, m := range ev.Models {
			wantActive := m.Provider == testProvider && m.ID == testAltModelID
			if m.Active != wantActive {
				t.Errorf("client %d: model %s/%s: Active=%t, want %t",
					i, m.Provider, m.ID, m.Active, wantActive)
			}
		}
	}
}

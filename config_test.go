package main

import (
	"os"
	"testing"
)

func TestBackendType_Default(t *testing.T) {
	clearConfigEnv(t)
	setRequiredMatrixEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.BackendType != "matrix" {
		t.Errorf("BackendType = %q, want %q", cfg.BackendType, "matrix")
	}
}

func TestBackendType_Unknown(t *testing.T) {
	clearConfigEnv(t)
	setRequiredMatrixEnv(t)
	t.Setenv("OPENCROW_BACKEND", "telegram")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for unknown backend, got nil")
	}
}

// clearConfigEnv unsets all OPENCROW env vars to get a clean test state.
func clearConfigEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"OPENCROW_BACKEND",
		"OPENCROW_MATRIX_HOMESERVER",
		"OPENCROW_MATRIX_USER_ID",
		"OPENCROW_MATRIX_ACCESS_TOKEN",
		"OPENCROW_MATRIX_DEVICE_ID",
		"OPENCROW_MATRIX_PICKLE_KEY",
		"OPENCROW_MATRIX_CRYPTO_DB",
		"OPENCROW_ALLOWED_USERS",
		"OPENCROW_PI_BINARY",
		"OPENCROW_PI_SESSION_DIR",
		"OPENCROW_PI_PROVIDER",
		"OPENCROW_PI_MODEL",
		"OPENCROW_PI_WORKING_DIR",
		"OPENCROW_PI_IDLE_TIMEOUT",
		"OPENCROW_PI_SYSTEM_PROMPT",
		"OPENCROW_PI_SKILLS",
		"OPENCROW_PI_SKILLS_DIR",
		"OPENCROW_SOUL_FILE",
		"OPENCROW_HEARTBEAT_INTERVAL",
		"OPENCROW_HEARTBEAT_PROMPT",
		"OPENCROW_LOG_LEVEL",
		"OPENCROW_NOSTR_PRIVATE_KEY",
		"OPENCROW_NOSTR_PRIVATE_KEY_FILE",
		"OPENCROW_NOSTR_RELAYS",
		"OPENCROW_NOSTR_BLOSSOM_SERVERS",
		"OPENCROW_NOSTR_ALLOWED_USERS",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// setRequiredMatrixEnv sets the minimum env vars needed for LoadConfig to succeed
// when backend=matrix (the default).
func setRequiredMatrixEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPENCROW_MATRIX_HOMESERVER", "https://matrix.example.com")
	t.Setenv("OPENCROW_MATRIX_USER_ID", "@bot:example.com")
	t.Setenv("OPENCROW_MATRIX_ACCESS_TOKEN", "syt_test_token")
}

func TestNostrConfig_RelayParsing(t *testing.T) {
	clearConfigEnv(t)
	setRequiredNostrEnv(t)
	t.Setenv("OPENCROW_NOSTR_RELAYS", "wss://relay1.example.com, wss://relay2.example.com")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := []string{"wss://relay1.example.com", "wss://relay2.example.com"}
	if len(cfg.Nostr.Relays) != len(want) {
		t.Fatalf("got %d relays, want %d", len(cfg.Nostr.Relays), len(want))
	}
	for i, r := range cfg.Nostr.Relays {
		if r != want[i] {
			t.Errorf("relay[%d] = %q, want %q", i, r, want[i])
		}
	}
}

func TestNostrConfig_AllowedUsersNpubDecoding(t *testing.T) {
	clearConfigEnv(t)
	setRequiredNostrEnv(t)

	// npub1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq23sn3t (all zeros pubkey)
	// Use a real hex pubkey and its npub equivalent
	hexPK := "0000000000000000000000000000000000000000000000000000000000000001"
	t.Setenv("OPENCROW_NOSTR_ALLOWED_USERS", hexPK)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if _, ok := cfg.Nostr.AllowedUsers[hexPK]; !ok {
		t.Errorf("hex pubkey not in allowed users: got %v", cfg.Nostr.AllowedUsers)
	}
}

func TestNostrConfig_MissingPrivateKey(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("OPENCROW_BACKEND", "nostr")
	t.Setenv("OPENCROW_NOSTR_RELAYS", "wss://relay.example.com")
	// No private key set

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing private key, got nil")
	}
}

func TestNostrConfig_BlossomServers(t *testing.T) {
	clearConfigEnv(t)
	setRequiredNostrEnv(t)
	t.Setenv("OPENCROW_NOSTR_BLOSSOM_SERVERS", "https://blossom1.example.com, https://blossom2.example.com")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := []string{"https://blossom1.example.com", "https://blossom2.example.com"}
	if len(cfg.Nostr.BlossomServers) != len(want) {
		t.Fatalf("got %d blossom servers, want %d", len(cfg.Nostr.BlossomServers), len(want))
	}
	for i, s := range cfg.Nostr.BlossomServers {
		if s != want[i] {
			t.Errorf("blossom[%d] = %q, want %q", i, s, want[i])
		}
	}
}

func TestNostrConfig_MissingRelays(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("OPENCROW_BACKEND", "nostr")
	t.Setenv("OPENCROW_NOSTR_PRIVATE_KEY", "0000000000000000000000000000000000000000000000000000000000000001")
	// No relays set

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing relays, got nil")
	}
}

// setRequiredNostrEnv sets the minimum env vars needed for LoadConfig to succeed
// when backend=nostr.
func setRequiredNostrEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPENCROW_BACKEND", "nostr")
	t.Setenv("OPENCROW_NOSTR_PRIVATE_KEY", "0000000000000000000000000000000000000000000000000000000000000001")
	t.Setenv("OPENCROW_NOSTR_RELAYS", "wss://relay.example.com")
}

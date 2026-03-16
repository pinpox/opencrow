package main

import (
	"os"
	"path/filepath"
	"testing"
)

// testEnv returns a getenv function backed by a map.
func testEnv(m map[string]string) func(string) string {
	return func(key string) string {
		return m[key]
	}
}

// baseMatrixEnv returns the minimum env needed for a matrix backend config.
func baseMatrixEnv() map[string]string {
	return map[string]string{
		"OPENCROW_MATRIX_HOMESERVER":   "https://matrix.example.com",
		"OPENCROW_MATRIX_USER_ID":      "@bot:example.com",
		"OPENCROW_MATRIX_ACCESS_TOKEN": "syt_test_token",
	}
}

// baseNostrEnv returns the minimum env needed for a nostr backend config.
func baseNostrEnv() map[string]string {
	return map[string]string{
		"OPENCROW_BACKEND":           "nostr",
		"OPENCROW_NOSTR_PRIVATE_KEY": "0000000000000000000000000000000000000000000000000000000000000001",
		"OPENCROW_NOSTR_RELAYS":      "wss://relay.example.com",
	}
}

// baseSignalEnv returns the minimum env needed for a signal backend config.
func baseSignalEnv() map[string]string {
	return map[string]string{
		"OPENCROW_BACKEND":        "signal",
		"OPENCROW_SIGNAL_ACCOUNT": "+12025550123",
	}
}

func TestBackendType_Default(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(testEnv(baseMatrixEnv()))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.BackendType != backendMatrix {
		t.Errorf("BackendType = %q, want %q", cfg.BackendType, backendMatrix)
	}
}

func TestBackendType_Unknown(t *testing.T) {
	t.Parallel()

	env := baseMatrixEnv()
	env["OPENCROW_BACKEND"] = "telegram"

	_, err := loadConfig(testEnv(env))
	if err == nil {
		t.Fatal("expected error for unknown backend, got nil")
	}
}

func TestSignalConfig_Valid(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(testEnv(baseSignalEnv()))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.BackendType != backendSignal {
		t.Fatalf("BackendType = %q, want %q", cfg.BackendType, backendSignal)
	}

	if cfg.Signal.Account != "+12025550123" {
		t.Fatalf("Signal.Account = %q, want +12025550123", cfg.Signal.Account)
	}

	if cfg.Signal.BinaryPath == "" {
		t.Fatal("Signal.BinaryPath is empty")
	}

	if cfg.Signal.SocketPath == "" {
		t.Fatal("Signal.SocketPath is empty")
	}
}

func TestSignalConfig_MissingAccount(t *testing.T) {
	t.Parallel()

	env := map[string]string{"OPENCROW_BACKEND": "signal"}

	_, err := loadConfig(testEnv(env))
	if err == nil {
		t.Fatal("expected error for missing signal account, got nil")
	}
}

func TestNostrConfig_RelayParsing(t *testing.T) {
	t.Parallel()

	env := baseNostrEnv()
	env["OPENCROW_NOSTR_RELAYS"] = "wss://relay1.example.com, wss://relay2.example.com"

	cfg, err := loadConfig(testEnv(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
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
	t.Parallel()

	hexPK := "0000000000000000000000000000000000000000000000000000000000000001"

	env := baseNostrEnv()
	env["OPENCROW_NOSTR_ALLOWED_USERS"] = hexPK

	cfg, err := loadConfig(testEnv(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if _, ok := cfg.Nostr.AllowedUsers[hexPK]; !ok {
		t.Errorf("hex pubkey not in allowed users: got %v", cfg.Nostr.AllowedUsers)
	}
}

func TestNostrConfig_MissingPrivateKey(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"OPENCROW_BACKEND":      "nostr",
		"OPENCROW_NOSTR_RELAYS": "wss://relay.example.com",
	}

	_, err := loadConfig(testEnv(env))
	if err == nil {
		t.Fatal("expected error for missing private key, got nil")
	}
}

func TestNostrConfig_BlossomServers(t *testing.T) {
	t.Parallel()

	env := baseNostrEnv()
	env["OPENCROW_NOSTR_BLOSSOM_SERVERS"] = "https://blossom1.example.com, https://blossom2.example.com"

	cfg, err := loadConfig(testEnv(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
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

func TestNostrConfig_DMRelays(t *testing.T) {
	t.Parallel()

	t.Run("explicit", func(t *testing.T) {
		t.Parallel()

		env := baseNostrEnv()
		env["OPENCROW_NOSTR_DM_RELAYS"] = "wss://dm1.example.com, wss://dm2.example.com"

		cfg, err := loadConfig(testEnv(env))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}

		want := []string{"wss://dm1.example.com", "wss://dm2.example.com"}
		if len(cfg.Nostr.DMRelays) != len(want) {
			t.Fatalf("got %d DM relays, want %d", len(cfg.Nostr.DMRelays), len(want))
		}

		for i, r := range cfg.Nostr.DMRelays {
			if r != want[i] {
				t.Errorf("DMRelays[%d] = %q, want %q", i, r, want[i])
			}
		}
	})

	t.Run("empty when not set", func(t *testing.T) {
		t.Parallel()

		env := baseNostrEnv()

		cfg, err := loadConfig(testEnv(env))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}

		// Config layer passes through nil; NewBackend applies the Relays default.
		if len(cfg.Nostr.DMRelays) != 0 {
			t.Fatalf("DMRelays len = %d, want 0 (defaulting is done by NewBackend)", len(cfg.Nostr.DMRelays))
		}
	})
}

func TestDiscoverSkills_Symlinks(t *testing.T) {
	t.Parallel()

	// Create a target directory with SKILL.md
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create a skills dir with a symlink to the target
	skillsDir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(skillsDir, "my-skill")); err != nil {
		t.Fatal(err)
	}

	skills := discoverSkills(skillsDir)
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1: %v", len(skills), skills)
	}

	want := filepath.Join(skillsDir, "my-skill")
	if skills[0] != want {
		t.Errorf("skill path = %q, want %q", skills[0], want)
	}
}

func TestNostrConfig_MissingRelays(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"OPENCROW_BACKEND":           "nostr",
		"OPENCROW_NOSTR_PRIVATE_KEY": "0000000000000000000000000000000000000000000000000000000000000001",
	}

	_, err := loadConfig(testEnv(env))
	if err == nil {
		t.Fatal("expected error for missing relays, got nil")
	}
}

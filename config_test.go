package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMatrixConfig_ValidateReportsAllMissing(t *testing.T) {
	t.Parallel()

	err := (MatrixConfig{}).validate()
	if err == nil {
		t.Fatal("expected error for empty MatrixConfig")
	}

	msg := err.Error()
	for _, want := range []string{
		"OPENCROW_MATRIX_HOMESERVER",
		"OPENCROW_MATRIX_USER_ID",
		"OPENCROW_MATRIX_ACCESS_TOKEN",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

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

// TestLoadConfig_Errors covers the env combinations that must fail
// validation. Each case only differs in the env map, so a table avoids
// repeating three near-identical error-checking functions.
func TestLoadConfig_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "unknown backend",
			env: func() map[string]string {
				m := baseMatrixEnv()
				m["OPENCROW_BACKEND"] = "telegram"

				return m
			}(),
		},
		{
			name: "nostr missing private key",
			env: map[string]string{
				"OPENCROW_BACKEND":      "nostr",
				"OPENCROW_NOSTR_RELAYS": "wss://relay.example.com",
			},
		},
		{
			name: "nostr missing relays",
			env: map[string]string{
				"OPENCROW_BACKEND":           "nostr",
				"OPENCROW_NOSTR_PRIVATE_KEY": "0000000000000000000000000000000000000000000000000000000000000001",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := loadConfig(testEnv(tc.env)); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestNostrConfig_ListParsing covers the comma-separated list env vars
// (relays, blossom servers, DM relays). All three go through the same
// splitter, so one table-driven test replaces three copy-pasted ones.
func TestNostrConfig_ListParsing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envKey string
		envVal string
		get    func(*Config) []string
		want   []string
	}{
		{
			name:   "relays",
			envKey: "OPENCROW_NOSTR_RELAYS",
			envVal: "wss://relay1.example.com, wss://relay2.example.com",
			get:    func(c *Config) []string { return c.Nostr.Relays },
			want:   []string{"wss://relay1.example.com", "wss://relay2.example.com"},
		},
		{
			name:   "blossom servers",
			envKey: "OPENCROW_NOSTR_BLOSSOM_SERVERS",
			envVal: "https://blossom1.example.com, https://blossom2.example.com",
			get:    func(c *Config) []string { return c.Nostr.BlossomServers },
			want:   []string{"https://blossom1.example.com", "https://blossom2.example.com"},
		},
		{
			name:   "DM relays explicit",
			envKey: "OPENCROW_NOSTR_DM_RELAYS",
			envVal: "wss://dm1.example.com, wss://dm2.example.com",
			get:    func(c *Config) []string { return c.Nostr.DMRelays },
			want:   []string{"wss://dm1.example.com", "wss://dm2.example.com"},
		},
		{
			// Config layer passes through nil; NewBackend applies
			// the Relays default.
			name: "DM relays empty when not set",
			get:  func(c *Config) []string { return c.Nostr.DMRelays },
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := baseNostrEnv()
			if tc.envKey != "" {
				env[tc.envKey] = tc.envVal
			}

			cfg, err := loadConfig(testEnv(env))
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}

			got := tc.get(cfg)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
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

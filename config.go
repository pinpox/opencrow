package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

type Config struct {
	BackendType string // "matrix" or "nostr"
	Matrix      MatrixConfig
	Nostr       NostrConfig
	Pi          PiConfig
	Heartbeat   HeartbeatConfig
}

type HeartbeatConfig struct {
	Interval time.Duration // OPENCROW_HEARTBEAT_INTERVAL, default 0 (disabled)
	Prompt   string        // OPENCROW_HEARTBEAT_PROMPT, default built-in
}

type MatrixConfig struct {
	Homeserver   string
	UserID       string
	AccessToken  string
	DeviceID     string
	AllowedUsers map[string]struct{}
	PickleKey    string
	CryptoDBPath string
}

type NostrConfig struct {
	PrivateKey     string              // hex secret key (resolved from file or env)
	Relays         []string            // OPENCROW_NOSTR_RELAYS
	BlossomServers []string            // OPENCROW_NOSTR_BLOSSOM_SERVERS
	AllowedUsers   map[string]struct{} // OPENCROW_NOSTR_ALLOWED_USERS (hex pubkeys)
	SessionBaseDir string              // shared with PiConfig.SessionDir
}

type PiConfig struct {
	BinaryPath   string
	SessionDir   string
	Provider     string
	Model        string
	WorkingDir   string
	IdleTimeout  time.Duration
	SystemPrompt string
	Skills       []string
}

func LoadConfig() (*Config, error) {
	backendType := envOr("OPENCROW_BACKEND", "matrix")

	switch backendType {
	case "matrix", "nostr":
		// valid
	default:
		return nil, fmt.Errorf("OPENCROW_BACKEND=%q is not supported (valid: matrix, nostr)", backendType)
	}

	idleTimeout, err := parseIdleTimeout()
	if err != nil {
		return nil, err
	}

	skills := parseSkills()
	allowedUsers := parseAllowedUsers()
	workingDir := envOr("OPENCROW_PI_WORKING_DIR", "/var/lib/opencrow")

	heartbeatInterval, err := parseHeartbeatInterval()
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		BackendType: backendType,
		Matrix: MatrixConfig{
			Homeserver:   os.Getenv("OPENCROW_MATRIX_HOMESERVER"),
			UserID:       os.Getenv("OPENCROW_MATRIX_USER_ID"),
			AccessToken:  os.Getenv("OPENCROW_MATRIX_ACCESS_TOKEN"),
			DeviceID:     os.Getenv("OPENCROW_MATRIX_DEVICE_ID"),
			AllowedUsers: allowedUsers,
			PickleKey:    envOr("OPENCROW_MATRIX_PICKLE_KEY", "opencrow-default-pickle-key"),
			CryptoDBPath: envOr("OPENCROW_MATRIX_CRYPTO_DB", filepath.Join(workingDir, "crypto.db")),
		},
		Pi: PiConfig{
			BinaryPath:   envOr("OPENCROW_PI_BINARY", "pi"),
			SessionDir:   envOr("OPENCROW_PI_SESSION_DIR", "/var/lib/opencrow/sessions"),
			Provider:     envOr("OPENCROW_PI_PROVIDER", "anthropic"),
			Model:        envOr("OPENCROW_PI_MODEL", "claude-opus-4-6"),
			WorkingDir:   workingDir,
			IdleTimeout:  idleTimeout,
			SystemPrompt: loadSoul(),
			Skills:       skills,
		},
		Heartbeat: HeartbeatConfig{
			Interval: heartbeatInterval,
			Prompt:   envOr("OPENCROW_HEARTBEAT_PROMPT", defaultHeartbeatPrompt),
		},
	}

	switch cfg.BackendType {
	case "matrix":
		if cfg.Matrix.Homeserver == "" {
			return nil, errors.New("OPENCROW_MATRIX_HOMESERVER is required")
		}
		if cfg.Matrix.UserID == "" {
			return nil, errors.New("OPENCROW_MATRIX_USER_ID is required")
		}
		if cfg.Matrix.AccessToken == "" {
			return nil, errors.New("OPENCROW_MATRIX_ACCESS_TOKEN is required")
		}

	case "nostr":
		nostrCfg, err := loadNostrConfig(cfg.Pi.SessionDir)
		if err != nil {
			return nil, err
		}
		cfg.Nostr = nostrCfg
	}

	return cfg, nil
}

func parseIdleTimeout() (time.Duration, error) {
	if v := os.Getenv("OPENCROW_PI_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("parsing OPENCROW_PI_IDLE_TIMEOUT: %w", err)
		}

		return d, nil
	}

	return 30 * time.Minute, nil
}

func parseSkills() []string {
	var skills []string

	if v := os.Getenv("OPENCROW_PI_SKILLS"); v != "" {
		for s := range strings.SplitSeq(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				skills = append(skills, s)
			}
		}
	}

	if dir := os.Getenv("OPENCROW_PI_SKILLS_DIR"); dir != "" {
		discovered := discoverSkills(dir)
		skills = append(skills, discovered...)
	}

	return skills
}

// discoverSkills scans a directory for subdirectories containing SKILL.md.
func discoverSkills(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: failed to read skills dir %s: %v\n", dir, err)
		}

		return nil
	}

	var skills []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(skillPath, "SKILL.md")

		if _, err := os.Stat(skillFile); err == nil {
			skills = append(skills, skillPath)
		}
	}

	return skills
}

func parseAllowedUsers() map[string]struct{} {
	allowedUsers := make(map[string]struct{})

	if v := os.Getenv("OPENCROW_ALLOWED_USERS"); v != "" {
		for u := range strings.SplitSeq(v, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				allowedUsers[u] = struct{}{}
			}
		}
	}

	return allowedUsers
}

func parseHeartbeatInterval() (time.Duration, error) {
	if v := os.Getenv("OPENCROW_HEARTBEAT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("parsing OPENCROW_HEARTBEAT_INTERVAL: %w", err)
		}

		return d, nil
	}

	return 0, nil
}

// loadSoul reads the system prompt from OPENCROW_SOUL_FILE if set,
// falling back to OPENCROW_PI_SYSTEM_PROMPT, then the built-in default.
func loadSoul() string {
	if path := os.Getenv("OPENCROW_SOUL_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to read soul file %s: %v\n", path, err)
		} else {
			return string(data)
		}
	}

	if v := os.Getenv("OPENCROW_PI_SYSTEM_PROMPT"); v != "" {
		return v
	}

	return defaultSoul
}

const defaultSoul = `You are OpenCrow, an AI assistant communicating via a messaging platform.

Be genuinely helpful, not performatively helpful. Skip the filler words — just help.
Have opinions. Be resourceful before asking. Earn trust through competence.
Be concise when needed, thorough when it matters. Not a corporate drone. Not a sycophant. Just good.
When using tools, prefer standard Unix tools. Check output before proceeding. Break complex tasks into steps and execute them.

## Reminders and scheduled tasks

You have a file called HEARTBEAT.md in your session directory. A background scheduler reads
this file periodically and prompts you with its contents. Use it for reminders and recurring tasks.

When a user asks you to remind them of something or to do something later, write the task to
HEARTBEAT.md in your session directory. Use a clear format, for example:

- [ ] 2025-06-15 14:00 — Remind user about the deployment
- [ ] Every Monday 09:00 — Post weekly standup summary

When a heartbeat fires and you act on a task, mark it done (- [x]) or remove it.
Do not duplicate tasks that are already listed.`

const defaultHeartbeatPrompt = `Read HEARTBEAT.md if it exists. Follow any tasks listed there strictly.
Do not infer or repeat old tasks from prior conversations.
If nothing needs attention, reply with exactly: HEARTBEAT_OK`

const defaultTriggerPrompt = `An external process sent a trigger message. Read the content below and act on it.
You MUST fully process the trigger before deciding on a response. Only reply with
exactly HEARTBEAT_OK if your processing rules explicitly tell you to ignore it.`

func loadNostrConfig(sessionBaseDir string) (NostrConfig, error) {
	privateKey, err := loadNostrPrivateKey()
	if err != nil {
		return NostrConfig{}, err
	}

	relays := parseCommaSeparated(os.Getenv("OPENCROW_NOSTR_RELAYS"))
	if len(relays) == 0 {
		return NostrConfig{}, errors.New("OPENCROW_NOSTR_RELAYS is required (comma-separated relay URLs)")
	}

	allowedUsers, err := parseNostrAllowedUsers(os.Getenv("OPENCROW_NOSTR_ALLOWED_USERS"))
	if err != nil {
		return NostrConfig{}, err
	}

	return NostrConfig{
		PrivateKey:     privateKey,
		Relays:         relays,
		BlossomServers: parseCommaSeparated(os.Getenv("OPENCROW_NOSTR_BLOSSOM_SERVERS")),
		AllowedUsers:   allowedUsers,
		SessionBaseDir: sessionBaseDir,
	}, nil
}

func loadNostrPrivateKey() (string, error) {
	var raw string

	if path := os.Getenv("OPENCROW_NOSTR_PRIVATE_KEY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading OPENCROW_NOSTR_PRIVATE_KEY_FILE: %w", err)
		}
		raw = strings.TrimSpace(string(data))
	}

	if raw == "" {
		raw = os.Getenv("OPENCROW_NOSTR_PRIVATE_KEY")
	}

	if raw == "" {
		return "", errors.New("OPENCROW_NOSTR_PRIVATE_KEY or OPENCROW_NOSTR_PRIVATE_KEY_FILE is required")
	}

	// Decode nsec if needed
	if strings.HasPrefix(raw, "nsec") {
		prefix, val, err := nip19.Decode(raw)
		if err != nil {
			return "", fmt.Errorf("decoding nsec: %w", err)
		}
		if prefix != "nsec" {
			return "", fmt.Errorf("expected nsec prefix, got %s", prefix)
		}
		raw = val.(gonostr.SecretKey).Hex()
	}

	return raw, nil
}

func parseNostrAllowedUsers(s string) (map[string]struct{}, error) {
	users := make(map[string]struct{})

	for _, u := range parseCommaSeparated(s) {
		if strings.HasPrefix(u, "npub") {
			prefix, val, err := nip19.Decode(u)
			if err != nil {
				return nil, fmt.Errorf("decoding npub %q: %w", u, err)
			}
			if prefix != "npub" {
				return nil, fmt.Errorf("expected npub prefix, got %s", prefix)
			}
			u = val.(gonostr.PubKey).Hex()
		}
		users[u] = struct{}{}
	}

	return users, nil
}

func parseCommaSeparated(s string) []string {
	if s == "" {
		return nil
	}

	var result []string
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}

	return result
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

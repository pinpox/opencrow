package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	nostrkeys "github.com/pinpox/opencrow/nostr"
)

const (
	backendMatrix = "matrix"
	backendNostr  = "nostr"
)

type Config struct {
	BackendType string // backendMatrix or backendNostr
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
	DMRelays       []string            // OPENCROW_NOSTR_DM_RELAYS (published in kind 10050; defaults to Relays)
	BlossomServers []string            // OPENCROW_NOSTR_BLOSSOM_SERVERS
	AllowedUsers   map[string]struct{} // OPENCROW_NOSTR_ALLOWED_USERS (hex pubkeys)
	SessionBaseDir string              // shared with PiConfig.SessionDir
	Name           string              // OPENCROW_NOSTR_NAME (kind 0 "name")
	DisplayName    string              // OPENCROW_NOSTR_DISPLAY_NAME (kind 0 "display_name")
	About          string              // OPENCROW_NOSTR_ABOUT (kind 0 "about")
	Picture        string              // OPENCROW_NOSTR_PICTURE (kind 0 "picture" URL)
}

type PiConfig struct {
	BinaryPath    string
	SessionDir    string
	Provider      string
	Model         string
	WorkingDir    string
	IdleTimeout   time.Duration
	SystemPrompt  string
	Skills        []string
	ShowToolCalls bool // OPENCROW_SHOW_TOOL_CALLS — relay tool_execution_start events to chat
}

// LoadConfig reads configuration from os.Getenv.
func LoadConfig() (*Config, error) {
	return loadConfig(os.Getenv)
}

// loadConfig reads configuration using the provided env-lookup function,
// allowing tests to supply isolated environments without mutating os state.
func loadConfig(getenv func(string) string) (*Config, error) {
	env := envReader{getenv: getenv}
	backendType := env.or("OPENCROW_BACKEND", backendMatrix)

	switch backendType {
	case backendMatrix, backendNostr:
		// valid
	default:
		return nil, fmt.Errorf("OPENCROW_BACKEND=%q is not supported (valid: matrix, nostr)", backendType)
	}

	idleTimeout, err := parseDuration(getenv("OPENCROW_PI_IDLE_TIMEOUT"), 30*time.Minute, "OPENCROW_PI_IDLE_TIMEOUT")
	if err != nil {
		return nil, err
	}

	skills := parseSkills(getenv)
	allowedUsers := parseAllowedUsers(getenv("OPENCROW_ALLOWED_USERS"))
	workingDir := env.or("OPENCROW_PI_WORKING_DIR", "/var/lib/opencrow")

	heartbeatInterval, err := parseDuration(getenv("OPENCROW_HEARTBEAT_INTERVAL"), 0, "OPENCROW_HEARTBEAT_INTERVAL")
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		BackendType: backendType,
		Matrix: MatrixConfig{
			Homeserver:   getenv("OPENCROW_MATRIX_HOMESERVER"),
			UserID:       getenv("OPENCROW_MATRIX_USER_ID"),
			AccessToken:  getenv("OPENCROW_MATRIX_ACCESS_TOKEN"),
			DeviceID:     getenv("OPENCROW_MATRIX_DEVICE_ID"),
			AllowedUsers: allowedUsers,
			PickleKey:    env.or("OPENCROW_MATRIX_PICKLE_KEY", "opencrow-default-pickle-key"),
			CryptoDBPath: env.or("OPENCROW_MATRIX_CRYPTO_DB", filepath.Join(workingDir, "crypto.db")),
		},
		Pi: PiConfig{
			BinaryPath:    env.or("OPENCROW_PI_BINARY", "pi"),
			SessionDir:    env.or("OPENCROW_PI_SESSION_DIR", "/var/lib/opencrow/sessions"),
			Provider:      env.or("OPENCROW_PI_PROVIDER", "anthropic"),
			Model:         env.or("OPENCROW_PI_MODEL", "claude-opus-4-6"),
			WorkingDir:    workingDir,
			IdleTimeout:   idleTimeout,
			SystemPrompt:  loadSoul(getenv),
			Skills:        skills,
			ShowToolCalls: parseBool(getenv("OPENCROW_SHOW_TOOL_CALLS")),
		},
		Heartbeat: HeartbeatConfig{
			Interval: heartbeatInterval,
			Prompt:   env.or("OPENCROW_HEARTBEAT_PROMPT", defaultHeartbeatPrompt),
		},
	}

	if err := cfg.validateBackend(getenv); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (cfg *Config) validateBackend(getenv func(string) string) error {
	switch cfg.BackendType {
	case backendMatrix:
		return cfg.Matrix.validate()
	case backendNostr:
		nostrCfg, err := loadNostrConfig(getenv, cfg.Pi.SessionDir)
		if err != nil {
			return err
		}

		cfg.Nostr = nostrCfg
	}

	return nil
}

func (m MatrixConfig) validate() error {
	if m.Homeserver == "" {
		return errors.New("OPENCROW_MATRIX_HOMESERVER is required")
	}

	if m.UserID == "" {
		return errors.New("OPENCROW_MATRIX_USER_ID is required")
	}

	if m.AccessToken == "" {
		return errors.New("OPENCROW_MATRIX_ACCESS_TOKEN is required")
	}

	return nil
}

// envReader wraps a getenv function with a fallback helper.
type envReader struct {
	getenv func(string) string
}

func (e envReader) or(key, fallback string) string {
	if v := e.getenv(key); v != "" {
		return v
	}

	return fallback
}

// parseDuration parses a duration string from an env var value.
// Returns the default if the value is empty.
func parseDuration(val string, defaultVal time.Duration, name string) (time.Duration, error) {
	if val == "" {
		return defaultVal, nil
	}

	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", name, err)
	}

	return d, nil
}

func parseSkills(getenv func(string) string) []string {
	var skills []string

	if v := getenv("OPENCROW_PI_SKILLS"); v != "" {
		for s := range strings.SplitSeq(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				skills = append(skills, s)
			}
		}
	}

	if dir := getenv("OPENCROW_PI_SKILLS_DIR"); dir != "" {
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
		skillPath := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(skillPath, "SKILL.md")

		if _, err := os.Stat(skillFile); err == nil {
			skills = append(skills, skillPath)
		}
	}

	return skills
}

func parseAllowedUsers(val string) map[string]struct{} {
	allowedUsers := make(map[string]struct{})

	if val != "" {
		for u := range strings.SplitSeq(val, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				allowedUsers[u] = struct{}{}
			}
		}
	}

	return allowedUsers
}

// loadSoul reads the system prompt from OPENCROW_SOUL_FILE if set,
// falling back to OPENCROW_PI_SYSTEM_PROMPT, then the built-in default.
func loadSoul(getenv func(string) string) string {
	if path := getenv("OPENCROW_SOUL_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to read soul file %s: %v\n", path, err)
		} else {
			return string(data)
		}
	}

	if v := getenv("OPENCROW_PI_SYSTEM_PROMPT"); v != "" {
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

func loadNostrConfig(getenv func(string) string, sessionBaseDir string) (NostrConfig, error) {
	privateKey, err := loadNostrPrivateKey(getenv)
	if err != nil {
		return NostrConfig{}, err
	}

	relays := parseCommaSeparated(getenv("OPENCROW_NOSTR_RELAYS"))
	if len(relays) == 0 {
		return NostrConfig{}, errors.New("OPENCROW_NOSTR_RELAYS is required (comma-separated relay URLs)")
	}

	dmRelays := parseCommaSeparated(getenv("OPENCROW_NOSTR_DM_RELAYS"))

	allowedUsers, err := parseNostrAllowedUsers(getenv("OPENCROW_NOSTR_ALLOWED_USERS"))
	if err != nil {
		return NostrConfig{}, err
	}

	return NostrConfig{
		PrivateKey:     privateKey,
		Relays:         relays,
		DMRelays:       dmRelays,
		BlossomServers: parseCommaSeparated(getenv("OPENCROW_NOSTR_BLOSSOM_SERVERS")),
		AllowedUsers:   allowedUsers,
		SessionBaseDir: sessionBaseDir,
		Name:           getenv("OPENCROW_NOSTR_NAME"),
		DisplayName:    getenv("OPENCROW_NOSTR_DISPLAY_NAME"),
		About:          getenv("OPENCROW_NOSTR_ABOUT"),
		Picture:        getenv("OPENCROW_NOSTR_PICTURE"),
	}, nil
}

func loadNostrPrivateKey(getenv func(string) string) (string, error) {
	var raw string

	if path := getenv("OPENCROW_NOSTR_PRIVATE_KEY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading OPENCROW_NOSTR_PRIVATE_KEY_FILE: %w", err)
		}

		raw = strings.TrimSpace(string(data))
	}

	if raw == "" {
		raw = getenv("OPENCROW_NOSTR_PRIVATE_KEY")
	}

	if raw == "" {
		return "", errors.New("OPENCROW_NOSTR_PRIVATE_KEY or OPENCROW_NOSTR_PRIVATE_KEY_FILE is required")
	}

	hex, err := nostrkeys.DecodeNsecToHex(raw)
	if err != nil {
		return "", fmt.Errorf("decoding nostr private key: %w", err)
	}

	return hex, nil
}

func parseNostrAllowedUsers(s string) (map[string]struct{}, error) {
	users := make(map[string]struct{})

	for _, u := range parseCommaSeparated(s) {
		hex, err := nostrkeys.DecodeNpubToHex(u)
		if err != nil {
			return nil, fmt.Errorf("decoding nostr allowed user: %w", err)
		}

		users[hex] = struct{}{}
	}

	return users, nil
}

func parseBool(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
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

package main

import (
	"cmp"
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
	backendSignal = "signal"
)

type Config struct {
	BackendType string // backendMatrix, backendNostr, or backendSignal
	Matrix      MatrixConfig
	Nostr       NostrConfig
	Signal      SignalConfig
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

type SignalConfig struct {
	Account      string
	BinaryPath   string
	ConfigDir    string
	SocketPath   string
	AllowedUsers map[string]struct{}
}

type NostrConfig struct {
	PrivateKey     string              // hex secret key (resolved from file or env)
	Relays         []string            // OPENCROW_NOSTR_RELAYS
	DMRelays       []string            // OPENCROW_NOSTR_DM_RELAYS (published in kind 10050; defaults to Relays)
	BlossomServers []string            // OPENCROW_NOSTR_BLOSSOM_SERVERS
	AllowedUsers   map[string]struct{} // OPENCROW_NOSTR_ALLOWED_USERS (hex pubkeys)
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
	DebugTiming   bool // OPENCROW_DEBUG_TIMING — append timing info to each reply
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
	case backendMatrix, backendNostr, backendSignal:
		// valid
	default:
		return nil, fmt.Errorf("OPENCROW_BACKEND=%q is not supported (valid: matrix, nostr, signal)", backendType)
	}

	idleTimeout, err := env.duration("OPENCROW_PI_IDLE_TIMEOUT", 30*time.Minute)
	if err != nil {
		return nil, err
	}

	skills := parseSkills(env)
	allowedUsers := parseAllowedUsers(env.list("OPENCROW_ALLOWED_USERS"))
	workingDir := env.or("OPENCROW_PI_WORKING_DIR", "/var/lib/opencrow")

	heartbeatInterval, err := env.duration("OPENCROW_HEARTBEAT_INTERVAL", 0)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		BackendType: backendType,
		Matrix: MatrixConfig{
			Homeserver:   env.str("OPENCROW_MATRIX_HOMESERVER"),
			UserID:       env.str("OPENCROW_MATRIX_USER_ID"),
			AccessToken:  env.str("OPENCROW_MATRIX_ACCESS_TOKEN"),
			DeviceID:     env.str("OPENCROW_MATRIX_DEVICE_ID"),
			AllowedUsers: allowedUsers,
			PickleKey:    env.or("OPENCROW_MATRIX_PICKLE_KEY", "opencrow-default-pickle-key"),
			CryptoDBPath: env.or("OPENCROW_MATRIX_CRYPTO_DB", filepath.Join(workingDir, "crypto.db")),
		},
		Signal: loadSignalConfig(env, workingDir, allowedUsers),
		Pi: PiConfig{
			BinaryPath:    env.or("OPENCROW_PI_BINARY", "pi"),
			SessionDir:    env.or("OPENCROW_PI_SESSION_DIR", "/var/lib/opencrow/sessions"),
			Provider:      env.or("OPENCROW_PI_PROVIDER", "anthropic"),
			Model:         env.or("OPENCROW_PI_MODEL", "claude-opus-4-6"),
			WorkingDir:    workingDir,
			IdleTimeout:   idleTimeout,
			SystemPrompt:  loadSoul(env),
			Skills:        skills,
			ShowToolCalls: env.bool("OPENCROW_SHOW_TOOL_CALLS"),
			DebugTiming:   env.bool("OPENCROW_DEBUG_TIMING"),
		},
		Heartbeat: HeartbeatConfig{
			Interval: heartbeatInterval,
			Prompt:   env.or("OPENCROW_HEARTBEAT_PROMPT", defaultHeartbeatPrompt),
		},
	}

	if err := cfg.validateBackend(env); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (cfg *Config) validateBackend(env envReader) error {
	switch cfg.BackendType {
	case backendMatrix:
		return cfg.Matrix.validate()
	case backendNostr:
		nostrCfg, err := loadNostrConfig(env)
		if err != nil {
			return err
		}

		cfg.Nostr = nostrCfg
	case backendSignal:
		return cfg.Signal.validate()
	}

	return nil
}

func (m MatrixConfig) validate() error {
	return errors.Join(
		requireField(m.Homeserver, "OPENCROW_MATRIX_HOMESERVER"),
		requireField(m.UserID, "OPENCROW_MATRIX_USER_ID"),
		requireField(m.AccessToken, "OPENCROW_MATRIX_ACCESS_TOKEN"),
	)
}

func loadSignalConfig(env envReader, workingDir string, allowedUsers map[string]struct{}) SignalConfig {
	return SignalConfig{
		Account:      env.str("OPENCROW_SIGNAL_ACCOUNT"),
		BinaryPath:   env.or("OPENCROW_SIGNAL_CLI_BINARY", "signal-cli"),
		ConfigDir:    env.or("OPENCROW_SIGNAL_CONFIG_DIR", filepath.Join(workingDir, "signal-cli")),
		SocketPath:   env.or("OPENCROW_SIGNAL_SOCKET_PATH", filepath.Join(workingDir, "signal-cli", "opencrow-jsonrpc.sock")),
		AllowedUsers: allowedUsers,
	}
}

func (s SignalConfig) validate() error {
	return errors.Join(
		requireField(s.Account, "OPENCROW_SIGNAL_ACCOUNT"),
		requireField(s.BinaryPath, "OPENCROW_SIGNAL_CLI_BINARY"),
		requireField(s.SocketPath, "OPENCROW_SIGNAL_SOCKET_PATH"),
	)
}

// requireField returns an "is required" error if v is empty. Intended for
// use with errors.Join so that validate() reports all missing fields at once.
func requireField(v, name string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}

	return nil
}

// envReader wraps a getenv function with typed accessors so callers do not
// mix raw string lookups with ad-hoc parsing at every call site.
type envReader struct {
	getenv func(string) string
}

// str returns the raw value of key.
func (e envReader) str(key string) string {
	return e.getenv(key)
}

// or returns the value of key, or fallback if empty.
func (e envReader) or(key, fallback string) string {
	if v := e.getenv(key); v != "" {
		return v
	}

	return fallback
}

// list parses a comma-separated value, trimming whitespace and dropping empties.
func (e envReader) list(key string) []string {
	return parseCommaSeparated(e.getenv(key))
}

// bool interprets "1", "true", "yes" (case-insensitive) as true.
func (e envReader) bool(key string) bool {
	switch strings.ToLower(e.getenv(key)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// duration parses a time.Duration, returning def if unset. The error message
// includes the key name so callers do not need to repeat it.
func (e envReader) duration(key string, def time.Duration) (time.Duration, error) {
	v := e.getenv(key)
	if v == "" {
		return def, nil
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", key, err)
	}

	return d, nil
}

func parseSkills(env envReader) []string {
	skills := env.list("OPENCROW_PI_SKILLS")

	if dir := env.str("OPENCROW_PI_SKILLS_DIR"); dir != "" {
		skills = append(skills, discoverSkills(dir)...)
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

func parseAllowedUsers(users []string) map[string]struct{} {
	allowedUsers := make(map[string]struct{})
	for _, u := range users {
		allowedUsers[u] = struct{}{}
	}

	return allowedUsers
}

// loadSoul reads the system prompt from OPENCROW_SOUL_FILE if set,
// falling back to OPENCROW_PI_SYSTEM_PROMPT, then the built-in default.
func loadSoul(env envReader) string {
	if path := env.str("OPENCROW_SOUL_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to read soul file %s: %v\n", path, err)
		} else {
			return string(data)
		}
	}

	return env.or("OPENCROW_PI_SYSTEM_PROMPT", defaultSoul)
}

const defaultSoul = `You are OpenCrow, an AI assistant communicating via a messaging platform.

Be genuinely helpful, not performatively helpful. Skip the filler words — just help.
Have opinions. Be resourceful before asking. Earn trust through competence.
Be concise when needed, thorough when it matters. Not a corporate drone. Not a sycophant. Just good.
When using tools, prefer standard Unix tools. Check output before proceeding. Break complex tasks into steps and execute them.`

// heartbeatSoul is appended to the system prompt only when heartbeat is
// enabled, so the agent isn't told about HEARTBEAT.md when the scheduler
// never reads it.
const heartbeatSoul = `## Heartbeat

HEARTBEAT.md in your session directory holds standing checks to run
periodically. One check per line:

  - Check for urgent email
  - [paused] Review old PRs

Edit the file to add/remove checks. Prefix with [paused] to skip without
deleting. This is a stable checklist — for one-shot reminders, use the
remind_at tool instead.`

const defaultHeartbeatPrompt = `Run through the standing checks below.
If nothing needs attention, reply with exactly: HEARTBEAT_OK`

const defaultTriggerPrompt = `An external process sent a trigger message. Read the content below and act on it.
You MUST fully process the trigger before deciding on a response. Only reply with
exactly HEARTBEAT_OK if your processing rules explicitly tell you to ignore it.`

func loadNostrConfig(env envReader) (NostrConfig, error) {
	privateKey, err := loadNostrPrivateKey(env)
	if err != nil {
		return NostrConfig{}, err
	}

	relays := env.list("OPENCROW_NOSTR_RELAYS")
	if len(relays) == 0 {
		return NostrConfig{}, errors.New("OPENCROW_NOSTR_RELAYS is required (comma-separated relay URLs)")
	}

	allowedUsers, err := parseNostrAllowedUsers(env.list("OPENCROW_NOSTR_ALLOWED_USERS"))
	if err != nil {
		return NostrConfig{}, err
	}

	return NostrConfig{
		PrivateKey:     privateKey,
		Relays:         relays,
		DMRelays:       env.list("OPENCROW_NOSTR_DM_RELAYS"),
		BlossomServers: env.list("OPENCROW_NOSTR_BLOSSOM_SERVERS"),
		AllowedUsers:   allowedUsers,
		Name:           env.str("OPENCROW_NOSTR_NAME"),
		DisplayName:    env.str("OPENCROW_NOSTR_DISPLAY_NAME"),
		About:          env.str("OPENCROW_NOSTR_ABOUT"),
		Picture:        env.str("OPENCROW_NOSTR_PICTURE"),
	}, nil
}

func loadNostrPrivateKey(env envReader) (string, error) {
	var raw string

	if path := env.str("OPENCROW_NOSTR_PRIVATE_KEY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading OPENCROW_NOSTR_PRIVATE_KEY_FILE: %w", err)
		}

		raw = strings.TrimSpace(string(data))
	}

	if raw = cmp.Or(raw, env.str("OPENCROW_NOSTR_PRIVATE_KEY")); raw == "" {
		return "", errors.New("OPENCROW_NOSTR_PRIVATE_KEY or OPENCROW_NOSTR_PRIVATE_KEY_FILE is required")
	}

	hex, err := nostrkeys.DecodeNsecToHex(raw)
	if err != nil {
		return "", fmt.Errorf("decoding nostr private key: %w", err)
	}

	return hex, nil
}

func parseNostrAllowedUsers(raw []string) (map[string]struct{}, error) {
	users := make(map[string]struct{})

	for _, u := range raw {
		hex, err := nostrkeys.DecodeNpubToHex(u)
		if err != nil {
			return nil, fmt.Errorf("decoding nostr allowed user: %w", err)
		}

		users[hex] = struct{}{}
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

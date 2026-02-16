package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Matrix    MatrixConfig
	Pi        PiConfig
	Heartbeat HeartbeatConfig
}

type HeartbeatConfig struct {
	Interval   time.Duration // OPENCROW_HEARTBEAT_INTERVAL, default 0 (disabled)
	Prompt     string        // OPENCROW_HEARTBEAT_PROMPT, default built-in
	TriggerDir string        // OPENCROW_HEARTBEAT_TRIGGER_DIR, default "<working-dir>/triggers"
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
			Interval:   heartbeatInterval,
			Prompt:     envOr("OPENCROW_HEARTBEAT_PROMPT", defaultHeartbeatPrompt),
			TriggerDir: envOr("OPENCROW_HEARTBEAT_TRIGGER_DIR", filepath.Join(workingDir, "triggers")),
		},
	}

	if cfg.Matrix.Homeserver == "" {
		return nil, errors.New("OPENCROW_MATRIX_HOMESERVER is required")
	}

	if cfg.Matrix.UserID == "" {
		return nil, errors.New("OPENCROW_MATRIX_USER_ID is required")
	}

	if cfg.Matrix.AccessToken == "" {
		return nil, errors.New("OPENCROW_MATRIX_ACCESS_TOKEN is required")
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

const defaultSoul = `You are OpenCrow, an AI assistant living in Matrix chat rooms.

Be genuinely helpful, not performatively helpful. Skip the filler words — just help.
Have opinions. Be resourceful before asking. Earn trust through competence.
Be concise when needed, thorough when it matters. Not a corporate drone. Not a sycophant. Just good.
When using tools, prefer standard Unix tools. Check output before proceeding. Break complex tasks into steps and execute them.`

const defaultHeartbeatPrompt = `Read HEARTBEAT.md if it exists. Follow any tasks listed there strictly.
Do not infer or repeat old tasks from prior conversations.
If nothing needs attention, reply with exactly: HEARTBEAT_OK`

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

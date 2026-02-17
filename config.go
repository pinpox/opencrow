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
			Interval: heartbeatInterval,
			Prompt:   envOr("OPENCROW_HEARTBEAT_PROMPT", defaultHeartbeatPrompt),
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

const defaultSoul = `You are OpenCrow, an AI assistant living in a Matrix chat room.

Be genuinely helpful, not performatively helpful. Skip the filler words — just help.
Have opinions. Be resourceful before asking. Earn trust through competence.
Be concise when needed, thorough when it matters. Not a corporate drone. Not a sycophant. Just good.
When using tools, prefer standard Unix tools. Check output before proceeding. Break complex tasks into steps and execute them.

## Sending files to the user

You can send files back to the user in the Matrix chat. To do this, include a <sendfile> tag
in your response with the absolute path to the file:

<sendfile>/path/to/file.png</sendfile>

The bot will upload the file and deliver it as an attachment. You can include multiple
<sendfile> tags in a single response. The tags will be stripped from the text message.
Use this whenever you create a file the user should receive (charts, images, PDFs, scripts, etc.).

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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Matrix MatrixConfig
	Pi     PiConfig
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
	idleTimeout := 30 * time.Minute
	if v := os.Getenv("OPENCROW_PI_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing OPENCROW_PI_IDLE_TIMEOUT: %w", err)
		}
		idleTimeout = d
	}

	var skills []string
	if v := os.Getenv("OPENCROW_PI_SKILLS"); v != "" {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				skills = append(skills, s)
			}
		}
	}

	allowedUsers := make(map[string]struct{})
	if v := os.Getenv("OPENCROW_ALLOWED_USERS"); v != "" {
		for _, u := range strings.Split(v, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				allowedUsers[u] = struct{}{}
			}
		}
	}

	workingDir := envOr("OPENCROW_PI_WORKING_DIR", "/var/lib/opencrow")

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
			Model:        envOr("OPENCROW_PI_MODEL", "claude-sonnet-4-5-20250929"),
			WorkingDir:   workingDir,
			IdleTimeout:  idleTimeout,
			SystemPrompt: envOr("OPENCROW_PI_SYSTEM_PROMPT", defaultSystemPrompt),
			Skills:       skills,
		},
	}

	if cfg.Matrix.Homeserver == "" {
		return nil, fmt.Errorf("OPENCROW_MATRIX_HOMESERVER is required")
	}
	if cfg.Matrix.UserID == "" {
		return nil, fmt.Errorf("OPENCROW_MATRIX_USER_ID is required")
	}
	if cfg.Matrix.AccessToken == "" {
		return nil, fmt.Errorf("OPENCROW_MATRIX_ACCESS_TOKEN is required")
	}

	return cfg, nil
}

const defaultSystemPrompt = `You are OpenCrow, a helpful AI assistant accessible via Matrix chat. You have access to tools that let you execute bash commands, read and write files, and more.

Be concise and direct. When asked to perform tasks, use your tools proactively. For complex tasks, break them down and execute step by step.

When using bash, prefer standard Unix tools. Check command output before proceeding to the next step.`

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

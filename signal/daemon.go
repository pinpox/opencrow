package signal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (b *Backend) preparePaths() error {
	if b.cfg.ConfigDir != "" {
		if err := os.MkdirAll(b.cfg.ConfigDir, 0o700); err != nil {
			return fmt.Errorf("creating signal config dir: %w", err)
		}
	}

	sockDir := filepath.Dir(b.cfg.SocketPath)
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return fmt.Errorf("creating signal socket dir: %w", err)
	}

	if err := os.Remove(b.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale signal socket: %w", err)
	}

	return nil
}

func (b *Backend) startDaemon(ctx context.Context) error {
	args := []string{"--output", "json", "--account", b.cfg.Account}
	if b.cfg.ConfigDir != "" {
		args = append(args, "--config", b.cfg.ConfigDir)
	}

	args = append(args,
		"daemon",
		"--socket="+b.cfg.SocketPath,
		"--receive-mode", "manual",
	)

	slog.Info("signal: starting daemon",
		"binary", b.cfg.BinaryPath,
		"account", b.cfg.Account,
		"config_dir", b.cfg.ConfigDir,
		"socket", b.cfg.SocketPath,
		"args", strings.Join(args, " "),
	)

	cmd := exec.CommandContext(ctx, b.cfg.BinaryPath, args...) //nolint:gosec // binary path and args come from trusted service config.

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating signal daemon stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("creating signal daemon stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting signal daemon: %w", err)
	}

	done := make(chan error, 1)

	b.resetDaemonLogTail()

	go b.captureDaemonStream("stdout", stdout)
	go b.captureDaemonStream("stderr", stderr)

	go func() {
		done <- cmd.Wait()

		close(done)
	}()

	b.daemonMu.Lock()
	b.daemonCmd = cmd
	b.daemonDone = done
	b.daemonMu.Unlock()

	return nil
}

func (b *Backend) captureDaemonStream(stream string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		b.appendDaemonLogLine(fmt.Sprintf("%s: %s", stream, line))
		slog.Debug("signal-cli daemon", "stream", stream, "line", line)
	}

	if err := scanner.Err(); err != nil {
		b.appendDaemonLogLine(fmt.Sprintf("%s scanner error: %v", stream, err))
		slog.Debug("signal-cli daemon stream error", "stream", stream, "error", err)
	}
}

func (b *Backend) appendDaemonLogLine(line string) {
	b.daemonMu.Lock()
	defer b.daemonMu.Unlock()

	const maxLines = 40

	b.daemonLogTail = append(b.daemonLogTail, line)
	if len(b.daemonLogTail) > maxLines {
		b.daemonLogTail = b.daemonLogTail[len(b.daemonLogTail)-maxLines:]
	}
}

func (b *Backend) resetDaemonLogTail() {
	b.daemonMu.Lock()
	b.daemonLogTail = nil
	b.daemonMu.Unlock()
}

func (b *Backend) daemonLogTailString() string {
	b.daemonMu.Lock()
	defer b.daemonMu.Unlock()

	if len(b.daemonLogTail) == 0 {
		return "(no daemon output captured)"
	}

	return strings.Join(b.daemonLogTail, " | ")
}

func (b *Backend) stopDaemon() {
	b.daemonMu.Lock()
	cmd := b.daemonCmd
	done := b.daemonDone
	b.daemonCmd = nil
	b.daemonDone = nil
	b.daemonMu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	// signal-cli handles SIGTERM gracefully (flushes state, closes DB).
	// Give it time before escalating to SIGKILL.
	_ = cmd.Process.Signal(syscall.SIGTERM)

	if done != nil {
		select {
		case <-done:
			return
		case <-time.After(5 * time.Second):
			slog.Warn("signal: daemon did not exit after SIGTERM, sending SIGKILL")
		}
	}

	_ = cmd.Process.Kill()

	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			slog.Error("signal: daemon did not exit after SIGKILL")
		}
	}
}

func (b *Backend) waitForSocket(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(15 * time.Second)

	dialer := net.Dialer{Timeout: 500 * time.Millisecond}

	for {
		conn, err := dialer.DialContext(ctx, "unix", b.cfg.SocketPath)
		if err == nil {
			return conn, nil
		}

		if ctx.Err() != nil {
			return nil, fmt.Errorf("connecting to signal daemon socket: %w", ctx.Err())
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for signal daemon socket %s (daemon output: %s)", b.cfg.SocketPath, b.daemonLogTailString())
		}

		b.daemonMu.Lock()
		done := b.daemonDone
		b.daemonMu.Unlock()

		if done != nil {
			select {
			case daemonErr := <-done:
				tail := b.daemonLogTailString()
				if daemonErr != nil {
					return nil, fmt.Errorf("signal daemon exited before socket ready: %w (daemon output: %s)", daemonErr, tail)
				}

				return nil, fmt.Errorf("signal daemon exited before socket ready (daemon output: %s)", tail)
			default:
			}
		}

		time.Sleep(200 * time.Millisecond)
	}
}

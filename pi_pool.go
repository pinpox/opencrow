package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PiPool manages per-room pi processes.
type PiPool struct {
	cfg       PiConfig
	mu        sync.Mutex
	processes map[string]*PiProcess
}

// NewPiPool creates a new process pool.
func NewPiPool(cfg PiConfig) *PiPool {
	return &PiPool{
		cfg:       cfg,
		processes: make(map[string]*PiProcess),
	}
}

// Get returns an existing live pi process for the room, or spawns a new one.
func (pool *PiPool) Get(ctx context.Context, roomID string) (*PiProcess, error) {
	pool.mu.Lock()

	if p, ok := pool.processes[roomID]; ok && p.IsAlive() {
		pool.mu.Unlock()

		return p, nil
	}

	// Remove stale entry if present
	delete(pool.processes, roomID)

	// Hold lock while starting to prevent duplicate processes for the same room.
	p, err := StartPi(ctx, pool.cfg, roomID)
	if err != nil {
		pool.mu.Unlock()

		return nil, err
	}

	pool.processes[roomID] = p
	pool.mu.Unlock()

	return p, nil
}

// Remove kills and removes the pi process for a room.
// This is called on errors so the next message gets a fresh process.
func (pool *PiPool) Remove(roomID string) {
	pool.mu.Lock()
	p, ok := pool.processes[roomID]
	delete(pool.processes, roomID)
	pool.mu.Unlock()

	if ok {
		slog.Info("removing pi process", "room", roomID)
		p.Kill()
	}
}

// Rooms returns the room IDs of all alive pi processes.
func (pool *PiPool) Rooms() []string {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	rooms := make([]string, 0, len(pool.processes))

	for roomID, p := range pool.processes {
		if p.IsAlive() {
			rooms = append(rooms, roomID)
		}
	}

	return rooms
}

// StopAll kills all managed pi processes.
func (pool *PiPool) StopAll() {
	pool.mu.Lock()

	procs := make(map[string]*PiProcess, len(pool.processes))
	maps.Copy(procs, pool.processes)

	pool.processes = make(map[string]*PiProcess)
	pool.mu.Unlock()

	for roomID, p := range procs {
		slog.Info("stopping pi process", "room", roomID)
		p.Kill()
	}
}

// StartIdleReaper starts a goroutine that periodically kills processes
// that have been idle longer than the configured timeout.
func (pool *PiPool) StartIdleReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pool.reapIdle()
			}
		}
	}()
}

// SkillsSummary returns a formatted list of loaded skill paths.
func (pool *PiPool) SkillsSummary() string {
	skills := pool.cfg.Skills
	if len(skills) == 0 {
		return "No skills loaded."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d skill(s) loaded:\n", len(skills)))

	for _, s := range skills {
		sb.WriteString(fmt.Sprintf("- %s\n", filepath.Base(s)))
	}

	return sb.String()
}

func (pool *PiPool) reapIdle() {
	pool.mu.Lock()

	var toReap []string

	for roomID, p := range pool.processes {
		if !p.IsAlive() || time.Since(p.LastUse()) > pool.cfg.IdleTimeout {
			toReap = append(toReap, roomID)
		}
	}

	pool.mu.Unlock()

	for _, roomID := range toReap {
		slog.Info("reaping idle pi process", "room", roomID)
		pool.Remove(roomID)
	}
}

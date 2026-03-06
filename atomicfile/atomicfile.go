// Package atomicfile provides crash-safe file writing via temp+rename.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write atomically writes data to destPath by writing to a temporary file
// in the same directory and renaming it into place. This ensures a crash
// mid-write cannot corrupt the target file.
func Write(destPath string, data []byte) error {
	dir := filepath.Dir(destPath)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp_*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)

		return fmt.Errorf("writing temp file: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)

		return fmt.Errorf("syncing temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("renaming into place: %w", err)
	}

	dirFd, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening directory for sync: %w", err)
	}

	if err := dirFd.Sync(); err != nil {
		dirFd.Close()

		return fmt.Errorf("syncing directory: %w", err)
	}

	if err := dirFd.Close(); err != nil {
		return fmt.Errorf("closing directory: %w", err)
	}

	return nil
}

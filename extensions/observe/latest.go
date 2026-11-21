package observe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// afterPartialWrite, when set by a test, runs after the first half of the temp file is written.
var afterPartialWrite func()

// renameFile is os.Rename; a test replaces it to make the rename fail.
var renameFile = os.Rename

var errNoHome = errors.New("home directory unknown")

func marshalLatest(l Latest) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(l); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return buf.Bytes(), nil
}

// writeLatest replaces path with data through a 0600 temp file in the same directory and a rename,
// so a reader sees the old file or the new one, never a partial one.
func writeLatest(path string, data []byte) (err error) {
	if path == "" {
		return errNoHome
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".latest-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	// CreateTemp already uses 0600; Chmod states it for umask-independent clarity.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	half := len(data) / 2 //nolint:mnd // the middle of the file, for the partial-write test hook
	if _, err := f.Write(data[:half]); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if afterPartialWrite != nil {
		afterPartialWrite()
	}
	if _, err := f.Write(data[half:]); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := renameFile(tmp, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

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

// writeError is a failed step of writeLatest; step is a fixed summary without the path.
type writeError struct {
	step string
	err  error
}

func (e *writeError) Error() string { return e.step + ": " + e.err.Error() }

func (e *writeError) Unwrap() error { return e.err }

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
		return &writeError{"create the state directory", err}
	}
	f, err := os.CreateTemp(dir, ".latest-*.json")
	if err != nil {
		return &writeError{"create the temp file", err}
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
		return &writeError{"set the temp file mode", err}
	}
	half := len(data) / 2 //nolint:mnd // the middle of the file, for the partial-write test hook
	if _, err := f.Write(data[:half]); err != nil {
		return &writeError{"write the temp file", err}
	}
	if afterPartialWrite != nil {
		afterPartialWrite()
	}
	if _, err := f.Write(data[half:]); err != nil {
		return &writeError{"write the temp file", err}
	}
	if err := f.Sync(); err != nil {
		return &writeError{"sync the temp file", err}
	}
	if err := f.Close(); err != nil {
		return &writeError{"close the temp file", err}
	}
	if err := renameFile(tmp, path); err != nil {
		return &writeError{"replace latest.json", err}
	}
	return nil
}

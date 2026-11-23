package classify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// lockRetry is how often a busy report lock is retried.
const lockRetry = 50 * time.Millisecond

// Test seams: the rename, and a hook run after the temp file is written, before the lock.
var (
	renameFile   = os.Rename
	beforeLock   func()
	afterCompare func()
)

// reportPath is .local/state/stack/jev-classify/latest.json under home.
func reportPath(home string) string {
	return filepath.Join(home, ".local", "state", "stack", "jev-classify", "latest.json")
}

// Publication outcomes.
const (
	published = iota
	dropped
	failed
)

// publishResult is how publication ended; cause is a fixed string for a failure.
type publishResult struct {
	outcome int
	cause   string
}

// publish writes data as the report at path (§3.2 Publication): a synced temp file in its
// directory, then under latest.lock (gofrs/flock, acquired until lockBy) the existing report is
// read and replaced only when its window ends earlier, or at the same time with a run started no
// later; otherwise this run's report is dropped. The temp file never outlives the call.
func publish(
	ctx context.Context, path string, data []byte, until, started time.Time, lockBy time.Time,
) publishResult {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return publishResult{failed, "cannot create the report directory"}
	}
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return publishResult{failed, err.Error()}
	}
	defer func() { _ = os.Remove(tmp) }()
	if beforeLock != nil {
		beforeLock()
	}
	lock := flock.New(filepath.Join(dir, "latest.lock"), flock.SetPermissions(0o600))
	lctx, cancel := context.WithDeadline(ctx, lockBy)
	defer cancel()
	locked, err := lock.TryLockContext(lctx, lockRetry)
	if err != nil || !locked {
		return publishResult{failed, "report lock busy"}
	}
	defer func() { _ = lock.Unlock() }()
	replace, err := newer(path, until, started)
	switch {
	case err != nil:
		return publishResult{failed, "cannot read the existing report"}
	case !replace:
		return publishResult{outcome: dropped}
	}
	if afterCompare != nil {
		afterCompare()
	}
	if err := renameFile(tmp, path); err != nil {
		return publishResult{failed, "cannot replace the report"}
	}
	return publishResult{outcome: published}
}

// newer reports whether this run's report should replace the one at path: there is none or it
// is malformed, its window ends earlier, or it ends at the same time with a run started no later
// than this one. A report that exists but cannot be read is an error: it may be newer.
func newer(path string, until, started time.Time) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the report: %w", err)
	}
	var old struct {
		Window Window `json:"window"`
		Run    struct {
			Started string `json:"started"`
		} `json:"run"`
	}
	if json.Unmarshal(data, &old) != nil {
		return true, nil //nolint:nilerr // a malformed report is replaced
	}
	ou, ok1 := parseTime(old.Window.Until)
	ostart, ok2 := parseTime(old.Run.Started)
	if !ok1 || !ok2 {
		return true, nil
	}
	return ou.Before(until) || ou.Equal(until) && !ostart.After(started), nil
}

var errWrite = errors.New("cannot write the report")

func writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".latest-*.json")
	if err != nil {
		return "", fmt.Errorf("%w: temp file", errWrite)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("%w: temp file mode", errWrite)
	}
	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("%w: temp file", errWrite)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("%w: sync", errWrite)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("%w: close", errWrite)
	}
	ok = true
	return tmp, nil
}

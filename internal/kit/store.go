// Package kit owns a session's list of open kits (`<id>.kits`), kit directories and the kit context
// renderer shared by `stack kit open` and compact restore.
package kit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/shuymn/stack/internal/paths"
)

const (
	// OpenTimeout bounds how long `kit open` and `kit close` wait for the `.kits` lock.
	OpenTimeout = 5 * time.Second
	// lockPoll is the interval between attempts to take the lock.
	lockPoll = 25 * time.Millisecond
)

var (
	// ErrLocked reports that another process held the `.kits` lock past the timeout.
	ErrLocked = errors.New("locked by another process")
	// ErrInvalidSessionID reports a session id that fails paths.ValidSessionID.
	ErrInvalidSessionID = errors.New("invalid session id")
	// ErrInvalidSlug reports a slug that fails paths.ValidSlug.
	ErrInvalidSlug = errors.New("invalid kit slug")
)

// Store is the set of `<id>.kits` files under Home's sessions dir. Every access, read or write,
// holds an exclusive OS lock on `<id>.lock`. The OS releases the lock when its holder exits, so
// there is no stale reclaim; the lock file is never deleted.
type Store struct {
	Home string
	// Timeout bounds the wait for the lock; zero means OpenTimeout.
	Timeout time.Duration
}

// List returns the slugs recorded for id, in order.
func (s Store) List(ctx context.Context, id string) ([]string, error) {
	var out []string
	err := s.locked(ctx, id, func(root *os.Root, name string) error {
		var err error
		out, err = readKits(root, name)
		return err
	})
	return out, err
}

// Add appends slug to id's list unless it is already there.
func (s Store) Add(ctx context.Context, id, slug string) error {
	return s.update(ctx, id, slug, func(list []string) []string {
		if slices.Contains(list, slug) {
			return list
		}
		return append(list, slug)
	})
}

// Remove drops slug from id's list; removing an absent slug is a no-op. An emptied list deletes the
// file.
func (s Store) Remove(ctx context.Context, id, slug string) error {
	return s.update(ctx, id, slug, func(list []string) []string {
		return slices.DeleteFunc(slices.Clone(list), func(v string) bool { return v == slug })
	})
}

func (s Store) update(ctx context.Context, id, slug string, change func([]string) []string) error {
	if !paths.ValidSlug(slug) {
		return fmt.Errorf("%w: %q", ErrInvalidSlug, slug)
	}
	return s.locked(ctx, id, func(root *os.Root, name string) error {
		old, err := readKits(root, name)
		if err != nil {
			return err
		}
		updated := change(old)
		if slices.Equal(old, updated) {
			return nil
		}
		if len(updated) == 0 {
			if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", name, err)
			}
			return nil
		}
		return writeKits(root, name, updated)
	})
}

// locked runs fn with the sessions dir opened as a root and id's lock held. fn receives the
// `.kits` file name relative to that root.
func (s Store) locked(
	ctx context.Context,
	id string,
	fn func(root *os.Root, name string) error,
) error {
	if !paths.ValidSessionID(id) {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	dir := paths.SessionsDir(s.Home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create sessions dir: %w", err)
	}
	name := filepath.Base(paths.SessionKitsFile(s.Home, id))

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = OpenTimeout
	}
	lockCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lock := flock.New(paths.SessionLockFile(s.Home, id))
	defer func() { _ = lock.Close() }()
	if ok, err := lock.TryLockContext(lockCtx, lockPoll); !ok {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("%s is %w", name, ErrLocked)
		}
		return fmt.Errorf("lock %s: %w", name, err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open sessions dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	return fn(root, name)
}

// readKits returns the valid, de-duplicated slugs of name in file order; a missing file is empty.
// Lines are trimmed, so CRLF files read as LF ones.
func readKits(root *os.Root, name string) ([]string, error) {
	data, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		slug := strings.TrimSpace(line)
		if paths.ValidSlug(slug) && !slices.Contains(out, slug) {
			out = append(out, slug)
		}
	}
	return out, nil
}

// writeKits replaces name with list through a temp file in the same dir. The temp handle is
// closed before the rename, which Windows requires; the lock keeps every other handle closed.
func writeKits(root *os.Root, name string, list []string) error {
	tmp := name + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	_, werr := f.WriteString(strings.Join(list, "\n") + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("replace %s: %w", name, err)
	}
	return nil
}

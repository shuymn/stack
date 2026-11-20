package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/kit"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc"
)

const sessionIDEnv = "STACK_SESSION_ID"

var (
	errNoSessionID      = errors.New(sessionIDEnv + " unset; there is no record to change")
	errInvalidSessionID = errors.New("invalid " + sessionIDEnv)
)

// refusal keeps err's message and marks it as a refusal (exit 1).
type refusal struct{ err error }

func (r refusal) Error() string   { return r.err.Error() }
func (r refusal) Unwrap() []error { return []error{r.err, ErrRefused} }

// storeError maps a lock timeout to a refusal; the caller retries the whole command.
func storeError(err error) error {
	if errors.Is(err, kit.ErrLocked) {
		return refusal{err}
	}
	return err
}

// sessionID returns the valid STACK_SESSION_ID, or "" when it is unset.
func sessionID(env Env) (string, error) {
	id := ""
	if env.Getenv != nil {
		id = env.Getenv(sessionIDEnv)
	}
	if id != "" && !paths.ValidSessionID(id) {
		return "", fmt.Errorf("%w: %q", errInvalidSessionID, id)
	}
	return id, nil
}

func checkSlug(slug string) error {
	if !paths.ValidSlug(slug) {
		return fmt.Errorf("%w: %q", kit.ErrInvalidSlug, slug)
	}
	return nil
}

func runKitOpen(ctx context.Context, env Env, inv invocation) error {
	slug := inv.args[0]
	if err := checkSlug(slug); err != nil {
		return err
	}
	if env.Home == "" {
		return paths.ErrNoHome
	}
	id, err := sessionID(env)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	// A bad entry name is a config error found before anything changes.
	if err := ext.CheckNames(cfg.Stages.Entry); err != nil {
		return fmt.Errorf("%w: %s: [stages].entry: %w", config.ErrInvalid,
			paths.ConfigFile(env.Home), err)
	}
	root, err := filepath.Abs(paths.FromConfig(env.Home, cfg.Kit.Root))
	if err != nil {
		return fmt.Errorf("kit root: %w", err)
	}

	// With the lock held, create the kit dir and record the slug only once that succeeded; render
	// after releasing the lock. A lock timeout changes nothing and prints nothing.
	dir := paths.KitDir(root, slug)
	if id == "" {
		err = kit.EnsureDir(dir)
	} else {
		err = storeError((kit.Store{Home: env.Home}).Open(ctx, id, slug, func() error {
			return kit.EnsureDir(dir)
		}))
	}
	if err != nil {
		return err
	}

	boardCtx, cancel := context.WithTimeout(ctx, kit.BoardDeadline)
	defer cancel()
	r := kit.Renderer{
		Runner:   proc.Exec{},
		Home:     env.Home,
		Getenv:   env.Getenv,
		Handover: cfg.Handover,
	}
	blocks := r.Render(boardCtx, dir)
	blocks = append(blocks, entryBlocks(ctx, env, cfg.Stages.Entry)...)
	if _, err := fmt.Fprint(env.Stdout, kit.Join(blocks)); err != nil {
		if id != "" {
			return fmt.Errorf("write context: %w; %s is recorded, rerun `stack kit open %s` to "+
				"print it", err, slug, slug)
		}
		return fmt.Errorf("write context: %w", err)
	}

	if id == "" {
		_, _ = fmt.Fprintf(env.Stderr, "stack: %s unset; %s not recorded and will not be restored "+
			"after compaction\n", sessionIDEnv, slug)
	}
	return nil
}

func runKitClose(ctx context.Context, env Env, inv invocation) error {
	slug := inv.args[0]
	if err := checkSlug(slug); err != nil {
		return err
	}
	id, err := sessionID(env)
	if err != nil {
		return err
	}
	if id == "" {
		return errNoSessionID
	}
	if env.Home == "" {
		return paths.ErrNoHome
	}
	return storeError((kit.Store{Home: env.Home}).Remove(ctx, id, slug))
}

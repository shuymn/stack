package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shuymn/stack/internal/config"
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
	cfg, warnings, err := config.Load(paths.ConfigFile(env.Home))
	if err != nil {
		return err
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(env.Stderr, "stack: %s\n", w)
	}
	root, err := filepath.Abs(paths.Expand(env.Home, cfg.Kit.Root))
	if err != nil {
		return fmt.Errorf("kit root: %w", err)
	}

	// Record before touching the kit dir, so a lock timeout changes nothing and prints nothing.
	if id != "" {
		if err := (kit.Store{Home: env.Home}).Add(ctx, id, slug); err != nil {
			return storeError(err)
		}
	}
	dir := paths.KitDir(root, slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create kit dir: %w", err)
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
	blocks = append(blocks, entryStage(ctx, env, cfg, slug)...)
	_, _ = fmt.Fprint(env.Stdout, kit.Join(blocks))

	if id == "" {
		_, _ = fmt.Fprintf(env.Stderr, "stack: %s unset; %s not recorded and will not be restored "+
			"after compaction\n", sessionIDEnv, slug)
	}
	return nil
}

// entryStage is the seam for the entry-stage extensions that run after a successful `kit open`
// (§4.1, §4.7): each result's context is appended in the renderer's block format, and a block
// there is shown, not enforced. Extensions arrive with the `ext` package; until then it adds
// nothing.
func entryStage(_ context.Context, _ Env, _ config.Config, _ string) []kit.Block {
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

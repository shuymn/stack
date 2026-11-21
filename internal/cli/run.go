package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shuymn/stack/internal/checkpub"
	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/kit"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc"
)

var (
	errReplayStage    = errors.New("replay request stage differs from the command's stage")
	errReplayConflict = errors.New("--replay cannot be combined with --range or --prompt-file")
	errEmptyFlag      = errors.New("empty value")
	errExpect         = errors.New("aggregate status differs from the replay's expect")
	errBlocked        = errors.New("block")
)

// runRun implements `stack run <stage>`: it runs the stage's extensions and prints the aggregate as
// JSON. block, which only outbound keeps, exits 1; with a replay's expect, exit 1 means the
// aggregate differs from it instead.
func runRun(ctx context.Context, env Env, inv invocation) error {
	stage := inv.args[0]
	set := map[string]bool{}
	inv.flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	// Arguments first, so a usage error does not depend on the config.
	if set["replay"] && (set["range"] || set["prompt-file"]) {
		return errReplayConflict
	}
	// An explicitly empty value is not an omitted flag: `--range=` must not fall back.
	for _, name := range []string{"range", "prompt-file"} {
		if set[name] && inv.flags.Lookup(name).Value.String() == "" {
			return fmt.Errorf("--%s: %w", name, errEmptyFlag)
		}
	}
	cfg := &lazyConfig{env: env}
	names, err := stageNames(env, inv, cfg, stage)
	if err != nil {
		return err
	}
	var request []byte
	expect := ""
	if set["replay"] {
		request, expect, err = replayRequest(env, inv.flags.Lookup("replay").Value.String(), stage)
	} else {
		request, err = buildRequest(ctx, env, inv, cfg, stage)
	}
	if err != nil {
		return err
	}

	plans := ext.Resolve(names)
	if boolFlagValue(inv.flags, "dry-run") {
		return writeDryRun(env.Stdout, stage, plans, request)
	}
	agg := ext.Run(ctx, extOptions(env), stage, request, plans)
	out, err := json.Marshal(agg)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if _, err := fmt.Fprintf(env.Stdout, "%s\n", out); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	// The aggregate is compared after demotion, so only a stage that can block exits 1 on it.
	switch {
	case expect != "" && agg.Status != expect:
		return refusal{fmt.Errorf("%w: got %s, want %s", errExpect, agg.Status, expect)}
	case expect == "" && agg.Status == ext.StatusBlock:
		return refusal{fmt.Errorf("stage %s: %w", stage, errBlocked)}
	}
	return nil
}

// lazyConfig loads the config, printing its warnings, at most once.
type lazyConfig struct {
	env    Env
	loaded bool
	cfg    config.Config
	err    error
}

func (l *lazyConfig) get() (config.Config, error) {
	if !l.loaded {
		l.cfg, l.err = loadConfig(l.env)
		l.loaded = true
	}
	return l.cfg, l.err
}

// stageNames returns the --ext names, or else the stage's configured ones; an invalid name is a
// usage error from --ext and a config error from the config.
func stageNames(env Env, inv invocation, lc *lazyConfig, stage string) ([]string, error) {
	if l, ok := inv.flags.Lookup("ext").Value.(*stringList); ok && len(*l) > 0 {
		return *l, ext.CheckNames(*l)
	}
	cfg, err := lc.get()
	if err != nil {
		return nil, err
	}
	names, err := ext.Names(cfg.Stages, stage)
	if err != nil {
		return nil, err
	}
	return names, checkStageNames(env, stage, names)
}

// checkStageNames reports an invalid name configured under [stages].<stage> as a config error.
func checkStageNames(env Env, stage string, names []string) error {
	if err := ext.CheckNames(names); err != nil {
		return fmt.Errorf("%w: %s: [stages].%s: %w", config.ErrInvalid,
			paths.ConfigFile(env.Home), stage, err)
	}
	return nil
}

// replayRequest reads a --replay fixture whose request stage must be stage.
func replayRequest(env Env, file, stage string) ([]byte, string, error) {
	text, err := readInputFile(env.Home, file)
	if err != nil {
		return nil, "", fmt.Errorf("--replay: %w", err)
	}
	replay, err := ext.ParseReplay([]byte(text))
	if err != nil {
		return nil, "", err
	}
	if replay.Stage != stage {
		return nil, "", fmt.Errorf("%w: %q, not %q", errReplayStage, replay.Stage, stage)
	}
	return replay.Request, replay.Expect, nil
}

// buildRequest builds the request from the repository around the working directory, --range and
// --prompt-file; an entry or merged request also lists the session's kits. A merged request has no
// merge field: only `stack pr merge` knows a verified merge.
func buildRequest(
	ctx context.Context, env Env, inv invocation, lc *lazyConfig, stage string,
) ([]byte, error) {
	prompt := ""
	if f := inv.flags.Lookup("prompt-file").Value.String(); f != "" {
		text, err := readInputFile(env.Home, f)
		if err != nil {
			return nil, fmt.Errorf("--prompt-file: %w", err)
		}
		prompt = text
	}
	rng := inv.flags.Lookup("range").Value.String()
	req, err := ext.BuildRequest(ctx, hostGit(), stage, rng, prompt)
	if err != nil {
		return nil, err
	}
	if stage == ext.StageEntry || stage == ext.StageMerged {
		req.Kits = runKits(ctx, env, lc)
	}
	request, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return request, nil
}

func writeDryRun(w io.Writer, stage string, plans []ext.Plan, request []byte) error {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "stage: %s\nplan:\n", stage)
	if len(plans) == 0 {
		b.WriteString("  (no extensions)\n")
	}
	for _, p := range plans {
		_, _ = fmt.Fprintf(&b, "  %s\n", p)
	}
	_, _ = fmt.Fprintf(&b, "request: %s\n", request)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

// loadConfig loads the config and prints its warnings.
func loadConfig(env Env) (config.Config, error) {
	if env.Home == "" {
		return config.Config{}, errNoHome
	}
	cfg, warnings, err := config.Load(paths.ConfigFile(env.Home))
	if err != nil {
		return config.Config{}, err
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(env.Stderr, "stack: %s\n", w)
	}
	return cfg, nil
}

// hostGit runs git on the repository around the current directory, never one named by variables a
// git hook exported.
func hostGit() ext.Git {
	return ext.Git{Runner: proc.Exec{}, Env: checkpub.DropRepoVars(os.Environ())}
}

// kitsLockTimeout bounds the wait for the `.kits` lock when a request lists the session's kits.
const kitsLockTimeout = time.Second

// requestKits returns the kits field of a live request: the kits recorded for id whose dir under
// kitRoot exists, in `.kits` order, or an empty list without a valid id. When the `.kits` lock is
// not taken within a second, or the list cannot be read, it is empty and stderr says why.
func requestKits(ctx context.Context, env Env, id, kitRoot string) *[]ext.Kit {
	kits := []ext.Kit{}
	if env.Home == "" || !paths.ValidSessionID(id) {
		return &kits
	}
	refs, err := (kit.Store{Home: env.Home, Timeout: kitsLockTimeout}).Refs(ctx, id, kitRoot)
	switch {
	case errors.Is(err, kit.ErrLocked):
		_, _ = fmt.Fprintf(env.Stderr, "stack: %s.kits locked; kits not sent\n", id)
	case err != nil:
		_, _ = fmt.Fprintf(env.Stderr, "stack: %v; kits not sent\n", err)
	}
	for _, r := range refs {
		kits = append(kits, ext.Kit{Slug: r.Slug, Dir: r.Dir})
	}
	return &kits
}

// runKits returns the kits of a live entry or merged request; only a valid session id loads the
// config for the kit root. A config that cannot be loaded costs only the kits: they are [] and
// stderr says why, so an --ext run still runs as it did before requests carried kits.
func runKits(ctx context.Context, env Env, lc *lazyConfig) *[]ext.Kit {
	id := ""
	if env.Getenv != nil {
		id = env.Getenv(sessionIDEnv)
	}
	root := ""
	if paths.ValidSessionID(id) && env.Home != "" {
		cfg, err := lc.get()
		if err == nil {
			root, err = kitRoot(env, cfg)
		}
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "stack: %v; kits not sent\n", err)
			return &[]ext.Kit{}
		}
	}
	return requestKits(ctx, env, id, root)
}

// kitRoot is the absolute kit root of cfg.
func kitRoot(env Env, cfg config.Config) (string, error) {
	root, err := filepath.Abs(paths.FromConfig(env.Home, cfg.Kit.Root))
	if err != nil {
		return "", fmt.Errorf("kit root: %w", err)
	}
	return root, nil
}

func extOptions(env Env) ext.Options {
	o := ext.Options{Runner: proc.Exec{}, Stderr: env.Stderr}
	if env.Getenv != nil {
		o.SessionID = env.Getenv(sessionIDEnv)
	}
	return o
}

// blockWriter writes kit blocks as they become known, in the bytes kit.Join gives for all of them.
type blockWriter struct {
	w       io.Writer
	started bool
}

func (b *blockWriter) write(blocks ...kit.Block) error {
	if len(blocks) == 0 {
		return nil
	}
	s := kit.Join(blocks)
	if b.started {
		s = "\n" + s
	}
	b.started = true
	if _, err := io.WriteString(b.w, s); err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	return nil
}

// writeEntryStage runs the entry stage after `kit open` with an empty range and prompt and the
// session's kits under kitRoot, and writes each result that has something to show, as soon as it
// completes, as a block in the kit renderer's format. names must be valid. After a write error no
// further extension starts.
func writeEntryStage(
	ctx context.Context, env Env, id, kitRoot string, names []string, bw *blockWriter,
) error {
	plans := ext.Resolve(names)
	var request []byte
	if ext.AnyToRun(plans) {
		req, err := ext.BuildRequest(ctx, hostGit(), ext.StageEntry, "", "")
		if err == nil {
			req.Kits = requestKits(ctx, env, id, kitRoot)
			request, err = json.Marshal(req)
		}
		if err != nil {
			return bw.write(kit.Block{Body: fmt.Sprintf("[entry extensions skipped: %v]", err)})
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var werr error
	ext.Stream(ctx, extOptions(env), ext.StageEntry, request, plans, func(r ext.Result) {
		if b, shown := kit.EntryBlock(r); werr == nil && shown {
			if werr = bw.write(b); werr != nil {
				cancel()
			}
		}
	})
	return werr
}

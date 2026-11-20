package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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
	errExpect         = errors.New("aggregate status differs from the replay's expect")
	errBlocked        = errors.New("block")
)

// runRun implements `stack run <stage>`: it runs the stage's extensions and prints the aggregate as
// JSON. block exits 1; with a replay's expect, exit 1 means the aggregate differs from it instead.
func runRun(ctx context.Context, env Env, inv invocation) error {
	stage := inv.args[0]
	set := map[string]bool{}
	inv.flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	// Arguments first, so a usage error does not depend on the config.
	if set["replay"] && (set["range"] || set["prompt-file"]) {
		return errReplayConflict
	}
	names, err := stageNames(env, inv, stage)
	if err != nil {
		return err
	}
	var request []byte
	expect := ""
	if set["replay"] {
		request, expect, err = replayRequest(env, inv.flags.Lookup("replay").Value.String(), stage)
	} else {
		request, err = buildRequest(ctx, env, inv, stage)
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
	switch {
	case expect != "" && agg.Status != expect:
		return refusal{fmt.Errorf("%w: got %s, want %s", errExpect, agg.Status, expect)}
	case expect == "" && agg.Status == ext.StatusBlock:
		return refusal{fmt.Errorf("stage %s: %w", stage, errBlocked)}
	}
	return nil
}

// stageNames returns the --ext names, or else the stage's configured ones; an invalid name is a
// usage error from --ext and a config error from the config.
func stageNames(env Env, inv invocation, stage string) ([]string, error) {
	if l, ok := inv.flags.Lookup("ext").Value.(*stringList); ok && len(*l) > 0 {
		return *l, ext.CheckNames(*l)
	}
	cfg, err := loadConfig(env)
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
// --prompt-file.
func buildRequest(ctx context.Context, env Env, inv invocation, stage string) ([]byte, error) {
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

func extOptions(env Env) ext.Options {
	o := ext.Options{Runner: proc.Exec{}, Stderr: env.Stderr}
	if env.Getenv != nil {
		o.SessionID = env.Getenv(sessionIDEnv)
	}
	return o
}

// entryBlocks runs the entry stage after `kit open` with an empty range and prompt and returns each
// result that has something to show as a block in the kit renderer's format. block is shown, not
// enforced. names must be valid.
func entryBlocks(ctx context.Context, env Env, names []string) []kit.Block {
	plans := ext.Resolve(names)
	var request []byte
	if ext.AnyToRun(plans) {
		req, err := ext.BuildRequest(ctx, hostGit(), ext.StageEntry, "", "")
		if err == nil {
			request, err = json.Marshal(req)
		}
		if err != nil {
			return []kit.Block{{Body: fmt.Sprintf("[entry extensions skipped: %v]", err)}}
		}
	}
	var blocks []kit.Block
	for _, r := range ext.Run(ctx, extOptions(env), ext.StageEntry, request, plans).Results {
		if r.Status != ext.StatusPass || r.Context != "" {
			blocks = append(blocks, resultBlock(r))
		}
	}
	return blocks
}

// resultBlock renders one extension result: its context in an `extension-context` block naming
// the executable, preceded by a status line unless it passed; a result without context is a bare
// marker.
func resultBlock(r ext.Result) kit.Block {
	note := ""
	switch r.Status {
	case ext.StatusBlock:
		note = fmt.Sprintf("[extension %s: block (shown, not enforced)]", r.Name)
	case ext.StatusWarn:
		reason := r.Error
		if reason == "" {
			reason = "warn"
		}
		note = fmt.Sprintf("[extension %s: warn: %s]", r.Name, reason)
	}
	body := strings.TrimSpace(r.Context)
	if body == "" || r.Path == "" {
		return kit.Block{Body: strings.TrimSpace(note + "\n" + body)}
	}
	if note != "" {
		body = note + "\n" + body
	}
	return kit.Block{Tag: "extension-context", Path: r.Path, Body: body}
}

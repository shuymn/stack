// Package session implements the SessionStart hook: it exports STACK_SESSION_ID through
// CLAUDE_ENV_FILE on every event and, on compact only, restores the session's kits and a worker's
// rules and brief within one character budget.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/kit"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc"
)

const (
	// EnvFile names the variable through which Claude Code hands the hook a file that bash sources.
	EnvFile = "CLAUDE_ENV_FILE"
	// IDVar is the exported variable.
	IDVar = "STACK_SESSION_ID"
	// lockTimeout bounds the wait for the `.kits` lock on compact.
	lockTimeout = time.Second
	// sourceCompact is the only source that prints context.
	sourceCompact = "compact"
)

var errNoTranscript = errors.New("no transcript path")

// Params is everything the hook reads.
type Params struct {
	Input  hookio.Input
	Home   string
	Getenv func(string) string
	Config config.Config
	Runner proc.Runner
}

func (p Params) getenv(k string) string {
	if p.Getenv == nil {
		return ""
	}
	return p.Getenv(k)
}

// Start runs the hook. It returns the hook stdout (nil for none) and the errors it swallowed, which
// never cost more than the part that needed them.
func Start(ctx context.Context, p Params) ([]byte, []error) {
	var errs []error
	validID := paths.ValidSessionID(p.Input.SessionID)
	if validID {
		if err := Export(p.getenv(EnvFile), p.Input.SessionID); err != nil {
			errs = append(errs, err)
		}
	}
	if p.Input.Source != sourceCompact {
		return nil, errs
	}
	text, cerrs := compact(ctx, p, validID)
	errs = append(errs, cerrs...)
	if text == "" {
		return nil, errs
	}
	out, err := hookio.Context(text)
	if err != nil {
		return nil, append(errs, err)
	}
	return out, errs
}

// Export appends `export STACK_SESSION_ID='<id>'` to file. An empty file name (Claude Code did not
// provide one) writes nothing. The caller validates id, so it needs no quoting.
func Export(file, id string) error {
	if file == "" {
		return nil
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", EnvFile, err)
	}
	_, werr := fmt.Fprintf(f, "export %s='%s'\n", IDVar, id)
	if err := errors.Join(werr, f.Close()); err != nil {
		return fmt.Errorf("write %s: %w", EnvFile, err)
	}
	return nil
}

// kitPieces are one kit's pieces: its state file and items pieces, then its board.
type kitPieces struct {
	state []*piece
	board []*piece
}

// compact builds the compact context: worker rules and brief, then the kits of the session id, then
// the handover, within Budget.
func compact(ctx context.Context, p Params, validID bool) (string, []error) {
	var errs []error
	rules, brief, werr := worker(p)
	if werr != nil {
		errs = append(errs, werr)
	}
	var locked *piece
	var kits []kitPieces
	var handover []*piece
	if validID && p.Home != "" {
		var kerr error
		locked, kits, handover, kerr = restoreKits(ctx, p)
		if kerr != nil {
			errs = append(errs, kerr)
		}
	}

	a := newAllocator()
	for _, pc := range []*piece{rules, brief, locked} {
		if pc != nil {
			a.place(pc, 0)
		}
	}
	for _, k := range kits { // reserve a start of every kit's state files first
		for _, pc := range k.state {
			if pc.kind == kit.KindState {
				a.place(pc, reserveChars)
			}
		}
	}
	for _, k := range kits {
		for _, pc := range k.state {
			a.place(pc, blockLimit)
		}
	}
	for _, pc := range handover {
		a.place(pc, 0)
	}
	for _, k := range kits {
		for _, pc := range k.board {
			a.place(pc, 0)
		}
	}

	display := []*piece{rules, brief, locked}
	for _, k := range kits {
		display = append(display, k.state...)
		display = append(display, k.board...)
	}
	display = append(display, handover...)
	return join(display), errs
}

// workerHeader opens the worker part, so the restored text is read as the source of truth.
const workerHeader = "A compaction summarized this conversation; the verbatim worker rules and, " +
	"when available, your original brief follow.\n\n"

// worker returns the rules and brief pieces when the session is a worker, else nils. The rules piece
// carries the worker header. A transcript that cannot be read costs only the brief.
func worker(p Params) (*piece, *piece, error) {
	in, cfg := p.Input, p.Config.Worker
	prompt, perr := "", errNoTranscript
	if in.TranscriptPath != "" {
		prompt, perr = FirstPrompt(in.TranscriptPath)
	}
	isWorker := (perr == nil && hasLinePrefix(prompt, cfg.Marker)) ||
		(in.CWD != "" && paths.IsAgentWorktree(in.CWD)) ||
		(in.AgentType != "" && slices.Contains(cfg.AgentTypes, in.AgentType))
	if !isWorker {
		return nil, nil, nil
	}

	path := resolveConfigPath(p.Home, cfg.Body)
	var rules *piece
	if data, err := os.ReadFile(path); err != nil {
		rules = &piece{body: workerHeader + "[unreadable: " + path + "]", kind: "worker rules"}
	} else {
		rules = tagged("worker-rules", path, stripFrontmatter(string(data)), "worker rules")
		rules.head = workerHeader + rules.head
	}
	var brief *piece
	if perr == nil && prompt != "" {
		brief = tagged("original-brief", "", prompt, "brief")
	}
	if errors.Is(perr, errNoTranscript) {
		perr = nil
	}
	return rules, brief, perr
}

// hasLinePrefix reports whether a line of s starts with marker; an empty marker never matches.
func hasLinePrefix(s, marker string) bool {
	if marker == "" {
		return false
	}
	for line := range strings.SplitSeq(s, "\n") {
		if strings.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

// stripFrontmatter removes a leading `---` frontmatter block, CRLF-tolerant, and trims the rest.
func stripFrontmatter(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		if _, body, ok := strings.Cut(rest, "\n---\n"); ok {
			s = body
		}
	}
	return strings.TrimSpace(s)
}

// resolveConfigPath expands `~` in a configured path; a relative one is taken against the config
// file's directory, like the handover paths.
func resolveConfigPath(home, p string) string {
	path := paths.Expand(home, p)
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(paths.ConfigFile(home)), path)
	}
	return path
}

// restoreKits renders the kits recorded for the session id whose dirs exist, and the handover once
// when any kit rendered. A `.kits` lock not acquired within lockTimeout omits the kits with a
// marker.
func restoreKits(ctx context.Context, p Params) (*piece, []kitPieces, []*piece, error) {
	slugs, err := (kit.Store{Home: p.Home, Timeout: lockTimeout}).List(ctx, p.Input.SessionID)
	switch {
	case errors.Is(err, kit.ErrLocked):
		return &piece{body: "[kits omitted: .kits locked]", kind: "kits"}, nil, nil, nil
	case err != nil:
		return &piece{body: "[kits omitted: .kits unreadable]", kind: "kits"}, nil, nil, err
	}
	root, err := filepath.Abs(paths.Expand(p.Home, p.Config.Kit.Root))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("kit root: %w", err)
	}

	boardCtx, cancel := context.WithTimeout(ctx, kit.BoardDeadline)
	defer cancel()
	r := kit.Renderer{Runner: p.Runner, Home: p.Home, Getenv: p.Getenv, Handover: p.Config.Handover}
	var kits []kitPieces
	for _, slug := range slugs {
		dir := paths.KitDir(root, slug)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		state := kit.StateBlocks(dir)
		kits = append(kits, kitPieces{
			state: pieces(state),
			board: pieces(r.BoardBlocks(boardCtx, dir, state)),
		})
	}
	if len(kits) == 0 {
		return nil, nil, nil, nil
	}
	return nil, kits, pieces(r.HandoverBlocks()), nil
}

func pieces(blocks []kit.Block) []*piece {
	out := make([]*piece, len(blocks))
	for i, b := range blocks {
		if b.Tag == "" {
			out[i] = &piece{body: b.Body, kind: b.Kind}
		} else {
			out[i] = tagged(b.Tag, b.Path, b.Body, b.Kind)
		}
	}
	return out
}

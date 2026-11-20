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
	// boardCutBound bounds the characters cut from a board's output: proc caps its stdout at 1 MiB.
	boardCutBound = 1 << 20
)

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

// Export appends `export STACK_SESSION_ID='<id>'` to file, on a line of its own: a file not ending
// in a newline gets one first, so its last line cannot swallow the assignment. An empty file name
// (Claude Code did not provide one) writes nothing. The caller validates id, so it needs no quoting.
func Export(file, id string) error {
	if file == "" {
		return nil
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", EnvFile, err)
	}
	line := fmt.Sprintf("export %s='%s'\n", IDVar, id)
	last := make([]byte, 1)
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			line = "\n" + line
		}
	}
	_, werr := f.WriteString(line)
	if err := errors.Join(werr, f.Close()); err != nil {
		return fmt.Errorf("write %s: %w", EnvFile, err)
	}
	return nil
}

// kitPieces is one kit as compact restores it: its dir, its rendered state blocks (NORTH.md,
// state.md, items and markers) and their pieces, and its board pieces once boards ran.
type kitPieces struct {
	dir    string
	blocks []kit.Block
	state  []*piece
	board  []*piece
}

// compact builds the compact context within Budget: worker rules and brief, then the kits of the
// session id, then the handover. Before any text is placed, the brief and every kit state block
// reserve their omitted markers (each naming its file), and a `.kits` failure its marker, so no
// part disappears silently. Boards run last and only while their block can still fit.
func compact(ctx context.Context, p Params, validID bool) (string, []error) {
	var errs []error
	rules, brief, werr := worker(p)
	if werr != nil {
		errs = append(errs, werr)
	}
	r := kit.Renderer{Runner: p.Runner, Home: p.Home, Getenv: p.Getenv, Handover: p.Config.Handover}
	var kits []kitPieces
	var handover []*piece
	if validID && p.Home != "" {
		var kerr error
		kits, kerr = restoreKits(ctx, p)
		if kerr != nil {
			errs = append(errs, kerr)
		}
		if len(kits) > 0 && kits[0].dir != "" {
			handover = pieces(r.HandoverBlocks())
		}
	}

	a := newAllocator()
	a.reserve(brief)
	for _, k := range kits {
		for _, pc := range k.state {
			if pc.kind == kit.KindState || pc.head == "" {
				a.reserve(pc)
			}
		}
	}
	a.place(rules, 0)
	a.place(brief, 0)
	for _, k := range kits { // the start of every kit's state files before any kit gets more
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
	runBoards(ctx, a, r, kits)

	display := []*piece{rules, brief}
	for _, k := range kits {
		display = append(display, k.state...)
		display = append(display, k.board...)
	}
	display = append(display, handover...)
	return join(display), errs
}

// runBoards runs, in kit order under one deadline, each kit's board whose block can still fit the
// budget, and places its output; a board that cannot fit is not launched and leaves an omitted
// marker when that fits. A board's leading repeat of any rendered kit's state is dropped, as legacy
// compact did.
func runBoards(ctx context.Context, a *allocator, r kit.Renderer, kits []kitPieces) {
	var prior []kit.Block
	for _, k := range kits {
		prior = append(prior, k.blocks...)
	}
	boardCtx, cancel := context.WithTimeout(ctx, kit.BoardDeadline)
	defer cancel()
	for i := range kits {
		k := &kits[i]
		if k.dir == "" {
			continue
		}
		path, err := proc.ResolveExecutable(filepath.Join(k.dir, "bin"), "board")
		if errors.Is(err, proc.ErrNotFound) {
			continue
		}
		if err == nil {
			probe := tagged("orchestration-board", path, "", kit.KindBoard)
			least := cost(probe.head + "x" + cutMarker(boardCutBound) + probe.tail)
			if a.room < least {
				k.board = []*piece{{body: probe.minimal(), kind: kit.KindBoard}}
				a.place(k.board[0], 0)
				continue
			}
		}
		k.board = pieces(r.BoardBlocks(boardCtx, k.dir, prior))
		for _, pc := range k.board {
			a.place(pc, 0)
		}
	}
}

// workerHeader opens the worker part, so the restored text is read as the source of truth.
const workerHeader = "A compaction summarized this conversation; the verbatim worker rules and, " +
	"when available, your original brief follow.\n\n"

// worker returns the rules and brief pieces when the session is a worker, else nils. The rules piece
// carries the worker header. A transcript that cannot be read costs only the brief.
func worker(p Params) (*piece, *piece, error) {
	in, cfg := p.Input, p.Config.Worker
	var prompt string
	var perr error
	if in.TranscriptPath != "" {
		prompt, perr = FirstPrompt(in.TranscriptPath)
	}
	isWorker := (perr == nil && hasLinePrefix(prompt, cfg.Marker)) ||
		(in.CWD != "" && paths.IsAgentWorktree(in.CWD)) ||
		(in.AgentType != "" && slices.Contains(cfg.AgentTypes, in.AgentType))
	if !isWorker {
		return nil, nil, nil
	}

	path := paths.FromConfig(p.Home, cfg.Body)
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

// restoreKits renders the state blocks of the kits recorded for the session id whose dirs exist.
// A `.kits` lock not acquired within lockTimeout, or a `.kits` that cannot be read, yields one
// dir-less kit holding only a marker.
func restoreKits(ctx context.Context, p Params) ([]kitPieces, error) {
	slugs, err := (kit.Store{Home: p.Home, Timeout: lockTimeout}).List(ctx, p.Input.SessionID)
	switch {
	case errors.Is(err, kit.ErrLocked):
		return []kitPieces{{state: []*piece{{body: "[kits omitted: .kits locked]"}}}}, nil
	case err != nil:
		return []kitPieces{{state: []*piece{{body: "[kits omitted: .kits unreadable]"}}}}, err
	}
	root, err := filepath.Abs(paths.FromConfig(p.Home, p.Config.Kit.Root))
	if err != nil {
		return nil, fmt.Errorf("kit root: %w", err)
	}
	var kits []kitPieces
	for _, slug := range slugs {
		dir := paths.KitDir(root, slug)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		blocks := kit.StateBlocks(dir)
		kits = append(kits, kitPieces{dir: dir, blocks: blocks, state: pieces(blocks)})
	}
	return kits, nil
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

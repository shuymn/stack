package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc"
)

const (
	// BoardDeadline is the one deadline covering every board of a render
	// (`orchestration-context.py:47`).
	BoardDeadline = 40 * time.Second
)

// Block kinds, so compact restore can budget each kind separately.
const (
	KindState    = "state"
	KindItems    = "items"
	KindHandover = "handover"
	KindBoard    = "board"
)

// StateFiles are a kit's state files, in render order. Callers must not modify it.
var StateFiles = []string{"NORTH.md", "state.md"}

// ItemsFile is a kit's attention item register, relative to the kit dir.
var ItemsFile = filepath.Join("attention", "items.json")

// Block is one part of a kit context: a tagged block naming its absolute path, or, with an empty
// Tag, a bare marker line in Body.
type Block struct {
	Kind string
	Tag  string
	Path string
	Body string
}

func (b Block) String() string {
	if b.Tag == "" {
		return b.Body
	}
	return fmt.Sprintf("<%s path=%q>\n%s\n</%s>", b.Tag, b.Path, b.Body, b.Tag)
}

// Join renders blocks separated by blank lines, ending with a newline; no blocks yield "".
func Join(blocks []Block) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = b.String()
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func marker(kind, s string) Block {
	return Block{Kind: kind, Body: s}
}

func unreadable(kind, path string) Block {
	return marker(kind, "[unreadable: "+path+"]")
}

// Renderer builds the context of a kit dir (§4.1.1).
type Renderer struct {
	Runner   proc.Runner
	Home     string
	Getenv   func(string) string
	Handover []config.HandoverEntry
}

// Render returns one kit's blocks in order: state files, items, handover, board; and the items
// warning of StateBlocksWarning. ctx bounds the board; give it BoardDeadline.
func (r Renderer) Render(ctx context.Context, dir string) ([]Block, string) {
	blocks, warning := StateBlocksWarning(dir)
	blocks = append(blocks, r.HandoverBlocks()...)
	return append(blocks, r.BoardBlocks(ctx, dir, blocks)...), warning
}

// StateBlocks returns the `NORTH.md`, `state.md` and attention item blocks of the kit at dir, which
// must be absolute. Files are opened through an os.Root on dir, so a symlink leaving the kit is not
// followed and renders as unreadable.
func StateBlocks(dir string) []Block {
	blocks, _ := StateBlocksWarning(dir)
	return blocks
}

// StateBlocksWarning is StateBlocks plus one line, `<path>: <reason>`, saying why it ignored items
// of a readable `attention/items.json` (`docs/design/kit-contracts.md` §4), or "" when it ignored
// none.
func StateBlocksWarning(dir string) ([]Block, string) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return []Block{unreadable(KindState, dir)}, ""
	}
	defer func() { _ = root.Close() }()

	var blocks []Block
	for _, name := range StateFiles {
		path := filepath.Join(dir, name)
		data, err := ReadFile(root, name)
		switch {
		case errors.Is(err, ErrMissing):
			if name == "state.md" {
				blocks = append(blocks, marker(KindState,
					fmt.Sprintf("[new kit: %s; write state.md here]", dir)))
			}
			continue
		case err != nil:
			blocks = append(blocks, unreadable(KindState, path))
			continue
		}
		blocks = append(blocks, Block{
			Kind: KindState,
			Tag:  "orchestration-state",
			Path: path,
			Body: strings.ReplaceAll(text(data), "\r\n", "\n"),
		})
	}
	b, ok, warning := itemsBlock(root, dir)
	if ok {
		blocks = append(blocks, b)
	}
	return blocks, warning
}

// ErrMissing reports a kit file that does not exist, as opposed to one that cannot be read.
var ErrMissing = errors.New("missing")

// ReadFile reads rel under root. Only a name that does not exist at all yields ErrMissing; a
// dangling symlink exists and fails as unreadable.
func ReadFile(root *os.Root, rel string) ([]byte, error) {
	data, err := root.ReadFile(rel)
	if missing(root, rel, err) {
		return nil, ErrMissing
	}
	return data, err //nolint:wrapcheck // callers only render a marker from it.
}

// Stat is ReadFile for the file's info, following a symlink that stays under root.
func Stat(root *os.Root, rel string) (fs.FileInfo, error) {
	info, err := root.Stat(rel)
	if missing(root, rel, err) {
		return nil, ErrMissing
	}
	return info, err //nolint:wrapcheck // callers only display a marker from it.
}

// missing reports whether err, from accessing rel under root, means rel does not exist at all.
func missing(root *os.Root, rel string, err error) bool {
	if !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	_, lerr := root.Lstat(rel)
	return errors.Is(lerr, fs.ErrNotExist)
}

// itemsBlock lists the rules of `attention/items.json` for roles orchestrator and all. A missing or
// malformed file, or one without such rules, yields no block; an unreadable one yields a marker.
// The warning is `<path>: <reason>` when items of a readable file were ignored, else "".
func itemsBlock(root *os.Root, dir string) (Block, bool, string) {
	path := filepath.Join(dir, ItemsFile)
	data, err := ReadFile(root, ItemsFile)
	if errors.Is(err, ErrMissing) {
		return Block{}, false, ""
	}
	if err != nil {
		return unreadable(KindItems, path), true, ""
	}
	rules, reason := parseItems(data)
	warning := ""
	if reason != "" {
		warning = path + ": " + reason
	}
	if len(rules) == 0 {
		return Block{}, false, warning
	}
	return Block{
		Kind: KindItems,
		Tag:  "orchestrator-attention-items",
		Path: path,
		Body: strings.Join(rules, "\n"),
	}, true, warning
}

// parseItems returns the rendered rules of an items file (`- ` and the rule) and why it ignored
// items: `attention items ignored: <reason>` for a file that is not an object whose `items` is an
// array, `<n> of <m> attention items ignored (first: item <i>: <reason>)` when elements are not
// objects with a string `role` and `rule`, else "". Keys match in any letter case, as in Go's JSON
// decoding.
func parseItems(data []byte) ([]string, string) {
	if err := json.Unmarshal(data, new(any)); err != nil {
		return nil, "attention items ignored: invalid JSON: " + err.Error()
	}
	if k := kindOf(data); k != kindObject {
		return nil, "attention items ignored: top level is " + k + ", not an object"
	}
	var doc struct {
		Items json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(data, &doc) // valid JSON object: always decodes into a raw field
	if doc.Items == nil {
		return nil, `attention items ignored: "items" is missing`
	}
	var items []json.RawMessage
	if k := kindOf(doc.Items); k != kindArray || json.Unmarshal(doc.Items, &items) != nil {
		return nil, `attention items ignored: "items" is ` + k + ", not an array"
	}
	var rules []string
	ignored, first := 0, ""
	for i, raw := range items {
		role, rule, reason := parseItem(raw)
		if reason != "" {
			if ignored == 0 {
				first = fmt.Sprintf("item %d: %s", i, reason)
			}
			ignored++
			continue
		}
		if role == "orchestrator" || role == "all" {
			rules = append(rules, "- "+rule)
		}
	}
	if ignored == 0 {
		return rules, ""
	}
	return rules, fmt.Sprintf("%d of %d attention items ignored (first: %s)",
		ignored, len(items), first)
}

// parseItem returns an element's role and rule, or why it is ignored.
func parseItem(raw json.RawMessage) (string, string, string) {
	if k := kindOf(raw); k != kindObject {
		return "", "", k + ", not an object"
	}
	var role, rule string
	var item struct {
		Role json.RawMessage `json:"role"`
		Rule json.RawMessage `json:"rule"`
	}
	_ = json.Unmarshal(raw, &item) // valid JSON object: always decodes into raw fields
	fields := []struct {
		name string
		raw  json.RawMessage
		dst  *string
	}{{"role", item.Role, &role}, {"rule", item.Rule, &rule}}
	for _, f := range fields {
		if f.raw == nil {
			return "", "", `"` + f.name + `" is missing`
		}
		if k := kindOf(f.raw); k != kindString || json.Unmarshal(f.raw, f.dst) != nil {
			return "", "", `"` + f.name + `" is ` + k + ", not a string"
		}
	}
	return role, rule, ""
}

const (
	kindObject = "an object"
	kindArray  = "an array"
	kindString = "a string"
)

// kindOf names the kind of a valid JSON value, with its article.
func kindOf(raw []byte) string {
	v := bytes.TrimLeft(raw, " \t\r\n")
	if len(v) == 0 {
		return "empty"
	}
	switch v[0] {
	case '{':
		return kindObject
	case '[':
		return kindArray
	case '"':
		return kindString
	case 'n':
		return "null"
	case 't', 'f':
		return "a boolean"
	default:
		return "a number"
	}
}

// HandoverBlocks returns the handover file of the first active `[handover]` entry, if any. An entry
// is active when its env var is non-empty, except HERDR_ENV, which must be exactly "1".
func (r Renderer) HandoverBlocks() []Block {
	for _, h := range r.Handover {
		v := ""
		if r.Getenv != nil {
			v = r.Getenv(h.Env)
		}
		if v == "" || (h.Env == "HERDR_ENV" && v != "1") {
			continue
		}
		path := paths.Expand(r.Home, h.Path)
		if !filepath.IsAbs(path) { // relative to the config file's dir, like the file itself
			path = filepath.Join(filepath.Dir(paths.ConfigFile(r.Home)), path)
		}
		// A user-configured path, read directly: a symlinked handover file is legitimate.
		data, err := os.ReadFile(path)
		if err != nil {
			return []Block{unreadable(KindHandover, path)}
		}
		return []Block{
			{Kind: KindHandover, Tag: "orchestration-handover", Path: path, Body: text(data)},
		}
	}
	return nil
}

// BoardBlocks runs `<dir>/bin/board` with cwd dir under ctx and returns its output, minus a leading
// repeat of the state files in prior (whole lines, after CRLF normalisation), or a marker when it
// failed. Output past proc's 1 MiB cap is cut and marked. No board yields nothing. A ctx
// already done launches nothing and yields `[board skipped: deadline]`.
func (r Renderer) BoardBlocks(ctx context.Context, dir string, prior []Block) []Block {
	path, err := proc.ResolveExecutable(filepath.Join(dir, "bin"), "board")
	switch {
	case errors.Is(err, proc.ErrNotFound):
		return nil
	case errors.Is(err, proc.ErrNotRunnable):
		return []Block{marker(KindBoard, "[board not runnable on this OS: "+path+"]")}
	case err != nil:
		return []Block{unreadable(KindBoard, filepath.Join(dir, "bin", "board"))}
	}
	if ctx.Err() != nil {
		return []Block{marker(KindBoard, "[board skipped: deadline]")}
	}
	board := func(body string) []Block {
		return []Block{{Kind: KindBoard, Tag: "orchestration-board", Path: path, Body: body}}
	}
	res, err := r.Runner.Run(ctx, proc.Cmd{Name: path, Dir: dir})
	switch {
	case err != nil:
		return board(fmt.Sprintf("[board failed: %v]", err))
	case res.TimedOut:
		return board("[board timed out]")
	}

	out := strings.ReplaceAll(text(res.Stdout), "\r\n", "\n")
	for _, b := range prior {
		// A board's leading repeat of a state file is dropped.
		if b.Kind == KindState && b.Tag != "" && b.Body != "" &&
			(out == b.Body || strings.HasPrefix(out, b.Body+"\n")) {
			out = strings.TrimLeft(out[len(b.Body):], " \t\n")
		}
	}
	notes := []string{out}
	if res.Truncated {
		notes = append(notes, "[board output truncated]")
	}
	if res.WaitDelayExceeded {
		notes = append(notes, "[board output incomplete: its pipes outlived it]")
	}
	if res.ExitCode != 0 || out == "" {
		lines := strings.Split(text(res.Stderr), "\n")
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" {
			last = "no output"
		}
		notes = append(notes, fmt.Sprintf("[board exit %d: %s]", res.ExitCode, last))
	}
	return board(strings.TrimSpace(strings.Join(notes, "\n")))
}

// text decodes file or process output for display: invalid UTF-8 becomes U+FFFD, and surrounding
// space is trimmed.
func text(data []byte) string {
	return strings.TrimSpace(strings.ToValidUTF8(string(data), string(utf8.RuneError)))
}

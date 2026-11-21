// Package auditdue is the stack-audit-due extension of the merged stage
// (docs/design/core-stages.md §4.7). For each open kit it counts merges in <kit dir>/audit-due.json
// and prints a due line when K merges passed since the last one, or when a merge follows the
// previous counted merge without a kit state update. It is deterministic and makes no model or
// network call; counting is approximate by design (no lock).
package auditdue

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Statuses of a response.
const (
	StatusPass = "pass"
	StatusWarn = "warn"
)

// Finding kinds.
const (
	kindConfig  = "config"
	kindKit     = "kit-state"
	kindRequest = "request"
)

const (
	// DefaultK is the number of merges after which a kit is due (§5.2).
	DefaultK = 5
	// StateFile is the per-kit state file name.
	StateFile = "audit-due.json"

	configRel = ".config/stack/audit-due.toml"
	absent    = "absent"
)

// stateFiles are the kit state files hashed, in order (§4.7).
var stateFiles = []string{"NORTH.md", "state.md"}

// Kit is one entry of the request's kits.
type Kit struct {
	Slug string `json:"slug"`
	Dir  string `json:"dir"`
}

// Merge is the request's merge field.
type Merge struct {
	PR   int    `json:"pr"`
	Head string `json:"head"`
	OID  string `json:"oid"`
}

// Request holds the fields of a merged request that audit-due reads; others are ignored.
type Request struct {
	Kits  []Kit  `json:"kits"`
	Merge *Merge `json:"merge"`
}

// Finding is one owner-actionable line of the response.
type Finding struct {
	Kind   string `json:"kind"`
	Where  string `json:"where"`
	Detail string `json:"detail"`
}

// Response is the extension protocol answer.
type Response struct {
	Status   string    `json:"status"`
	Context  string    `json:"context"`
	Findings []Finding `json:"findings"`
}

// state is the content of audit-due.json.
type state struct {
	Merges int    `json:"merges"`
	State  string `json:"state"`
	OID    string `json:"oid"`
}

// Respond reads a request to EOF, decodes it and runs it; home is the home directory ("" when
// unknown).
func Respond(r io.Reader, home string) Response {
	var req Request
	data, err := io.ReadAll(r)
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	if err != nil {
		return warn(Finding{
			Kind: kindRequest, Where: "stdin",
			Detail: "cannot read the request: " + err.Error(),
		})
	}
	return Run(req, home)
}

// Run processes every kit of a request with a merge field, in kits order.
func Run(req Request, home string) Response {
	resp := Response{Status: StatusPass, Findings: []Finding{}}
	if req.Merge == nil || len(req.Kits) == 0 {
		return resp
	}
	k, f := limit(home)
	if f != nil {
		resp.Findings = append(resp.Findings, *f)
	}
	var lines []string
	for _, kit := range req.Kits {
		line, findings := count(kit, req.Merge.OID, k)
		resp.Findings = append(resp.Findings, findings...)
		if line != "" {
			lines = append(lines, line)
		}
	}
	resp.Context = strings.Join(lines, "\n")
	if len(resp.Findings) > 0 {
		resp.Status = StatusWarn
	}
	return resp
}

func warn(f Finding) Response {
	return Response{Status: StatusWarn, Findings: []Finding{f}}
}

// limit reads K from ~/.config/stack/audit-due.toml: `merges = <int >= 1>`. An absent file or key
// is the default; anything invalid is the default with a finding.
func limit(home string) (int, *Finding) {
	bad := func(detail string) (int, *Finding) {
		return DefaultK, &Finding{
			Kind: kindConfig, Where: "~/" + configRel,
			Detail: fmt.Sprintf("%s; using merges = %d", detail, DefaultK),
		}
	}
	if home == "" {
		return bad("home directory unknown")
	}
	//nolint:gosec // G703: the owner's own config file under HOME, by design.
	data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(configRel)))
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultK, nil
	}
	if err != nil {
		return bad("cannot read the file: " + err.Error())
	}
	var cfg map[string]any
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return bad("not valid TOML")
	}
	v, ok := cfg["merges"]
	if !ok {
		return DefaultK, nil
	}
	n, ok := v.(int64)
	if !ok || n < 1 {
		return bad(fmt.Sprintf("merges = %v is not an integer >= 1", v))
	}
	if n > int64(^uint32(0)>>1) {
		return bad(fmt.Sprintf("merges = %d is too large", n))
	}
	return int(n), nil
}

// count applies §4.7 to one kit and returns its due line ("" when not due) and its findings.
func count(kit Kit, oid string, k int) (string, []Finding) {
	finding := func(format string, a ...any) Finding {
		return Finding{
			Kind: kindKit, Where: kit.Slug,
			Detail: fmt.Sprintf("kit %s: ", kit.Slug) + fmt.Sprintf(format, a...),
		}
	}
	fail := func(format string, a ...any) (string, []Finding) {
		return "", []Finding{finding(format, a...)}
	}
	if kit.Dir == "" {
		return fail("no kit dir in the request")
	}
	h, err := hash(kit.Dir)
	if err != nil {
		return fail("cannot read the kit state: %v", err)
	}
	var findings []Finding
	path := filepath.Join(kit.Dir, StateFile)
	prev, existed, err := load(path)
	switch {
	case errors.Is(err, errCorrupt):
		findings = append(findings, finding("%s is not valid (%v); replaced", StateFile, err))
	case err != nil:
		return fail("cannot read %s: %v", StateFile, err)
	}
	if existed && prev.OID == oid {
		return "", findings
	}
	n := prev.Merges + 1
	reason := ""
	switch {
	case n >= k:
		reason = fmt.Sprintf("%d merges since the last due signal", n)
	case existed && h == prev.State:
		reason = "merged without a state update since the previous merge"
	}
	next := state{Merges: n, State: h, OID: oid}
	if reason != "" {
		next.Merges = 0
	}
	if err := save(kit.Dir, next); err != nil {
		return "", append(findings, finding("cannot write %s: %v", StateFile, err))
	}
	if reason == "" {
		return "", findings
	}
	return fmt.Sprintf("audit due (%s): run steward on kit %s", reason, kit.Slug), findings
}

// hash is the kit state hash: SHA-256 over, for NORTH.md then state.md, the file name, NUL, its
// bytes or "absent", NUL.
func hash(dir string) (string, error) {
	sum := sha256.New()
	for _, name := range stateFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			data = []byte(absent)
		case err != nil:
			return "", fmt.Errorf("read %s: %w", name, err)
		}
		sum.Write([]byte(name))
		sum.Write([]byte{0})
		sum.Write(data)
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

var errCorrupt = errors.New("corrupt state file")

// load reads audit-due.json. A missing file is (zero, false, nil); a file that is not a JSON
// object with an integer merges >= 0 and string state and oid is (zero, false, errCorrupt).
func load(path string) (state, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return state{}, false, nil
	}
	if err != nil {
		return state{}, false, fmt.Errorf("read: %w", err)
	}
	var raw struct {
		Merges *int    `json:"merges"`
		State  *string `json:"state"`
		OID    *string `json:"oid"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return state{}, false, fmt.Errorf("%w: %w", errCorrupt, err)
	}
	if dec.More() {
		return state{}, false, fmt.Errorf("%w: trailing data", errCorrupt)
	}
	if raw.Merges == nil || raw.State == nil || raw.OID == nil || *raw.Merges < 0 {
		return state{}, false, fmt.Errorf("%w: needs merges >= 0, state and oid", errCorrupt)
	}
	return state{Merges: *raw.Merges, State: *raw.State, OID: *raw.OID}, true, nil
}

// save writes the state to a temp file in the kit dir and renames it over audit-due.json, so the
// file is always whole.
func save(dir string, s state) (err error) {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	f, err := os.CreateTemp(dir, ".audit-due-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmp, filepath.Join(dir, StateFile)); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

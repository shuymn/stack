package improve

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed writing.md
var writing string

const (
	model        = "claude-opus-5-5"
	effort       = "xhigh"
	maxBudgetUSD = "50"
	fileTools    = "Read,Edit,Write,Grep,Glob"
	// modelSettings turns auto memory off and blocks file-tool reads and writes outside the
	// working directory (design §3.4 step 9).
	modelSettings = `{"autoMemoryEnabled":false,"permissions":` +
		`{"blockReadsOutsideWorkingDirectories":true,"blockWritesOutsideWorkingDirectories":true}}`
	// outputSchema is the legacy result schema (improve.py:88-92).
	outputSchema = `{"type":"object","additionalProperties":false,` +
		`"required":["change","evidence_ids","summary","watch","commit_message","pr_title","pr_body"],` +
		`"properties":{"change":{"type":"boolean"},` +
		`"evidence_ids":{"type":"array","items":{"type":"string"}},` +
		`"summary":{"type":"string"},"watch":{"type":"string"},` +
		`"commit_message":{"type":"string"},"pr_title":{"type":"string"},"pr_body":{"type":"string"}}}`
	maxPatch = 256 << 10
	// scanChunk is the read size of the untracked-file credential scan.
	scanChunk = 64 << 10
)

// task is the legacy task (improve.py:73-87) with the allowed paths from the target's config.
const task = `## Task

The working directory is a copy of the public repository %s at commit %s. Its editable agent
instructions are the existing files matching these patterns (** is any number of directories):
%s
Using only the evidence above, make at most one change to those instructions. Prefer, in this order: a mechanism,
delivery at an event, deleting text, adding text. Edit only existing files matching the patterns; create, delete or
rename nothing. If the evidence does not justify a change, return change: false.

Return evidence_ids (the ids above the change rests on), summary, watch (the signal to observe after the merge),
commit_message (Conventional Commits, English), pr_title and pr_body. Everything you return may be published: write
nothing that the evidence and the repository do not already show.
`

// schemaKeys are the keys of the structured output.
var schemaKeys = []string{
	"change", "evidence_ids", "summary", "watch", "commit_message", "pr_title", "pr_body",
}

var counters = []string{
	"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
}

var reCount = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// modelOutput is the structured output of the model.
type modelOutput struct {
	Change        bool
	EvidenceIDs   []string
	Summary       string
	Watch         string
	CommitMessage string
	PRTitle       string
	PRBody        string
}

func (m *modelOutput) strings() []string {
	return append([]string{m.Summary, m.Watch, m.CommitMessage, m.PRTitle, m.PRBody},
		m.EvidenceIDs...)
}

// result is what improve reads from the claude JSON result.
type result struct {
	cost float64
	// known: total_cost_usd is a finite non-negative number and usage holds the four counters as
	// non-negative integers. zero: known, the cost is 0 and every counter is 0.
	known, zero bool
	// output is the structured output of a success result that matches the schema, else nil.
	output *modelOutput
}

// parseResult reads stdout to EOF: exactly one complete `type: result` object, else nothing is
// known (trailing bytes mean the output is not the one result improve can trust).
func parseResult(data []byte) result {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil || m["type"] != "result" {
		return result{}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return result{}
	}
	var r result
	costOK := false
	if n, ok := m["total_cost_usd"].(json.Number); ok {
		if f, err := n.Float64(); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) && f >= 0 {
			r.cost, costOK = f, true
		}
	}
	usageOK, allZero := false, true
	if u, ok := m["usage"].(map[string]any); ok {
		usageOK = true
		for _, k := range counters {
			n, ok := u[k].(json.Number)
			if !ok || !reCount.MatchString(n.String()) {
				usageOK = false
				break
			}
			if _, err := strconv.ParseUint(n.String(), 10, 64); err != nil {
				usageOK = false
				break
			}
			if n.String() != "0" {
				allZero = false
			}
		}
	}
	r.known = costOK && usageOK
	r.zero = r.known && r.cost == 0 && allZero
	if m["subtype"] == "success" && m["is_error"] == false {
		r.output = decodeOutput(m["structured_output"])
	}
	return r
}

// decodeOutput checks the structured output against the schema: exactly the seven keys, each of
// its type.
func decodeOutput(v any) *modelOutput {
	m, ok := v.(map[string]any)
	if !ok || len(m) != len(schemaKeys) {
		return nil
	}
	var out modelOutput
	str := func(k string, into *string) bool {
		s, ok := m[k].(string)
		*into = s
		return ok
	}
	change, ok := m["change"].(bool)
	if !ok {
		return nil
	}
	out.Change = change
	ids, ok := m["evidence_ids"].([]any)
	if !ok {
		return nil
	}
	out.EvidenceIDs = []string{}
	for _, id := range ids {
		s, ok := id.(string)
		if !ok {
			return nil
		}
		out.EvidenceIDs = append(out.EvidenceIDs, s)
	}
	if !str("summary", &out.Summary) || !str("watch", &out.Watch) ||
		!str("commit_message", &out.CommitMessage) || !str("pr_title", &out.PRTitle) ||
		!str("pr_body", &out.PRBody) {
		return nil
	}
	return &out
}

// claudeArgs is the argv of the model call (design §3.4 step 9).
func claudeArgs() []string {
	return []string{
		"-p", "--setting-sources", "user", "--strict-mcp-config", "--tools", fileTools,
		"--permission-mode", "acceptEdits", "--permission-prompts", "none",
		"--no-session-persistence", "--settings", modelSettings, "--output-format", "json",
		"--json-schema", outputSchema, "--model", model, "--effort", effort,
		"--max-budget-usd", maxBudgetUSD,
	}
}

// claudeEnvSet is what claudeEnv sets; it also drops CLAUDE_CONFIG_DIR.
const claudeEnvSet = "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"

// claudeEnv is env without CLAUDE_CONFIG_DIR, with auto memory off.
func claudeEnv(env []string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == "CLAUDE_CONFIG_DIR" || name == "CLAUDE_CODE_DISABLE_AUTO_MEMORY"
	})
	return append(out, claudeEnvSet)
}

func (a *attempt) prompt() string {
	var pats strings.Builder
	for _, p := range a.s.paths {
		fmt.Fprintf(&pats, "- %s\n", p)
	}
	return a.briefTxt + "\n## Writing rules\n\n" + writing + "\n" +
		fmt.Sprintf(task, a.s.slug, a.s.baseOID, pats.String())
}

// model is steps 9-11: the model call, billing, the credential guard and the result checks.
func (a *attempt) model() *stop {
	s := a.s
	timeout := s.modelTime()
	s.logf("claude argv: %q", claudeArgs())
	s.logf("claude env: %s set, CLAUDE_CONFIG_DIR removed; timeout %s", claudeEnvSet,
		timeout.Round(time.Second))
	start := time.Now()
	out := s.child("claude", command{
		args: claudeArgs(), dir: a.worktree, env: claudeEnv(s.o.Env), stdin: []byte(a.prompt()),
		timeout: timeout,
	})
	s.logf("claude wall time %s (%s)", time.Since(start).Round(time.Second), out.describe())
	if errors.Is(out.err, errStart) {
		return halt("failed:model-start", "claude did not start")
	}
	a.modelStarted = true
	a.hook("model")
	res := parseResult(out.stdout)
	// Billing trusts only output read to EOF from a child that exited on its own: not timed out,
	// not killed by a signal (exit -1), not cut, with no wait or pipe failure.
	complete := out.err == nil && !out.timedOut && !out.truncated && !out.pipesCut && out.exit >= 0
	if complete && res.known {
		a.st.CostUSD, a.costKnown = res.cost, true
		a.zeroCost = res.zero
	} else {
		a.st.CostUSD = unknownCost
	}
	entries, err := a.status()
	if err != nil {
		return halt("failed:check", "cannot read the worktree status: %v", err)
	}
	diff, err := a.worktreeDiff(entries)
	if err != nil {
		return halt("failed:check", "cannot read the worktree diff: %v", err)
	}
	a.st.Patch = cutPatch(s.redact(diff))
	if out.timedOut {
		return halt("failed:timeout", "claude did not finish within its timeout")
	}
	if s.leaks(diff) || (res.output != nil && slices.ContainsFunc(res.output.strings(), s.leaks)) {
		return halt("proposal:credential", "the model output contains an endpoint credential")
	}
	if stop := a.scanUntracked(entries); stop != nil {
		return stop
	}
	if !out.ok() || res.output == nil {
		return halt("failed:model-result", "claude %s; result not a success matching the schema",
			out.describe())
	}
	o := res.output
	a.st.Summary, a.st.Watch, a.st.EvidenceIDs = o.Summary, o.Watch, o.EvidenceIDs
	if !o.Change {
		a.st.Outcome = outNoChange
		return nil
	}
	if len(o.EvidenceIDs) == 0 {
		return halt("proposal:evidence-ids", "no evidence id cited")
	}
	for _, id := range o.EvidenceIDs {
		if _, ok := a.briefIDs[id]; !ok {
			return halt("proposal:evidence-ids", "cited id %q is not in the brief", id)
		}
	}
	if err := a.treeCheck(entries); err != nil {
		return err
	}
	a.title, a.message = strings.TrimSpace(o.PRTitle), strings.TrimSpace(o.CommitMessage)
	body := strings.TrimSpace(o.PRBody)
	if a.title == "" || strings.ContainsAny(a.title, "\r\n") || a.message == "" || body == "" {
		return halt("proposal:metadata", "blank or multi-line title, or blank message or body")
	}
	var b strings.Builder
	b.WriteString(body + "\n\nEvidence:\n")
	seen := map[string]bool{}
	for _, id := range o.EvidenceIDs {
		if !seen[id] {
			seen[id] = true
			b.WriteString("- " + a.briefIDs[id].Where + "\n")
		}
	}
	a.body = b.String()
	return nil
}

// statusEntry is one entry of `git status --porcelain=v2 -z`.
type statusEntry struct {
	kind  byte
	xy    string
	sub   string
	modes [3]string
	path  string
}

// status reads the worktree status, untracked files included.
func (a *attempt) status() ([]statusEntry, error) {
	out := a.s.git(a.worktree, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if !out.ok() {
		return nil, fmt.Errorf("%w: git status %s", errGit, out.describe())
	}
	return parseStatus(out.stdout)
}

var (
	errGit    = errors.New("git failed")
	errStatus = errors.New("unexpected git status entry")
)

func parseStatus(data []byte) ([]statusEntry, error) {
	toks := strings.Split(string(data), "\x00")
	var out []statusEntry
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t == "" {
			continue
		}
		var f []string
		switch t[0] {
		case '1':
			f = strings.SplitN(t, " ", 9) //nolint:mnd // porcelain v2 ordinary entry fields
		case '2':
			f = strings.SplitN(t, " ", 10) //nolint:mnd // porcelain v2 rename entry fields
			i++                            // the original path
		case 'u':
			f = strings.SplitN(t, " ", 11) //nolint:mnd // porcelain v2 unmerged entry fields
		case '?', '!':
			if len(t) < 3 { //nolint:mnd // "? " and a path
				return nil, errStatus
			}
			out = append(out, statusEntry{kind: t[0], path: t[2:]})
			continue
		default:
			return nil, errStatus
		}
		if len(f) < 9 { //nolint:mnd // the shortest entry
			return nil, errStatus
		}
		e := statusEntry{kind: t[0], xy: f[1], sub: f[2], path: f[len(f)-1]}
		copy(e.modes[:], f[3:6])
		out = append(out, e)
	}
	return out, nil
}

// worktreeDiff returns the patch: tracked changes against base, plus the names of untracked files.
func (a *attempt) worktreeDiff(entries []statusEntry) (string, error) {
	out := a.s.git(a.worktree, "diff", "--text", "--no-ext-diff", "--no-textconv",
		"--no-color", a.s.baseOID)
	if !out.ok() {
		return "", fmt.Errorf("%w: git diff %s", errGit, out.describe())
	}
	var patch strings.Builder
	patch.Write(out.stdout)
	for _, e := range entries {
		if e.kind == '?' || e.kind == '!' {
			fmt.Fprintf(&patch, "# untracked: %s\n", e.path)
		}
	}
	return patch.String(), nil
}

// scanUntracked runs the credential guard over every untracked regular file, whole, one at a
// time. A file it cannot read leaves the check incomplete, which refuses like a hit.
func (a *attempt) scanUntracked(entries []statusEntry) *stop {
	for _, e := range entries {
		if e.kind != '?' && e.kind != '!' {
			continue
		}
		p := filepath.Join(a.worktree, filepath.FromSlash(e.path))
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		hit, err := a.s.fileLeaks(p)
		if err != nil {
			a.s.logf("credential scan: %v", err)
			return halt("proposal:credential", "cannot scan the untracked file %s for an "+
				"endpoint credential", e.path)
		}
		if hit {
			return halt("proposal:credential", "the model output contains an endpoint credential")
		}
	}
	return nil
}

// fileLeaks streams the file in chunks, each read with the previous chunk's tail, so a
// credential across a chunk boundary is found without holding the file.
func (s *state) fileLeaks(p string) (bool, error) {
	longest := 0
	for _, c := range s.creds {
		longest = max(longest, len(c))
	}
	if longest == 0 {
		return false, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return false, fmt.Errorf("read untracked file: %w", err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 0, longest-1+scanChunk)
	for {
		n, err := f.Read(buf[len(buf) : len(buf)+scanChunk])
		buf = buf[:len(buf)+n]
		if s.leaks(string(buf)) {
			return true, nil
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read untracked file: %w", err)
		}
		if keep := longest - 1; len(buf) > keep {
			buf = buf[:copy(buf, buf[len(buf)-keep:])]
		}
	}
}

// cutPatch keeps at most maxPatch bytes, marking a cut.
func cutPatch(p string) string {
	if len(p) <= maxPatch {
		return p
	}
	return p[:maxPatch] + "\n[patch cut at 256 KiB]\n"
}

// treeCheck is the step 11 tree rule: only modifications of existing 100644 files within paths.
func (a *attempt) treeCheck(entries []statusEntry) *stop {
	if len(entries) == 0 {
		return halt("proposal:tree", "change: true without a diff")
	}
	for _, e := range entries {
		switch {
		case e.kind != '1':
			return halt("proposal:tree", "not a modification of an existing file: %s", e.path)
		case strings.Trim(e.xy, "M.") != "" || e.sub != "N...":
			return halt("proposal:tree", "not a plain modification (%s): %s", e.xy, e.path)
		case e.modes != [3]string{"100644", "100644", "100644"}:
			return halt("proposal:tree", "not a 100644 file kept at 100644: %s", e.path)
		case !matchAny(a.s.paths, e.path):
			return halt("proposal:tree", "outside the allowed paths: %s", e.path)
		}
	}
	return nil
}

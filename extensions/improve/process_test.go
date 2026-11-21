package improve

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// processEnv is f's environment with dirs first on PATH and the improvetest clock set.
func (f *fx) processEnv(at time.Time, dirs ...string) []string {
	env := slices.Clone(f.opts.Env)
	for i, kv := range env {
		if path, ok := strings.CutPrefix(kv, "PATH="); ok {
			env[i] = "PATH=" + strings.Join(append(dirs, path), string(os.PathListSeparator))
		}
	}
	return append(env, "STACK_IMPROVE_NOW="+at.Format(time.RFC3339))
}

// toolDir installs the default fakes, with claude answering claudeResp, into a new directory.
func (f *fx) toolDir(claudeResp proctest.Response, withStack bool) string {
	f.t.Helper()
	dir := f.t.TempDir()
	install := func(name string, spec *fakeSpec, resp ...proctest.Response) {
		p := proctest.InstallAt(f.t, dir, name, proctest.Scenario{Responses: resp})
		if spec != nil {
			if err := os.WriteFile(strings.TrimSuffix(p, ".exe")+specSuffix, mustJSON(f.t, spec),
				0o600); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	install("gh", f.ghSpec(), ghDefaults()...)
	install("claude", &fakeSpec{Edits: []fakeEdit{{Path: "skills/a.md", Content: "alpha two\n"}}},
		claudeResp)
	install("codex", &fakeSpec{WriteAfter: "-o", WriteContent: `{"high":[]}`}, proctest.Response{})
	if withStack {
		install("stack", nil, proctest.Response{})
	}
	return dir
}

// startImprove starts the improvetest stack-improve on f's repository.
func (f *fx) startImprove(ctx context.Context, env []string) (*exec.Cmd, *bytes.Buffer) {
	f.t.Helper()
	cmd := exec.CommandContext(ctx, built.improve)
	cmd.Dir = f.repo.Dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(`{"stage":"cycle","repo":` + string(mustJSON(f.t, f.repo.Dir)) +
		`,"range":"","prompt":""}`)
	var out bytes.Buffer
	cmd.Stdout = &out
	// Each process gets its own stderr writer: two processes run at once.
	cmd.Stderr = &lockedWriter{f: f}
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	return cmd, &out
}

// lockedWriter appends to f.stderr under a lock.
type lockedWriter struct{ f *fx }

var stderrMu sync.Mutex

func (w *lockedWriter) Write(p []byte) (int, error) {
	stderrMu.Lock()
	defer stderrMu.Unlock()
	return w.f.stderr.Write(p) //nolint:wrapcheck // a bytes.Buffer never fails
}

func decodeResponse(t *testing.T, data []byte) Response {
	t.Helper()
	var r Response
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("response %q: %v", data, err)
	}
	checkResponse(t, r)
	return r
}

// I5: two real processes; the second run in the next week finds the lock held, leaves the
// running worktree alone, and a run after the first ends claims its own week.
func TestRunLock(t *testing.T) {
	t.Parallel()
	needBuilt(t)
	f := newFx(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	unbilled := `{"type":"result","subtype":"error_during_execution","is_error":true,` +
		`"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
	dirA := f.toolDir(proctest.Response{Stdout: unbilled, Sleep: 8 * time.Second}, true)
	noChange := claudeOut(map[string]any{
		"change": false, "evidence_ids": []string{}, "summary": "s", "watch": "w",
		"commit_message": "", "pr_title": "", "pr_body": "",
	}, 1.0, zeroUsage(1))
	dirB := f.toolDir(proctest.Response{Stdout: noChange}, true)
	next := now.Add(7 * 24 * time.Hour)
	stateDir := filepath.Join(f.home, ".local", "state", "stack", "improve")

	cmdA, outA := f.startImprove(ctx, f.processEnv(now, dirA))
	doneA := make(chan error, 1)
	go func() { doneA <- cmdA.Wait() }()
	claudeA := filepath.Join(dirA, "claude")
	for len(proctest.Calls(t, claudeA)) == 0 {
		select {
		case err := <-doneA:
			t.Fatalf("A ended before claude (%v): %s\n%s", err, outA, f.stderr)
		case <-ctx.Done():
			t.Fatalf("A never reached claude:\n%s", f.stderr)
		case <-time.After(50 * time.Millisecond):
		}
	}
	// The observation B and C read is fresh in the next week.
	f.latest(next.Add(-time.Hour), publicItems())
	worktreeA := filepath.Join(stateDir, "work", week)
	if _, err := os.Stat(filepath.Join(worktreeA, "skills", "a.md")); err != nil {
		t.Fatalf("A's worktree: %v", err)
	}

	cmdB, outB := f.startImprove(ctx, f.processEnv(next, dirB))
	if err := cmdB.Wait(); err != nil {
		t.Fatalf("B: %v", err)
	}
	respB := decodeResponse(t, outB.Bytes())
	if respB.Status != StatusPass || respB.Context != "another improve run is active" {
		t.Fatalf("B = %+v", respB)
	}
	if _, err := os.Stat(filepath.Join(worktreeA, "skills", "a.md")); err != nil {
		t.Fatalf("A's worktree after B: %v", err)
	}
	if err := <-doneA; err != nil {
		t.Fatalf("A: %v", err)
	}
	respA := decodeResponse(t, outA.Bytes())
	if !strings.Contains(respA.Context, "failed:model-result") ||
		!strings.Contains(respA.Context, "stamp removed") {
		t.Fatalf("A = %+v", respA)
	}
	if _, err := os.Stat(filepath.Join(stateDir, week)); !os.IsNotExist(err) {
		t.Fatalf("A's stamp kept: %v", err)
	}

	cmdC, outC := f.startImprove(ctx, f.processEnv(next, dirB))
	if err := cmdC.Wait(); err != nil {
		t.Fatalf("C: %v", err)
	}
	respC := decodeResponse(t, outC.Bytes())
	if respC.Status != StatusPass || !strings.Contains(respC.Context, "2026-W48: no-change") {
		t.Fatalf("C = %+v\n%s", respC, f.stderr)
	}
	if n := len(proctest.Calls(t, filepath.Join(dirB, "claude"))); n != 1 {
		t.Fatalf("model calls in W48 = %d, want 1", n)
	}
	if n := len(proctest.Calls(t, claudeA)); n != 1 {
		t.Fatalf("model calls in W47 = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "2026-W48")); err != nil {
		t.Fatalf("C's stamp: %v", err)
	}
}

// TestProtocol runs stack-improve through `stack run cycle --ext improve` with fakes on PATH:
// core adopts the response (no synthetic error) both when improve is off and when it publishes.
func TestProtocol(t *testing.T) {
	t.Parallel()
	needBuilt(t)
	for _, tc := range []struct {
		name    string
		enabled bool
		context string
	}{
		{"disabled", false, "improve disabled"},
		{"publishes", true, "pr https://github.com/o/r/pull/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			if !tc.enabled {
				if err := os.Remove(
					filepath.Join(f.home, ".config", "stack", "improve.toml"),
				); err != nil {
					t.Fatal(err)
				}
			}
			tools := f.toolDir(
				proctest.Response{Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1))},
				false,
			)
			runCycle(t, f.repo.Dir, f.processEnv(now, built.dir, tools), f.stderr, tc.context)
		})
	}
}

// runCycle runs `stack run cycle --ext improve` in dir and requires one adopted improve result
// that passes with want in its context.
func runCycle(t *testing.T, dir string, env []string, stderr io.Writer, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, built.stack, "run", "cycle", "--ext", "improve")
	c.Dir = dir
	c.Env = env
	var stdout bytes.Buffer
	c.Stdout, c.Stderr = &stdout, stderr
	if err := c.Run(); err != nil {
		t.Fatalf("stack run: %v\n%s", err, stdout.String())
	}
	var agg struct {
		Stage   string `json:"stage"`
		Status  string `json:"status"`
		Results []struct {
			Name     string            `json:"name"`
			Status   string            `json:"status"`
			Error    string            `json:"error"`
			Context  string            `json:"context"`
			Findings []json.RawMessage `json:"findings"`
		} `json:"results"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &agg); err != nil {
		t.Fatalf("aggregate %q: %v", stdout.String(), err)
	}
	if agg.Stage != "cycle" || agg.Status != StatusPass || len(agg.Results) != 1 ||
		agg.Results[0].Name != "improve" || agg.Results[0].Error != "" ||
		agg.Results[0].Status != StatusPass || agg.Results[0].Findings == nil ||
		!strings.Contains(agg.Results[0].Context, want) {
		t.Fatalf("aggregate = %s", stdout.String())
	}
}

// P9: on every runner, including Windows, a disabled improve answers pass through core and
// starts none of the tools it would call.
func TestProtocolDisabled(t *testing.T) {
	t.Parallel()
	needBuilt(t)
	r := gittest.New(t)
	home, tools := t.TempDir(), t.TempDir()
	fakes := make([]string, 0, 3)
	for _, name := range []string{"gh", "claude", "codex"} {
		fakes = append(fakes, proctest.InstallAt(t, tools, name,
			proctest.Scenario{Responses: []proctest.Response{{}}}))
	}
	env := slices.DeleteFunc(slices.Clone(r.Env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(name, "HOME") || strings.EqualFold(name, "USERPROFILE")
	})
	for i, kv := range env {
		if name, path, ok := strings.Cut(kv, "="); ok && strings.EqualFold(name, "PATH") {
			env[i] = name + "=" + strings.Join([]string{built.dir, tools, path},
				string(os.PathListSeparator))
		}
	}
	env = append(env, "HOME="+home, "USERPROFILE="+home)
	var stderr bytes.Buffer
	runCycle(t, r.Dir, env, &stderr, "improve disabled")
	for _, fake := range fakes {
		if calls := proctest.Calls(t, fake); len(calls) != 0 {
			t.Fatalf("%s ran: %+v", filepath.Base(fake), calls)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Fatalf("state written: %v", err)
	}
}

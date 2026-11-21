package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestGhVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		gh               *proctest.Response
		stack            proctest.Response
		installed, model string
		finding          bool
	}{
		{"equal", &ghVersion, stackModel, "v2.105.0", "v2.105.0", false},
		{
			"differ", &proctest.Response{Args: []string{"--version"}, Stdout: "gh version 2.104.1 (2026-10-01)\nhttps://x\n"},
			stackModel, "v2.104.1", "v2.105.0", true,
		},
		{"gh missing", nil, stackModel, unknown, "v2.105.0", true},
		{
			"old stack", &ghVersion,
			proctest.Response{Args: []string{"--version", "--json"}, Stdout: "stack dev\n"},
			"v2.105.0", unknown, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if tc.gh != nil {
				f.fake("gh", ghAuthOK, *tc.gh)
			}
			f.fake("stack", tc.stack)
			resp, l := f.run()
			if l.Gh.Installed != tc.installed || l.Gh.Model != tc.model {
				t.Errorf("gh = %+v", l.Gh)
			}
			got := findings(resp, kindGhVersion)
			if (len(got) == 1) != tc.finding || len(got) > 1 {
				t.Fatalf("gh-version findings = %+v", got)
			}
			if tc.finding {
				if !strings.Contains(got[0].Detail, tc.installed) ||
					!strings.Contains(got[0].Detail, tc.model) {
					t.Errorf("detail = %q", got[0].Detail)
				}
				if resp.Status != StatusWarn {
					t.Errorf("status = %q", resp.Status)
				}
			}
			if !tc.finding && tc.gh != nil && resp.Status != StatusPass {
				t.Errorf(
					"status = %q, findings %+v, sources %+v",
					resp.Status,
					resp.Findings,
					l.Sources,
				)
			}
		})
	}
}

// TestLatestFile changes package hooks, so it does not run in parallel.
func TestLatestFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observe", "latest.json")
	old := []byte(`{"version":1,"old":true}` + "\n")
	if err := writeLatest(path, old); err != nil {
		t.Fatal(err)
	}
	newData := []byte(`{"version":1,"items":["` + strings.Repeat("n", 4096) + `"]}` + "\n")
	reached, release := make(chan struct{}), make(chan struct{})
	afterPartialWrite = func() {
		close(reached)
		<-release
	}
	t.Cleanup(func() { afterPartialWrite = nil })
	done := make(chan error, 1)
	go func() { done <- writeLatest(path, newData) }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("writer never reached the partial write")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, old) {
		t.Fatalf("during the write latest.json = %q, %v; want the old bytes", got, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	afterPartialWrite = nil
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, newData) {
		t.Fatalf("after the rename latest.json = %d bytes, %v", len(got), err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
}

// TestLatestFileRun covers latest.json through Run; it replaces renameFile, so it is not parallel.
func TestLatestFileRun(t *testing.T) {
	old := []byte(`{"version":1,"old":true}` + "\n")
	t.Run("rename failure keeps the old file", func(t *testing.T) {
		f := newFixture(t)
		f.fake("gh", ghAuthOK, ghVersion)
		f.fake("stack", stackModel)
		prev := f.write(".local/state/stack/observe/latest.json", string(old))
		renameFile = func(string, string) error { return errors.New("rename refused") }
		t.Cleanup(func() { renameFile = os.Rename })
		resp := Run(context.Background(), f.opts)
		checkResponse(t, resp)
		got := findings(resp, kindLatestMissing)
		if resp.Status != StatusWarn || len(got) != 1 ||
			!strings.Contains(got[0].Detail, "rename refused") {
			t.Fatalf("status %q, findings %+v", resp.Status, resp.Findings)
		}
		if data, err := os.ReadFile(prev); err != nil || !bytes.Equal(data, old) {
			t.Errorf("latest.json = %q, %v; want the old bytes", data, err)
		}
		entries, _ := os.ReadDir(filepath.Dir(prev))
		if len(entries) != 1 {
			t.Errorf("temp files left: %v", entries)
		}
		if !strings.Contains(resp.Context, "not written") {
			t.Errorf("context = %q", resp.Context)
		}
	})
	t.Run("no home", func(t *testing.T) {
		f := newFixture(t)
		f.opts.Home = ""
		resp := Run(context.Background(), f.opts)
		checkResponse(t, resp)
		if resp.Status != StatusWarn || len(findings(resp, kindLatestMissing)) != 1 {
			t.Fatalf("status %q, findings %+v", resp.Status, resp.Findings)
		}
	})
	t.Run("schema", func(t *testing.T) {
		f := newFixture(t)
		f.fake("gh", ghAuthOK, ghVersion)
		f.fake("stack", stackModel)
		resp, _ := f.run()
		data, err := os.ReadFile(latestPath(f.home))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"version", "window", "sources", "tally", "items", "replay", "gh"} {
			if _, ok := doc[k]; !ok {
				t.Errorf("latest.json lacks %q", k)
			}
		}
		sources, _ := doc["sources"].(map[string]any)
		for _, k := range sourceNames {
			if _, ok := sources[k].(map[string]any)["status"]; !ok {
				t.Errorf("source %s has no status", k)
			}
		}
		if _, ok := sources["gh"].(map[string]any)["repos_read"]; !ok {
			t.Error("gh has no repos_read")
		}
		if resp.Status != StatusPass {
			t.Errorf("status = %q, findings %+v", resp.Status, resp.Findings)
		}
		if !strings.Contains(
			resp.Context,
			"observe window 2026-11-14T00:00:00Z .. 2026-11-21T00:00:00Z\nsources: transcripts off;",
		) || !strings.Contains(resp.Context, "\nreplay: compared 0, changed 0") {
			t.Errorf("context = %q", resp.Context)
		}
	})
}

func TestDeadline(t *testing.T) {
	t.Parallel()
	const budget, reserve = 3 * time.Second, time.Second
	slack := 3 * time.Second
	t.Run("gh and later sources", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.opts.Budget, f.opts.Reserve = budget, reserve
		f.calls("r", bashCall(hourAgo, f.cwd, cmd("b"), guardDenySecrets))
		f.fake(
			"gh",
			proctest.Response{Args: []string{"auth", "status"}, Sleep: time.Minute},
			ghVersion,
		)
		f.fake(
			"stack",
			stackModel,
			proctest.Response{Args: []string{"hook", "claude", "pre-tool-use"}},
		)
		start := time.Now()
		resp, l := f.run()
		if took := time.Since(start); took > budget+slack {
			t.Fatalf("took %s", took)
		}
		if l.Sources["gh"].Status != srcTimeout || l.Sources["replay"].Status != srcTimeout {
			t.Errorf("gh %+v replay %+v, want timeout", l.Sources["gh"], l.Sources["replay"])
		}
		if l.Replay != (ReplayTotals{TruncatedTime: 1}) {
			t.Errorf("replay = %+v", l.Replay)
		}
		if resp.Status != StatusWarn {
			t.Errorf("status = %q", resp.Status)
		}
	})
	t.Run("replay cut by time and by cap", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.opts.Budget, f.opts.Reserve = budget, reserve
		f.opts.replayCap = 1
		f.calls(
			"r",
			bashCall(hourAgo, f.cwd, cmd("a"), "ok"),
			bashCall(hourAgo, f.cwd, cmd("b"), "ok"),
			bashCall(hourAgo, f.cwd, cmd("c"), "ok"),
		)
		f.fake(
			"stack",
			stackModel,
			proctest.Response{Args: []string{"hook", "claude", "pre-tool-use"}, Sleep: time.Minute},
		)
		f.opts.replayTimeout = time.Minute
		start := time.Now()
		resp, l := f.run()
		if took := time.Since(start); took > budget+slack {
			t.Fatalf("took %s", took)
		}
		if l.Replay != (ReplayTotals{TruncatedCap: 2, TruncatedTime: 1}) {
			t.Errorf("replay = %+v", l.Replay)
		}
		if l.Sources["replay"].Status != srcTimeout || resp.Status != StatusWarn {
			t.Errorf("replay %+v status %q", l.Sources["replay"], resp.Status)
		}
	})
}

// TestProtocol runs stack-observe through the real `stack run cycle --ext observe`.
func TestProtocol(t *testing.T) {
	t.Parallel()
	needBuilt(t)
	for _, tc := range []struct {
		name   string
		gh     []proctest.Response
		env    []string
		status string
		limit  time.Duration
	}{
		{"pass", []proctest.Response{ghAuthOK, ghVersion}, nil, StatusPass, time.Minute},
		{
			"one-second budget",
			[]proctest.Response{{Args: []string{"auth", "status"}, Sleep: time.Minute}, ghVersion},
			[]string{"STACK_OBSERVE_BUDGET=1s", "STACK_OBSERVE_RESERVE=300ms"},
			StatusWarn, 15 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, bin := t.TempDir(), t.TempDir()
			proctest.InstallAt(t, bin, "gh", proctest.Scenario{Responses: tc.gh})
			env := []string{
				"PATH=" + built.dir + string(
					os.PathListSeparator,
				) + bin,
				"HOME=" + home,
				"USERPROFILE=" + home,
			}
			for _, k := range []string{"SYSTEMROOT", "COMSPEC", "TEMP", "TMP", "TMPDIR"} {
				if v, ok := os.LookupEnv(k); ok {
					env = append(env, k+"="+v)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			c := exec.CommandContext(ctx, built.stack, "run", "cycle", "--ext", "observe")
			c.Dir = t.TempDir()
			c.Env = append(slices.Clone(env), tc.env...)
			var stdout, stderr bytes.Buffer
			c.Stdout, c.Stderr = &stdout, &stderr
			start := time.Now()
			if err := c.Run(); err != nil {
				t.Fatalf(
					"stack run: %v\nstdout %s\nstderr %s",
					err,
					stdout.String(),
					stderr.String(),
				)
			}
			if took := time.Since(start); took > tc.limit {
				t.Fatalf("took %s", took)
			}
			var agg struct {
				Stage   string `json:"stage"`
				Status  string `json:"status"`
				Results []struct {
					Name     string            `json:"name"`
					Status   string            `json:"status"`
					Error    string            `json:"error"`
					Findings []json.RawMessage `json:"findings"`
				} `json:"results"`
				Skipped []string `json:"skipped"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &agg); err != nil {
				t.Fatalf("aggregate %q: %v", stdout.String(), err)
			}
			if agg.Stage != "cycle" || len(agg.Results) != 1 || agg.Results[0].Name != "observe" ||
				agg.Results[0].Error != "" || agg.Results[0].Status != tc.status || agg.Results[0].Findings == nil {
				t.Fatalf("aggregate = %s\nstderr %s", stdout.String(), stderr.String())
			}
			if _, err := os.Stat(latestPath(home)); err != nil {
				t.Errorf("latest.json: %v", err)
			}
		})
	}
}

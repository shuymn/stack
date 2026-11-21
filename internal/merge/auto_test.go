package merge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/github"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// The argv of the --auto calls, spelled out so the tests pin the contract, not the helpers.
var (
	autoView = []string{
		"pr", "view", "7", "--json",
		"headRefOid,baseRefName,state,isDraft,mergeable,autoMergeRequest",
	}
	autoMergeCall = []string{"pr", "merge", "7", "--merge", "--auto", "--match-head-commit", head}
	autoRead      = []string{
		"pr", "view", "7", "--json", "state,headRefOid,mergeCommit,autoMergeRequest",
	}
	autoDisable = []string{"pr", "merge", "7", "--disable-auto"}
)

// autoPre is the argv of steps 3-8 before the merge call, the checks last.
func autoPre() [][]string {
	a := argv("merge", github.MergeArgs{})
	return [][]string{autoView, a[kRepoView], a[kCommits], a[kQueue], a[kChecks]}
}

func autoViewResponse(autoMerge string) proctest.Response {
	return proctest.Response{Stdout: `{"headRefOid":"` + head + `","baseRefName":"main",` +
		`"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","autoMergeRequest":` + autoMerge + `}`}
}

// rb is a read-back response; merge is the merge OID or empty for none.
func rb(state, oid, merge string, autoMerge bool) proctest.Response {
	mc, am := "null", "null"
	if merge != "" {
		mc = `{"oid":"` + merge + `"}`
	}
	if autoMerge {
		am = `{"enabledAt":"2026-01-01T00:00:00Z","mergeMethod":"MERGE"}`
	}
	return proctest.Response{Stdout: `{"state":"` + state + `","headRefOid":"` + oid +
		`","mergeCommit":` + mc + `,"autoMergeRequest":` + am + `}`}
}

// fakeClock is the clock of the --auto wait: Sleep advances it at once.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
	// onSleep runs after the nth sleep advanced the clock.
	onSleep func(n int)
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	if c.onSleep != nil {
		c.onSleep(len(c.sleeps))
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	return nil
}

// autoRunner logs every gh argv in order and sends each read-back to the fake scripted for it: the
// polls in turn (the last one repeats), and the read after the disable call to final.
type autoRunner struct {
	proc.Exec

	gh     string
	polls  []string
	final  string
	cancel func()
	// stopInMerge cancels the run shortly after the merge call starts, as a signal does.
	stopInMerge bool
	// cancelBeforeMerge cancels the run before the merge call starts; missingAtMerge makes the
	// merge call's executable missing.
	cancelBeforeMerge, missingAtMerge bool

	mu       sync.Mutex
	calls    [][]string
	nPoll    int
	disabled bool
}

func (r *autoRunner) Run(ctx context.Context, c proc.Cmd) (proc.Result, error) {
	if c.Name == r.gh {
		r.mu.Lock()
		r.calls = append(r.calls, slices.Clone(c.Args))
		switch {
		case slices.Equal(c.Args, autoRead) && r.disabled:
			c.Name = r.final
		case slices.Equal(c.Args, autoRead) && len(r.polls) > 0:
			c.Name = r.polls[min(r.nPoll, len(r.polls)-1)]
			r.nPoll++
		case slices.Equal(c.Args, autoDisable):
			r.disabled = true
		case len(c.Args) > 1 && c.Args[1] == "merge":
			switch {
			case r.stopInMerge:
				time.AfterFunc(300*time.Millisecond, r.cancel)
			case r.cancelBeforeMerge:
				r.cancel()
			case r.missingAtMerge:
				c.Name = filepath.Join(os.TempDir(), "stack-missing-gh", "gh")
			}
		}
		r.mu.Unlock()
	}
	return r.Exec.Run(ctx, c)
}

type autoCase struct {
	view    string // autoMergeRequest in step 3; default null
	opts    func(*Options)
	merge   proctest.Response
	disable proctest.Response
	polls   []proctest.Response
	final   proctest.Response
	// cancelAt cancels the run during the nth sleep of the wait; deadlineAt moves the clock to
	// the wait's deadline after the nth sleep.
	deadlineAt int
	// cancelAt cancels the run during the nth sleep of the wait.
	cancelAt     int
	brokenStdout bool
	// stdout also receives every write to stdout, after the outcome's buffer.
	stdout                                         io.Writer
	stopInMerge, cancelBeforeMerge, missingAtMerge bool
}

type autoOutcome struct {
	err            error
	stdout, stderr string
	gh             [][]string
	sleeps         []time.Duration
}

func runAuto(t *testing.T, tc autoCase) autoOutcome {
	t.Helper()
	r := gittest.New(t)
	o := Options{PR: "7", Head: head[:7], RetryDelay: time.Millisecond}
	o.Merge.Auto = true
	if tc.opts != nil {
		tc.opts(&o)
	}
	if tc.view == "" {
		tc.view = "null"
	}
	ok := okResponses()
	var sc proctest.Scenario
	for _, resp := range []struct {
		args []string
		r    proctest.Response
	}{
		{autoView, autoViewResponse(tc.view)},
		{argv("merge", o.Merge)[kRepoView], ok[kRepoView]},
		{argv("merge", o.Merge)[kCommits], ok[kCommits]},
		{argv("merge", o.Merge)[kChecks], ok[kChecks]},
		{argv("merge", o.Merge)[kQueue], ok[kQueue]},
		{github.MergeArgv(prNum, "merge", head, o.Merge), tc.merge},
		{autoDisable, tc.disable},
	} {
		resp.r.Args = resp.args
		sc.Responses = append(sc.Responses, resp.r)
	}
	install := func(resp proctest.Response) string {
		resp.Args = autoRead
		return proctest.Install(t, "gh", proctest.Scenario{Responses: []proctest.Response{resp}})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := &autoRunner{
		gh: proctest.Install(t, "gh", sc), final: install(tc.final),
		cancel: cancel, stopInMerge: tc.stopInMerge,
		cancelBeforeMerge: tc.cancelBeforeMerge, missingAtMerge: tc.missingAtMerge,
	}
	for _, p := range tc.polls {
		runner.polls = append(runner.polls, install(p))
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	clock.onSleep = func(n int) {
		if n == tc.cancelAt {
			cancel()
		}
		if n == tc.deadlineAt {
			clock.now = start.Add(AutoWait)
		}
	}
	var stdout, stderr bytes.Buffer
	o.Client = github.Client{Runner: runner, Gh: runner.gh, Dir: r.Dir, Env: r.Env}
	o.Stdout, o.Stderr = &stdout, &stderr
	if tc.stdout != nil {
		o.Stdout = io.MultiWriter(&stdout, tc.stdout)
	}
	if tc.brokenStdout {
		o.Stdout = brokenWriter{}
	}
	o.Now, o.Sleep = clock.Now, clock.Sleep
	err := Run(ctx, o)
	return autoOutcome{err, stdout.String(), stderr.String(), runner.calls, clock.sleeps}
}

// exitCode is the exit status cmd/stack gives err.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrRefused):
		return 1
	default:
		return 2
	}
}

func repeatN[T any](n int, v T) []T {
	out := make([]T, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// autoTest is one TestAuto case.
type autoTest struct {
	name   string
	tc     autoCase
	code   int
	stdout string
	// want are substrings of the error, or of stderr on success.
	want   []string
	argv   [][]string
	sleeps []time.Duration
	// waits is how often stderr says it is waiting for GitHub.
	waits int
}

// TestAuto covers C28: --auto submits only after steps 1-8 pass, succeeds only on a read-back of
// MERGED on the checked head with a full merge OID, and on every stop condition attempts to
// disable auto-merge and decides by a final read-back.
func TestAuto(t *testing.T) {
	t.Parallel()
	for _, tt := range autoTests() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkAuto(t, tt, runAuto(t, tt.tc))
		})
	}
}

// Fixtures shared by the TestAuto cases.
const (
	autoMerged  = "MERGED " + head + " " + mergeOID + "\n"
	autoStopped = "PR 7 is not merged; auto-merge disabled: stack pr merge was stopped"
	unknownAuto = "result unknown for PR 7 — auto-merge may still be enabled; " +
		"check the PR before retrying"
)

var (
	openAuto    = rb("OPEN", head, "", true)
	openOff     = rb("OPEN", head, "", false)
	autoFailing = proctest.Response{Stderr: "HTTP 502\n", Exit: 1}
)

// seq is the argv of steps 3-8 followed by tail.
func seq(tail ...[]string) [][]string { return append(autoPre(), tail...) }

func autoTests() []autoTest {
	return append(submitTests(), stopTests()...)
}

// submitTests are the cases up to a verified merge or a refusal without a stop condition.
func submitTests() []autoTest {
	merged, failing := autoMerged, autoFailing
	return []autoTest{
		{
			name: "existing request refused",
			tc:   autoCase{view: `{"enabledAt":"2026-01-01T00:00:00Z"}`},
			code: 1, want: []string{"PR 7 already has auto-merge enabled"},
			argv: [][]string{autoView},
		},
		{
			name: "unreadable request refused",
			tc:   autoCase{view: `"yes"`},
			code: 1, want: []string{"could not read PR 7", "unexpected autoMergeRequest"},
			argv: [][]string{autoView},
		},
		{
			name: "immediate merge",
			tc: autoCase{
				opts:  func(o *Options) { o.Merge.DeleteBranch = true },
				polls: []proctest.Response{rb("MERGED", head, mergeOID, false)},
			},
			code: 0, stdout: merged,
			argv: seq(append(slices.Clone(autoMergeCall), "--delete-branch"), autoRead),
		},
		{
			name: "delayed merge",
			tc: autoCase{polls: []proctest.Response{
				openAuto, openAuto, rb("MERGED", head, mergeOID, false),
			}},
			code: 0, stdout: merged,
			want:   []string{"waiting up to 15m0s for GitHub to merge PR 7 on " + head},
			argv:   seq(autoMergeCall, autoRead, autoRead, autoRead),
			sleeps: []time.Duration{20 * time.Second, 20 * time.Second},
			waits:  1,
		},
		{
			name: "cancel before gh starts",
			tc:   autoCase{cancelBeforeMerge: true},
			code: 1,
			want: []string{
				"PR 7 is not merged: gh pr merge --auto did not run",
				"context canceled",
			},
			argv: seq(autoMergeCall),
		},
		{
			name: "gh missing at the merge",
			tc:   autoCase{missingAtMerge: true},
			code: 1,
			want: []string{"PR 7 is not merged: gh pr merge --auto did not run"},
			argv: seq(autoMergeCall),
		},
		{
			name: "merged without a merge commit to the deadline",
			tc: autoCase{
				polls: []proctest.Response{rb("MERGED", head, "", false)},
				final: failing,
			},
			code: 1,
			want: []string{
				unknownAuto,
				"the 15m0s deadline expired before the merge could be confirmed " +
					"(last read back MERGED " + head + ")",
				"the read-back failed",
			},
			argv: seq(append(append([][]string{autoMergeCall}, repeatN(45, autoRead)...),
				autoDisable, autoRead)...),
			sleeps: repeatN(45, 20*time.Second),
		},
		{
			name: "merged with a broken stdout",
			tc: autoCase{
				brokenStdout: true,
				polls:        []proctest.Response{rb("MERGED", head, mergeOID, false)},
			},
			code: 0,
			want: []string{
				"stack pr merge: " + strings.TrimSuffix(merged, "\n") +
					" (writing stdout failed: broken pipe)\n",
			},
			argv: seq(autoMergeCall, autoRead),
		},
		{
			name: "non-zero gh then verified merge",
			tc: autoCase{
				merge: proctest.Response{Stderr: "GraphQL: timeout\n", Exit: 1},
				polls: []proctest.Response{rb("MERGED", head, mergeOID, false)},
			},
			code: 0, stdout: merged,
			want: []string{"GraphQL: timeout", "PR 7 is merged, but gh pr merge exited 1"},
			argv: seq(autoMergeCall, autoRead),
		},
	}
}

// stopTests are the cases that end in a stop condition, the disable and the final read-back.
func stopTests() []autoTest {
	merged, failing, stopped := autoMerged, autoFailing, autoStopped
	return []autoTest{
		{
			name: "non-zero gh then open disables without polling",
			tc: autoCase{
				merge: proctest.Response{Stderr: "GraphQL: timeout\n", Exit: 1},
				polls: []proctest.Response{openAuto},
				final: openOff,
			},
			code: 1,
			want: []string{
				"PR 7 is not merged; auto-merge disabled: gh pr merge --auto failed; " +
					"gh pr merge --auto exited 1",
			},
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
		{
			name: "open on the head with no request stops",
			tc:   autoCase{polls: []proctest.Response{openOff}, final: openOff},
			code: 1,
			want: []string{
				"PR 7 is not merged; auto-merge disabled: no auto-merge request is active on " + head,
			},
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
		{
			name: "merged other head",
			tc: autoCase{polls: []proctest.Response{
				openAuto, rb("MERGED", other, mergeOID, false),
			}},
			code: 1,
			want: []string{
				"PR 7 was merged on " + other + ", not on the checked " + head,
				"inspect the PR",
			},
			argv:   seq(autoMergeCall, autoRead, autoRead),
			sleeps: []time.Duration{20 * time.Second},
			waits:  1,
		},
		{
			name: "moved head disables",
			tc: autoCase{
				polls: []proctest.Response{openAuto, rb("OPEN", other, "", true)},
				final: rb("OPEN", other, "", false),
			},
			code: 1,
			want: []string{
				"PR 7 is not merged; auto-merge disabled: the head moved to " + other,
			},
			argv:   seq(autoMergeCall, autoRead, autoRead, autoDisable, autoRead),
			sleeps: []time.Duration{20 * time.Second},
			waits:  1,
		},
		{
			name: "closed disables",
			tc: autoCase{
				polls: []proctest.Response{rb("CLOSED", head, "", false)},
				final: rb("CLOSED", head, "", false),
			},
			code: 1, want: []string{unknownAuto, "read back CLOSED " + head, "the PR was closed"},
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
		{
			name: "unreadable polls to the deadline",
			tc: autoCase{
				polls: []proctest.Response{
					{Stdout: "not json"},
					{Stdout: `{"state":"OPEN","headRefOid":"` + head + `"}`},
					openAuto, failing,
				},
				final: openOff,
			},
			code: 1,
			want: []string{
				"PR 7 is not merged; auto-merge disabled: not merged within 15m0s " +
					"(last read back: gh failed: pr view: exit 1: HTTP 502)",
			},
			argv: seq(append(append([][]string{autoMergeCall}, repeatN(45, autoRead)...),
				autoDisable, autoRead)...),
			sleeps: repeatN(45, 20*time.Second),
			waits:  1,
		},
		{
			name: "disable fails then open with no request",
			tc: autoCase{
				disable: proctest.Response{Stderr: "HTTP 502\n", Exit: 1},
				polls:   []proctest.Response{rb("OPEN", other, "", true)},
				final:   rb("OPEN", head, "", false),
			},
			code: 1,
			want: []string{
				"PR 7 is not merged; auto-merge disabled: the head moved to " + other,
				"--disable-auto exited 1",
			},
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
		{
			name: "disable fails then unreadable request",
			tc: autoCase{
				disable: proctest.Response{Stderr: "HTTP 502\n", Exit: 1},
				polls:   []proctest.Response{rb("OPEN", other, "", true)},
				final: proctest.Response{
					Stdout: `{"state":"OPEN","headRefOid":"` + head + `","mergeCommit":null}`,
				},
			},
			code: 1,
			want: []string{unknownAuto, "no autoMergeRequest field", "--disable-auto exited 1"},
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
		{
			name: "disable fails then merged",
			tc: autoCase{
				disable: proctest.Response{Stderr: "HTTP 502\n", Exit: 1},
				polls:   []proctest.Response{rb("CLOSED", head, "", false)},
				final:   rb("MERGED", head, mergeOID, false),
			},
			code: 0, stdout: merged,
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
		{
			name: "disable fails then active request",
			tc: autoCase{
				disable: proctest.Response{Stderr: "HTTP 502\n", Exit: 1},
				polls:   []proctest.Response{rb("OPEN", other, "", true)},
				final:   rb("OPEN", other, "", true),
			},
			code: 1,
			want: []string{unknownAuto, "(auto-merge enabled)", "--disable-auto exited 1"},
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
		{
			name: "final read-back fails",
			tc: autoCase{
				polls: []proctest.Response{rb("CLOSED", head, "", false)},
				final: failing,
			},
			code: 1, want: []string{unknownAuto, "the read-back failed"},
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
		{
			name: "merge racing the disable",
			tc: autoCase{
				disable:  proctest.Response{Stderr: "Pull request is already merged\n", Exit: 1},
				polls:    []proctest.Response{openAuto},
				final:    rb("MERGED", head, mergeOID, false),
				cancelAt: 1,
			},
			code: 0, stdout: merged,
			argv:   seq(autoMergeCall, autoRead, autoDisable, autoRead),
			sleeps: []time.Duration{20 * time.Second},
			waits:  1,
		},
		{
			name: "cancel disables",
			tc:   autoCase{polls: []proctest.Response{openAuto}, final: openOff, cancelAt: 1},
			code: 1, want: []string{stopped},
			argv:   seq(autoMergeCall, autoRead, autoDisable, autoRead),
			sleeps: []time.Duration{20 * time.Second},
			waits:  1,
		},
		{
			name: "cancel during gh disables",
			tc: autoCase{
				merge: proctest.Response{Sleep: 20 * time.Second},
				polls: []proctest.Response{openAuto}, final: openOff, stopInMerge: true,
			},
			code: 1, want: []string{stopped},
			argv: seq(autoMergeCall, autoRead, autoDisable, autoRead),
		},
	}
}

func checkAuto(t *testing.T, tt autoTest, out autoOutcome) {
	t.Helper()
	if got := exitCode(out.err); got != tt.code {
		t.Errorf("exit = %d (err %v), want %d", got, out.err, tt.code)
	}
	if out.stdout != tt.stdout {
		t.Errorf("stdout = %q, want %q", out.stdout, tt.stdout)
	}
	text := out.stderr
	if out.err != nil {
		text = out.err.Error()
		if strings.Contains(text, "\n") {
			t.Errorf("reason spans lines: %q", text)
		}
	}
	for _, w := range tt.want {
		if !strings.Contains(text, w) {
			t.Errorf("got %q, want it to contain %q", text, w)
		}
	}
	if !slices.EqualFunc(out.gh, tt.argv, slices.Equal) {
		i := 0
		for i < min(len(out.gh), len(tt.argv)) && slices.Equal(out.gh[i], tt.argv[i]) {
			i++
		}
		t.Errorf("gh argv: %d calls, want %d; first difference at call %d: got %q",
			len(out.gh), len(tt.argv), i, out.gh[i:min(i+1, len(out.gh))])
	}
	if n := strings.Count(out.stderr, "waiting up to"); n != tt.waits {
		t.Errorf("waiting line printed %d times, want %d: %q", n, tt.waits, out.stderr)
	}
	if !slices.Equal(out.sleeps, tt.sleeps) {
		t.Errorf("sleeps = %v, want %v", out.sleeps, tt.sleeps)
	}
}

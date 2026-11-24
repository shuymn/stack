package merge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/github"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestMain(m *testing.M) {
	proctest.Main()
	os.Exit(gittest.Main(m))
}

const (
	head     = "0123456789abcdef0123456789abcdef01234567"
	other    = "89abcdef0123456789abcdef0123456789abcdef"
	mergeOID = "fedcba9876543210fedcba9876543210fedcba98"
	prNum    = 7
)

var repo = github.Repo{Owner: "octo", Name: "demo"}

// scenario names each gh call of a merge; tests replace single entries.
type scenario map[string]proctest.Response

const (
	kPRView   = "pr view"
	kRepoView = "repo view"
	kCommits  = "commits"
	kChecks   = "checks"
	kChecks2  = "checks page 2"
	kQueue    = "queue"
	kMerge    = "merge"
	kReadBack = "readback"
)

func okResponses() scenario {
	return scenario{
		kPRView: {Stdout: `{"headRefOid":"` + head + `","baseRefName":"main","state":"OPEN",` +
			`"isDraft":false,"mergeable":"MERGEABLE"}`},
		kRepoView: {Stdout: `{"nameWithOwner":"octo/demo","mergeCommitAllowed":true,` +
			`"squashMergeAllowed":false,"rebaseMergeAllowed":false}`},
		kCommits: {Stdout: `{"commits":[{"messageHeadline":"feat: add x"}]}`},
		kChecks: {Stdout: checksPage(head, 2, false, "",
			`{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}`,
			`{"__typename":"StatusContext","context":"ci/legacy","state":"SUCCESS"}`)},
		kQueue: {Stdout: `{"data":{"repository":{"pullRequest":{"isMergeQueueEnabled":false}}}}`},
		kMerge: {Stdout: "merged\n"},
		kReadBack: {
			Stdout: `{"state":"MERGED","headRefOid":"` + head + `","mergeCommit":{"oid":"` +
				mergeOID + `"}}`,
		},
	}
}

func checksPage(oid string, total int, next bool, cursor string, nodes ...string) string {
	n := "false"
	if next {
		n = "true"
	}
	return `{"data":{"repository":{"object":{"oid":"` + oid + `","statusCheckRollup":{"contexts":` +
		`{"totalCount":` + strconv.Itoa(total) + `,"pageInfo":{"hasNextPage":` + n + `,"endCursor":"` +
		cursor + `"},"nodes":[` + strings.Join(nodes, ",") + `]}}}}}}`
}

// argv returns the argv of each scenario key for a merge with method and args.
func argv(method string, a github.MergeArgs) map[string][]string {
	pr := "7"
	return map[string][]string{
		kPRView: {"pr", "view", pr, "--json", "headRefOid,baseRefName,state,isDraft,mergeable"},
		kRepoView: {
			"repo",
			"view",
			"--json",
			"nameWithOwner,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed",
		},
		kCommits:  {"pr", "view", pr, "--json", "commits"},
		kChecks:   github.ChecksArgs(repo, head, ""),
		kChecks2:  github.ChecksArgs(repo, head, "c1"),
		kQueue:    github.MergeQueueArgs(repo, prNum),
		kMerge:    github.MergeArgv(prNum, method, head, a),
		kReadBack: {"pr", "view", pr, "--json", "state,headRefOid,mergeCommit"},
	}
}

type outcome struct {
	err    error
	stdout string
	stderr string
	gh     []proctest.Call
	// git are the calls of the fake git of setup.fakeGit; dir is the fixture repository.
	git []proctest.Call
	dir string
	// stop is the stop of setup.stop.
	stop *stopAtMerge
}

// merged reports whether the fake gh saw a `gh pr merge` call.
func (o outcome) merged() bool {
	return slices.ContainsFunc(o.gh, func(c proctest.Call) bool {
		return len(c.Args) >= 2 && c.Args[0] == "pr" && c.Args[1] == "merge"
	})
}

type setup struct {
	opts   func(*Options)
	method string // the method gh pr merge is expected with; default "merge"
	before func(t *testing.T, dir string)
	// git replaces the git executable.
	git string
	// stop cancels the run's context once the fake gh pr merge is running.
	stop bool
	// outside makes git report that the directory is not in a work tree.
	outside bool
	// brokenStdout makes every write to stdout fail, as a closed pipe does.
	brokenStdout bool
	// fakeGit makes git a fake that prints the fixture's top level, slash-separated as git does.
	fakeGit bool
	// stdout also receives every write to stdout, after the outcome's buffer.
	stdout io.Writer
	// withCancel receives the cancel function of the run's context.
	withCancel func(context.CancelFunc)
}

// run executes Run in a fixture repository with a fake gh answering s.
func run(t *testing.T, s scenario, st setup) outcome {
	t.Helper()
	r := gittest.New(t)
	if st.before != nil {
		st.before(t, r.Dir)
	}
	o := Options{PR: "7", Head: head[:7], RetryDelay: time.Millisecond}
	if st.opts != nil {
		st.opts(&o)
	}
	method := st.method
	if method == "" {
		method = "merge"
	}
	args := argv(method, o.Merge)
	var stop *stopAtMerge
	if st.stop {
		// The fake writes the file once it runs, before its output and sleep.
		stop = &stopAtMerge{ready: filepath.Join(t.TempDir(), "merge-running")}
		resp := s[kMerge]
		resp.WriteFiles = map[string]string{stop.ready: ""}
		s[kMerge] = resp
	}
	var sc proctest.Scenario
	for k, resp := range s {
		resp.Args = args[k]
		sc.Responses = append(sc.Responses, resp)
	}
	gh := proctest.Install(t, "gh", sc)
	var stdout, stderr bytes.Buffer
	if st.fakeGit {
		st.git = proctest.Install(t, "git", proctest.Scenario{Responses: []proctest.Response{{
			Args: []string{"rev-parse", "--show-toplevel"}, Stdout: filepath.ToSlash(r.Dir) + "\n",
		}}})
	}
	if st.outside {
		st.git = proctest.Install(t, "git", proctest.Scenario{Responses: []proctest.Response{{
			Args:   []string{"rev-parse", "--show-toplevel"},
			Stderr: "fatal: this operation must be run in a work tree\n", Exit: 128,
		}}})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if st.withCancel != nil {
		st.withCancel(cancel)
	}
	var runner proc.Runner = proc.Exec{}
	if stop != nil {
		stop.cancel = cancel
		runner = stop
	}
	o.Client = github.Client{Runner: runner, Gh: gh, Git: st.git, Dir: r.Dir, Env: r.Env}
	o.Stdout, o.Stderr = &stdout, &stderr
	if st.stdout != nil {
		o.Stdout = io.MultiWriter(&stdout, st.stdout)
	}
	if st.brokenStdout {
		o.Stdout = brokenWriter{}
	}
	err := Run(ctx, o)
	out := outcome{
		err: err, stdout: stdout.String(), stderr: stderr.String(),
		gh: proctest.Calls(t, gh), dir: r.Dir, stop: stop,
	}
	if st.fakeGit {
		out.git = proctest.Calls(t, st.git)
	}
	return out
}

// brokenWriter fails every write, as a pipe whose reader closed does.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }

var errBrokenPipe = errors.New("broken pipe")

// TestMergedWithBrokenStdout: a verified merge succeeds also when the result line cannot be written,
// and the line goes to stderr instead.
func TestMergedWithBrokenStdout(t *testing.T) {
	t.Parallel()
	out := run(t, okResponses(), setup{brokenStdout: true})
	want := "stack pr merge: MERGED " + head + " " + mergeOID + " (writing stdout failed: broken pipe)\n"
	if out.err != nil || !strings.HasSuffix(out.stderr, want) {
		t.Errorf("err = %v, stderr = %q; want success and %q", out.err, out.stderr, want)
	}
}

func with(s scenario, k string, resp proctest.Response) scenario {
	s[k] = resp
	return s
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	failing := proctest.Response{Stderr: "HTTP 502\n", Exit: 1}
	view := func(state, draft, mergeable, oid string) proctest.Response {
		return proctest.Response{Stdout: `{"headRefOid":"` + oid + `","baseRefName":"main",` +
			`"state":"` + state + `","isDraft":` + draft + `,"mergeable":"` + mergeable + `"}`}
	}
	checks := func(nodes ...string) proctest.Response {
		return proctest.Response{Stdout: checksPage(head, len(nodes), false, "", nodes...)}
	}
	tests := []struct {
		name  string
		s     scenario
		setup setup
		want  string
		// calls is the number of gh calls expected when positive, none when negative.
		calls int
	}{
		{
			name: "pr view fails",
			s:    with(okResponses(), kPRView, failing),
			want: "could not read PR 7",
		},
		{
			name: "head moved",
			s:    with(okResponses(), kPRView, view("OPEN", "false", "MERGEABLE", other)),
			want: "head is " + other,
		},
		{
			name: "head not a full SHA",
			s:    with(okResponses(), kPRView, view("OPEN", "false", "MERGEABLE", head[:12])),
			want: "no usable head SHA",
		},
		{
			name: "closed",
			s:    with(okResponses(), kPRView, view("CLOSED", "false", "MERGEABLE", head)),
			want: "not open (CLOSED)",
		},
		{
			name: "draft", s: with(okResponses(), kPRView, view("OPEN", "true", "MERGEABLE", head)),
			want: "is a draft",
		},
		{
			name: "conflicting",
			s:    with(okResponses(), kPRView, view("OPEN", "false", "CONFLICTING", head)),
			want: "not mergeable (CONFLICTING",
		},
		{
			name: "unknown after five reads",
			s:    with(okResponses(), kPRView, view("OPEN", "false", "UNKNOWN", head)),
			want: "not mergeable (UNKNOWN", calls: 5,
		},
		{
			name: "repo view fails",
			s:    with(okResponses(), kRepoView, failing),
			want: "could not read the repository",
		},
		{
			name: "no method and two allowed",
			s: with(
				okResponses(),
				kRepoView,
				proctest.Response{Stdout: `{"nameWithOwner":"octo/demo",` +
					`"mergeCommitAllowed":true,"squashMergeAllowed":true,"rebaseMergeAllowed":false}`},
			),
			want: "allows 2 merge methods (merge, squash)",
		},
		{
			name: "commits unreadable", s: with(okResponses(), kCommits, failing),
			want: "could not read the commits",
		},
		{
			name: "fixup commit",
			s: with(okResponses(), kCommits, proctest.Response{
				Stdout: `{"commits":[{"messageHeadline":"feat: x"},{"messageHeadline":"fixup! feat: x"}]}`,
			}),
			want: `fold these commits first (git rebase --autosquash), then check the new head: "fixup! feat: x"`,
		},
		{
			name: "checks query fails",
			s:    with(okResponses(), kChecks, failing),
			want: "could not read the checks",
		},
		{
			name: "checks query GraphQL error",
			s: with(okResponses(), kChecks, proctest.Response{
				Stdout: `{"data":null,"errors":[{"message":"Something went wrong"}]}`,
			}),
			want: "Something went wrong",
		},
		{
			name: "checks answer for another commit",
			s: with(
				okResponses(),
				kChecks,
				proctest.Response{Stdout: checksPage(
					other,
					1,
					false,
					"",
					`{"__typename":"CheckRun","name":"t","status":"COMPLETED","conclusion":"SUCCESS"}`,
				)},
			),
			want: `answered for commit "` + other + `", not ` + head,
		},
		{
			name: "commit not found",
			s: with(okResponses(), kChecks, proctest.Response{
				Stdout: `{"data":{"repository":{"object":null}}}`,
			}),
			want: "commit " + head + " not found",
		},
		{
			name: "no checks without the flag",
			s: with(okResponses(), kChecks, proctest.Response{
				Stdout: `{"data":{"repository":{"object":{"oid":"` + head + `","statusCheckRollup":null}}}}`,
			}),
			want: "no checks ran on " + head,
		},
		{
			name: "zero total without the flag",
			s:    with(okResponses(), kChecks, checks()),
			want: "no checks ran on " + head,
		},
		{
			name: "failing check run",
			s: with(okResponses(), kChecks, checks(
				`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"FAILURE"}`,
			)),
			want: "not every check on " + head + " passed (pending: wait and rerun): lint (COMPLETED/FAILURE)",
		},
		{
			name: "pending check run",
			s: with(okResponses(), kChecks, checks(
				`{"__typename":"CheckRun","name":"test","status":"IN_PROGRESS","conclusion":null}`,
			)),
			want: "test (IN_PROGRESS)",
		},
		{
			name: "failing commit status",
			s: with(okResponses(), kChecks, checks(
				`{"__typename":"StatusContext","context":"ci/x","state":"PENDING"}`)),
			want: "ci/x (PENDING)",
		},
		{
			name: "failure on the second page",
			s: with(
				with(okResponses(), kChecks, proctest.Response{Stdout: checksPage(
					head,
					2,
					true,
					"c1",
					`{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"SUCCESS"}`,
				)}),
				kChecks2,
				proctest.Response{Stdout: checksPage(
					head,
					2,
					false,
					"c2",
					`{"__typename":"CheckRun","name":"b","status":"COMPLETED","conclusion":"CANCELLED"}`,
				)},
			),
			want: "b (COMPLETED/CANCELLED)",
		},
		{
			name: "fewer checks than the total",
			s: with(okResponses(), kChecks, proctest.Response{Stdout: checksPage(
				head,
				3,
				false,
				"",
				`{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"SUCCESS"}`,
			)}),
			want: "read 1 of 3 checks",
		},
		{
			name: "merge queue",
			s: with(okResponses(), kQueue, proctest.Response{
				Stdout: `{"data":{"repository":{"pullRequest":{"isMergeQueueEnabled":true}}}}`,
			}),
			want: "merges through a merge queue",
		},
		{
			name: "merge queue unreadable",
			s:    with(okResponses(), kQueue, failing),
			want: "merge queue: gh failed",
		},
		{
			name: "merge queue field missing",
			s: with(
				okResponses(),
				kQueue,
				proctest.Response{Stdout: `{"data":{"repository":null}}`},
			),
			want: "no isMergeQueueEnabled",
		},
		{
			name:  "toplevel cannot run",
			s:     okResponses(),
			setup: setup{git: filepath.Join(os.TempDir(), "stack-missing-git", "git")},
			want:  "could not find the repository", calls: -1,
		},
		{
			// git exits 128 outside a work tree, e.g. when started inside the .git directory.
			name:  "outside a checkout",
			s:     okResponses(),
			setup: setup{outside: true},
			want:  "not inside a checkout of the PR's repository", calls: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := run(t, tt.s, tt.setup)
			assertRefused(t, out, tt.want)
			if tt.calls < 0 && len(out.gh) != 0 || tt.calls > 0 && len(out.gh) != tt.calls {
				t.Errorf("gh calls = %d, want %d: %+v", len(out.gh), tt.calls, out.gh)
			}
		})
	}
}

func assertRefused(t *testing.T, out outcome, want string) {
	t.Helper()
	if !errors.Is(out.err, ErrRefused) || !strings.Contains(out.err.Error(), want) {
		t.Errorf("err = %v, want a refusal containing %q", out.err, want)
	}
	if out.merged() {
		t.Errorf("gh pr merge was called: %+v", out.gh)
	}
	if out.stdout != "" {
		t.Errorf("stdout = %q, want empty", out.stdout)
	}
	if strings.Contains(out.err.Error(), "\n") {
		t.Errorf("reason spans lines: %q", out.err)
	}
}

func TestInvalidArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts func(*Options)
		want string
	}{
		{"pr not decimal", func(o *Options) { o.PR = "#7" }, "pass the PR number"},
		{"pr overflows", func(o *Options) { o.PR = "99999999999999999999" }, "PR number"},
		{"head too short", func(o *Options) { o.Head = "abc" }, "7-40 lower-case hex"},
		{
			"head upper case",
			func(o *Options) { o.Head = strings.ToUpper(head) },
			"7-40 lower-case hex",
		},
		{"head too long", func(o *Options) { o.Head = head + "0" }, "7-40 lower-case hex"},
		{"GH_REPO", func(o *Options) { o.GHRepo = "octo/other" }, "GH_REPO"},
		{
			"two methods",
			func(o *Options) { o.Methods = []string{"merge", "squash"} },
			"--merge --squash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := run(t, okResponses(), setup{opts: tt.opts})
			if !errors.Is(out.err, ErrInvalid) || !strings.Contains(out.err.Error(), tt.want) {
				t.Errorf("err = %v, want invalid with %q", out.err, tt.want)
			}
			if len(out.gh) != 0 {
				t.Errorf("gh calls = %+v, want none", out.gh)
			}
		})
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()
	subject, body := "-feat: x (#7)", ""
	tests := []struct {
		name   string
		s      scenario
		setup  setup
		argv   []string
		noCall string // a scenario key that must not be called
	}{
		{
			name: "repository's only method",
			s:    okResponses(),
			argv: []string{"pr", "merge", "7", "--merge", "--match-head-commit", head},
		},
		{
			name: "squash with flags skips the fixup check",
			s: with(okResponses(), kCommits, proctest.Response{
				Stdout: `{"commits":[{"messageHeadline":"fixup! x"}]}`,
			}),
			setup: setup{method: "squash", opts: func(o *Options) {
				o.Methods = []string{"squash"}
				o.Merge = github.MergeArgs{DeleteBranch: true, Subject: &subject, Body: &body}
			}},
			argv: []string{
				"pr", "merge", "7", "--squash", "--match-head-commit", head, "--delete-branch",
				"--subject=-feat: x (#7)", "--body=",
			},
			noCall: kCommits,
		},
		{
			name: "no checks with the flag",
			s: with(okResponses(), kChecks, proctest.Response{
				Stdout: `{"data":{"repository":{"object":{"oid":"` + head + `","statusCheckRollup":null}}}}`,
			}),
			setup: setup{opts: func(o *Options) { o.AllowNoChecks = true }},
			argv:  []string{"pr", "merge", "7", "--merge", "--match-head-commit", head},
		},
		{
			name: "two pages, neutral and skipped pass, full head given",
			s: with(
				with(okResponses(), kChecks, proctest.Response{Stdout: checksPage(
					head,
					2,
					true,
					"c1",
					`{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"NEUTRAL"}`,
				)}),
				kChecks2,
				proctest.Response{Stdout: checksPage(
					head,
					2,
					false,
					"c2",
					`{"__typename":"CheckRun","name":"b","status":"COMPLETED","conclusion":"SKIPPED"}`,
				)},
			),
			setup: setup{opts: func(o *Options) { o.Head = head }},
			argv:  []string{"pr", "merge", "7", "--merge", "--match-head-commit", head},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := run(t, tt.s, tt.setup)
			if out.err != nil {
				t.Fatalf("err = %v; stderr %+v; gh %+v", out.err, out.stderr, out.gh)
			}
			if want := "MERGED " + head + " " + mergeOID + "\n"; out.stdout != want {
				t.Errorf("stdout = %q, want %q", out.stdout, want)
			}
			var merges [][]string
			sawChecks := false
			for _, c := range out.gh {
				if len(c.Args) > 1 && c.Args[0] == "pr" && c.Args[1] == "merge" {
					merges = append(merges, c.Args)
				}
				if slices.Equal(c.Args, github.ChecksArgs(repo, head, "")) {
					sawChecks = true
				}
				if tt.noCall != "" &&
					slices.Equal(c.Args, argv("merge", github.MergeArgs{})[tt.noCall]) {
					t.Errorf("unexpected call %q", c.Args)
				}
			}
			if len(merges) != 1 || !slices.Equal(merges[0], tt.argv) {
				t.Errorf("merge calls = %q, want exactly %q", merges, tt.argv)
			}
			assertChecksLast(t, out.gh)
			if !sawChecks {
				t.Errorf("checks query not pinned to the full head: %+v", out.gh)
			}
			if !strings.Contains(out.stderr, "merged\n") ||
				strings.Contains(out.stderr, "do not retry") {
				t.Errorf("stderr = %q, want gh's output and no note", out.stderr)
			}
		})
	}
}

// assertChecksLast asserts that every call between the first checks query and the merge call is a
// checks query of head, so a rerun during the other steps is seen before merging.
func assertChecksLast(t *testing.T, calls []proctest.Call) {
	t.Helper()
	first := slices.IndexFunc(calls, func(c proctest.Call) bool {
		return slices.Equal(c.Args, github.ChecksArgs(repo, head, ""))
	})
	mergeAt := slices.IndexFunc(calls, func(c proctest.Call) bool {
		return len(c.Args) > 1 && c.Args[0] == "pr" && c.Args[1] == "merge"
	})
	if first < 0 || mergeAt < first {
		t.Fatalf("checks query at %d, merge at %d: %+v", first, mergeAt, calls)
	}
	for _, c := range calls[first:mergeAt] {
		if !slices.Contains(c.Args, "oid="+head) {
			t.Errorf("call %q runs between the checks query and the merge", c.Args)
		}
	}
}

func TestReadback(t *testing.T) {
	t.Parallel()
	readback := func(state, oid, merge string) proctest.Response {
		mc := "null"
		if merge != "" {
			mc = `{"oid":"` + merge + `"}`
		}
		return proctest.Response{
			Stdout: `{"state":"` + state + `","headRefOid":"` + oid + `","mergeCommit":` + mc + `}`,
		}
	}
	failedMerge := proctest.Response{Stderr: "failed to delete branch\n", Exit: 1}
	t.Run("merged after a non-zero exit", func(t *testing.T) {
		t.Parallel()
		out := run(t, with(okResponses(), kMerge, failedMerge), setup{})
		if out.err != nil {
			t.Fatalf("err = %v", out.err)
		}
		if out.stdout != "MERGED "+head+" "+mergeOID+"\n" {
			t.Errorf("stdout = %q", out.stdout)
		}
		if !strings.Contains(out.stderr, "failed to delete branch") ||
			!strings.Contains(out.stderr, "gh pr merge exited 1") ||
			!strings.Contains(out.stderr, "do not retry") {
			t.Errorf("stderr = %q, want gh's error and the note", out.stderr)
		}
	})
	sha256 := strings.Repeat("ab", 32)
	t.Run("SHA-256 merge OID", func(t *testing.T) {
		t.Parallel()
		out := run(t, with(okResponses(), kReadBack, readback("MERGED", head, sha256)), setup{})
		if out.err != nil || out.stdout != "MERGED "+head+" "+sha256+"\n" {
			t.Errorf("err = %v, stdout = %q", out.err, out.stdout)
		}
	})
	unknownMsg := "result unknown for PR 7 — check the PR before retrying"
	for _, tt := range []struct {
		name  string
		merge proctest.Response
		back  proctest.Response
		// unknown marks an outcome the read-back cannot confirm.
		unknown bool
		want    string
	}{
		{
			"open after a non-zero exit", failedMerge, readback("OPEN", head, ""), false,
			"PR 7 is not merged on " + head + " (OPEN " + head + "); gh pr merge exited 1",
		},
		{
			"merged on another head",
			proctest.Response{},
			readback("MERGED", other, mergeOID), true,
			"read back MERGED " + other + " " + mergeOID + ", not MERGED on " + head,
		},
		{
			"closed",
			proctest.Response{},
			readback("CLOSED", head, ""), true,
			"read back CLOSED " + head + ",",
		},
		{
			"empty merge OID",
			proctest.Response{},
			readback("MERGED", head, ""), true,
			"read back MERGED " + head + ",",
		},
		{
			"short merge OID",
			proctest.Response{},
			readback("MERGED", head, mergeOID[:9]), true,
			"read back MERGED " + head + " " + mergeOID[:9] + ",",
		},
		{
			"upper-case merge OID",
			proctest.Response{},
			readback("MERGED", head, strings.ToUpper(mergeOID)), true, "not MERGED on " + head,
		},
		{
			"read-back fails", failedMerge,
			proctest.Response{Stderr: "timeout", Exit: 1},
			true,
			"gh pr merge exited 1; the read-back failed",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := run(t, with(with(okResponses(), kMerge, tt.merge), kReadBack, tt.back), setup{})
			if !errors.Is(out.err, ErrRefused) || !strings.Contains(out.err.Error(), tt.want) {
				t.Errorf("err = %v, want a refusal containing %q", out.err, tt.want)
			}
			if got := errors.Is(out.err, ErrUnknown); got != tt.unknown ||
				got != strings.Contains(out.err.Error(), unknownMsg) {
				t.Errorf("err = %v, want unknown %v with %q", out.err, tt.unknown, unknownMsg)
			}
			if strings.Contains(out.err.Error(), "retry)") || out.stdout != "" {
				t.Errorf("err = %v, stdout = %q", out.err, out.stdout)
			}
		})
	}
}

// stopAtMerge cancels the context of the run once the fake gh pr merge has written ready, as
// SIGINT or SIGTERM does through cmd/stack, and records how long the merge call took after that.
type stopAtMerge struct {
	proc.Exec

	cancel func()
	ready  string
	// stopped is the time from the cancel to the merge call's return; zero when no cancel came.
	stopped time.Duration
}

func (s *stopAtMerge) Run(ctx context.Context, c proc.Cmd) (proc.Result, error) {
	if len(c.Args) < 2 || c.Args[0] != "pr" || c.Args[1] != "merge" {
		return s.Exec.Run(ctx, c)
	}
	done := make(chan struct{})
	var at time.Time
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := os.Stat(s.ready); err == nil {
				at = time.Now()
				s.cancel()
				return
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	res, err := s.Exec.Run(ctx, c)
	close(done)
	<-watched
	if !at.IsZero() {
		s.stopped = time.Since(at)
	}
	return res, err
}

// TestStopDuringMerge: a stop after the merge call started kills it and still reads the PR back.
func TestStopDuringMerge(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		back   string
		stdout string
		want   string
	}{
		{"merged", "MERGED", "MERGED " + head + " " + mergeOID + "\n", ""},
		{"open", "OPEN", "", "PR 7 is not merged on " + head},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The merge call outlives the stop by far unless the stop kills it; the bound below
			// times only the merge call after the stop, so slow fixture setup cannot reach it.
			s := with(okResponses(), kMerge, proctest.Response{Sleep: time.Minute})
			s[kReadBack] = proctest.Response{
				Stdout: `{"state":"` + tt.back + `","headRefOid":"` + head +
					`","mergeCommit":{"oid":"` + mergeOID + `"}}`,
			}
			out := run(t, s, setup{stop: true})
			if out.stop.stopped == 0 || out.stop.stopped > 20*time.Second {
				t.Errorf("the stop did not kill the running merge call (%s after the stop)",
					out.stop.stopped)
			}
			if !slices.ContainsFunc(out.gh, func(c proctest.Call) bool {
				return slices.Equal(c.Args, argv("merge", github.MergeArgs{})[kReadBack])
			}) {
				t.Errorf("no read-back after the stop: %+v", out.gh)
			}
			if out.stdout != tt.stdout || tt.want == "" && out.err != nil ||
				tt.want != "" && (out.err == nil || !strings.Contains(out.err.Error(), tt.want)) {
				t.Errorf(
					"err = %v, stdout = %q; want %q, %q",
					out.err,
					out.stdout,
					tt.want,
					tt.stdout,
				)
			}
		})
	}
}

// TestMergeCheck covers C25: absent passes, not runnable and non-zero refuse, each separately.
func TestMergeCheck(t *testing.T) {
	t.Parallel()
	install := func(exit int) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			proctest.InstallAt(t, mkdir(t, filepath.Join(dir, ".claude")), "merge-check",
				proctest.Scenario{Responses: []proctest.Response{{
					Args: []string{"7", head}, Stdout: "bench ok\n", Exit: exit,
				}}})
		}
	}
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		out := run(t, okResponses(), setup{before: func(t *testing.T, dir string) {
			t.Helper()
			mkdir(t, filepath.Join(dir, ".claude"))
		}})
		if out.err != nil || !out.merged() {
			t.Errorf("err = %v, merged %v", out.err, out.merged())
		}
	})
	t.Run("passes with the full head", func(t *testing.T) {
		t.Parallel()
		var check string
		out := run(t, okResponses(), setup{before: func(t *testing.T, dir string) {
			t.Helper()
			install(0)(t, dir)
			check = filepath.Join(dir, ".claude", "merge-check")
		}})
		if out.err != nil || !out.merged() {
			t.Fatalf("err = %v, merged %v", out.err, out.merged())
		}
		calls := proctest.Calls(t, check)
		if len(calls) != 1 || !slices.Equal(calls[0].Args, []string{"7", head}) {
			t.Errorf("merge-check calls = %+v", calls)
		}
		if !strings.Contains(out.stderr, "bench ok") || strings.Contains(out.stdout, "bench") {
			t.Errorf(
				"merge-check stdout should go to stderr: stdout %q stderr %q",
				out.stdout,
				out.stderr,
			)
		}
	})
	t.Run("non-zero", func(t *testing.T) {
		t.Parallel()
		out := run(t, okResponses(), setup{before: install(3)})
		assertRefused(t, out, "refused (exit 3)")
	})
	t.Run("not runnable", func(t *testing.T) {
		t.Parallel()
		out := run(t, okResponses(), setup{before: func(t *testing.T, dir string) {
			t.Helper()
			name := "merge-check"
			if runtime.GOOS == "windows" {
				name += ".cmd"
			}
			p := filepath.Join(mkdir(t, filepath.Join(dir, ".claude")), name)
			if err := os.WriteFile(p, []byte("exit 0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}})
		assertRefused(t, out, "merge-check cannot run")
	})
	t.Run("directory", func(t *testing.T) {
		t.Parallel()
		out := run(t, okResponses(), setup{before: func(t *testing.T, dir string) {
			t.Helper()
			mkdir(t, filepath.Join(dir, ".claude", "merge-check"))
			if runtime.GOOS == "windows" {
				mkdir(t, filepath.Join(dir, ".claude", "merge-check.exe"))
			}
		}})
		assertRefused(t, out, "merge-check cannot run")
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		out := run(t, okResponses(), setup{
			opts: func(o *Options) { o.MergeCheckTimeout = 200 * time.Millisecond },
			before: func(t *testing.T, dir string) {
				t.Helper()
				proctest.InstallAt(t, mkdir(t, filepath.Join(dir, ".claude")), "merge-check",
					proctest.Scenario{Responses: []proctest.Response{{Sleep: 10 * time.Second}}})
			},
		})
		assertRefused(t, out, "timed out")
	})
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

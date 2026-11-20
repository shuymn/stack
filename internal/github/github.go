// Package github wraps the `gh` and `git` calls of `stack pr merge` in typed functions.
//
// Every call runs through a proc.Runner with a deadline. A call that times out, exits non-zero,
// overruns the pipe wait, truncates its stdout or prints JSON that does not decode is an error; the
// caller refuses on it. Only Merge reports a non-zero exit as a result instead, because its outcome
// is read back afterwards whatever the exit status.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shuymn/stack/internal/proc"
)

// DefaultTimeout bounds each gh or git call except Merge.
const DefaultTimeout = 2 * time.Minute

// DefaultMergeTimeout bounds `gh pr merge`.
const DefaultMergeTimeout = 5 * time.Minute

// whatWords is how many leading arguments name a gh call in errors, e.g. "pr view".
const whatWords = 2

// maxCheckPages bounds the check pagination (100 per page), so a cursor that never ends cannot loop.
const maxCheckPages = 100

// ChecksQuery reads one page of the check runs and commit statuses of a commit given by OID.
const ChecksQuery = `query($owner:String!,$name:String!,$oid:GitObjectID!,$c:String){` +
	`repository(owner:$owner,name:$name){object(oid:$oid){... on Commit{oid ` +
	`statusCheckRollup{contexts(first:100,after:$c){totalCount pageInfo{hasNextPage endCursor} ` +
	`nodes{__typename ... on CheckRun{name status conclusion} ` +
	`... on StatusContext{context state}}}}}}}}`

// MergeQueueQuery reads whether a PR's base branch merges through a merge queue.
const MergeQueueQuery = `query($owner:String!,$name:String!,$n:Int!){` +
	`repository(owner:$owner,name:$name){pullRequest(number:$n){isMergeQueueEnabled}}}`

var (
	// ErrGh reports a gh call that failed or whose output cannot be used.
	ErrGh = errors.New("gh failed")
	// ErrGit reports a git call that failed.
	ErrGit = errors.New("git failed")

	fullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// IsFullSHA reports whether s is a full lower-case SHA-1 or SHA-256 object name.
func IsFullSHA(s string) bool {
	return fullSHA.MatchString(s)
}

// Client runs gh and git in one directory with one environment.
type Client struct {
	Runner proc.Runner
	// Gh and Git are the executables; empty means "gh" and "git" from PATH.
	Gh  string
	Git string
	// Dir is the working directory of every call; empty means the current one.
	Dir string
	// Env replaces the inherited environment when non-nil.
	Env []string
	// Timeout bounds each call except Merge; zero means DefaultTimeout.
	Timeout time.Duration
	// MergeTimeout bounds Merge; zero means DefaultMergeTimeout.
	MergeTimeout time.Duration
}

// PR is the state of a pull request that the merge depends on.
type PR struct {
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	State       string `json:"state"`
	IsDraft     bool   `json:"isDraft"`
	Mergeable   string `json:"mergeable"`
	// AutoMerge reports an active auto-merge request; only PRViewAuto reads it.
	AutoMerge bool `json:"-"`
}

// Repo is the repository gh resolves from the working directory.
type Repo struct {
	Owner, Name        string
	MergeCommitAllowed bool
	SquashMergeAllowed bool
	RebaseMergeAllowed bool
}

// Context is one check run or commit status of a commit.
type Context struct {
	// Kind is "CheckRun" or "StatusContext".
	Kind string
	// Name is the check run name or the status context.
	Name string
	// Status is a check run's status; empty for a commit status.
	Status string
	// Conclusion is a check run's conclusion or a commit status's state.
	Conclusion string
}

// Checks is every check of one commit.
type Checks struct {
	// OID is the commit the response is about.
	OID string
	// Contexts lists every check run and commit status, all pages read.
	Contexts []Context
}

// Readback is a PR's state after a merge attempt.
type Readback struct {
	State      string
	HeadRefOid string
	// MergeOID is the merge commit, or empty.
	MergeOID string
	// AutoMerge reports an active auto-merge request; only AutoReadBack reads it.
	AutoMerge bool
}

func (r Readback) String() string {
	return strings.TrimSpace(fmt.Sprintf("%s %s %s", r.State, r.HeadRefOid, r.MergeOID))
}

// MergeArgs are the optional arguments of `gh pr merge`.
type MergeArgs struct {
	// Auto asks gh to merge now if GitHub allows it, otherwise to enable auto-merge.
	Auto         bool
	DeleteBranch bool
	// Subject and Body are passed when non-nil, also when empty.
	Subject *string
	Body    *string
}

// MergeResult is how `gh pr merge` ended. Err is set when it could not start or was canceled.
type MergeResult struct {
	ExitCode int
	TimedOut bool
	Err      error
}

// OK reports a zero exit within the deadline.
func (r MergeResult) OK() bool {
	return r.Err == nil && !r.TimedOut && r.ExitCode == 0
}

func (r MergeResult) String() string {
	switch {
	case r.Err != nil:
		return r.Err.Error()
	case r.TimedOut:
		return "timed out"
	default:
		return fmt.Sprintf("exited %d", r.ExitCode)
	}
}

// Toplevel returns the work tree containing Dir, or "" when Dir is not in a work tree (git exits
// non-zero). A git that cannot run or times out is an error.
func (c Client) Toplevel(ctx context.Context) (string, error) {
	res, err := c.run(ctx, c.timeout(),
		proc.Cmd{Name: c.git(), Args: []string{"rev-parse", "--show-toplevel"}})
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: git rev-parse: %w", ErrGit, err)
	case res.TimedOut:
		return "", fmt.Errorf("%w: git rev-parse: %w", ErrGit, proc.ErrTimedOut)
	case res.ExitCode != 0:
		return "", nil
	case res.WaitDelayExceeded || res.Truncated:
		return "", fmt.Errorf("%w: git rev-parse: incomplete output", ErrGit)
	}
	// Only git's line end: a directory name may itself end in a space.
	return strings.TrimSuffix(strings.TrimSuffix(string(res.Stdout), "\n"), "\r"), nil
}

// PRView reads the PR's head, base, state, draft flag and mergeability.
func (c Client) PRView(ctx context.Context, pr int) (PR, error) {
	var v PR
	err := c.ghJSON(ctx, &v, "pr", "view", strconv.Itoa(pr),
		"--json", "headRefOid,baseRefName,state,isDraft,mergeable")
	return v, err
}

// PRViewAuto is PRView that also reads whether an auto-merge request is active.
func (c Client) PRViewAuto(ctx context.Context, pr int) (PR, error) {
	var v struct {
		PR

		AutoMergeRequest json.RawMessage `json:"autoMergeRequest"`
	}
	err := c.ghJSON(ctx, &v, "pr", "view", strconv.Itoa(pr),
		"--json", "headRefOid,baseRefName,state,isDraft,mergeable,autoMergeRequest")
	if err != nil {
		return PR{}, err
	}
	if v.AutoMerge, err = autoMergeActive(v.AutoMergeRequest); err != nil {
		return PR{}, err
	}
	return v.PR, nil
}

// autoMergeActive decodes the autoMergeRequest field: null means none, an object an active
// request, and a missing field or any other value cannot be trusted.
func autoMergeActive(raw json.RawMessage) (bool, error) {
	switch {
	case len(raw) == 0:
		return false, fmt.Errorf("%w: pr view: no autoMergeRequest field", ErrGh)
	case string(raw) == "null":
		return false, nil
	case raw[0] == '{':
		return true, nil
	}
	return false, fmt.Errorf("%w: pr view: unexpected autoMergeRequest %s", ErrGh, raw)
}

// RepoView reads the repository's name and allowed merge methods.
func (c Client) RepoView(ctx context.Context) (Repo, error) {
	var v struct {
		NameWithOwner      string `json:"nameWithOwner"`
		MergeCommitAllowed bool   `json:"mergeCommitAllowed"`
		SquashMergeAllowed bool   `json:"squashMergeAllowed"`
		RebaseMergeAllowed bool   `json:"rebaseMergeAllowed"`
	}
	err := c.ghJSON(ctx, &v, "repo", "view",
		"--json", "nameWithOwner,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed")
	if err != nil {
		return Repo{}, err
	}
	owner, name, ok := strings.Cut(v.NameWithOwner, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("%w: repo view: unexpected nameWithOwner %q", ErrGh,
			v.NameWithOwner)
	}
	return Repo{
		Owner:              owner,
		Name:               name,
		MergeCommitAllowed: v.MergeCommitAllowed,
		SquashMergeAllowed: v.SquashMergeAllowed,
		RebaseMergeAllowed: v.RebaseMergeAllowed,
	}, nil
}

// CommitHeadlines returns the headline of every commit of the PR.
func (c Client) CommitHeadlines(ctx context.Context, pr int) ([]string, error) {
	var v struct {
		Commits *[]struct {
			MessageHeadline string `json:"messageHeadline"`
		} `json:"commits"`
	}
	if err := c.ghJSON(ctx, &v, "pr", "view", strconv.Itoa(pr), "--json", "commits"); err != nil {
		return nil, err
	}
	if v.Commits == nil {
		return nil, fmt.Errorf("%w: pr view: no commits field", ErrGh)
	}
	out := make([]string, 0, len(*v.Commits))
	for _, commit := range *v.Commits {
		out = append(out, commit.MessageHeadline)
	}
	return out, nil
}

// ChecksArgs is the argv of the checks query for one page; cursor is empty on the first page.
func ChecksArgs(repo Repo, oid, cursor string) []string {
	args := []string{
		"api", "graphql", "-f", "query=" + ChecksQuery,
		"-f", "owner=" + repo.Owner, "-f", "name=" + repo.Name, "-f", "oid=" + oid,
	}
	if cursor != "" {
		args = append(args, "-f", "c="+cursor)
	}
	return args
}

// CommitChecks reads every check run and commit status of the commit oid, paging to the end. A
// commit that is not found, a response whose pages disagree on the commit or on the total, or a
// node of an unknown kind is an error. A commit without a rollup has no contexts.
func (c Client) CommitChecks(ctx context.Context, repo Repo, oid string) (Checks, error) {
	type node struct {
		Typename   string `json:"__typename"` //nolint:tagliatelle // GraphQL meta field name.
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Context    string `json:"context"`
		State      string `json:"state"`
	}
	type page struct {
		Data struct {
			Repository *struct {
				Object *struct {
					OID    string `json:"oid"`
					Rollup *struct {
						Contexts *struct {
							TotalCount int `json:"totalCount"`
							PageInfo   struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
							Nodes []node `json:"nodes"`
						} `json:"contexts"`
					} `json:"statusCheckRollup"`
				} `json:"object"`
			} `json:"repository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}

	var out Checks
	cursor, total := "", -1
	for range maxCheckPages {
		var p page
		if err := c.ghJSON(ctx, &p, ChecksArgs(repo, oid, cursor)...); err != nil {
			return Checks{}, err
		}
		if len(p.Errors) > 0 {
			return Checks{}, fmt.Errorf("%w: checks query: %s", ErrGh, p.Errors[0])
		}
		if p.Data.Repository == nil || p.Data.Repository.Object == nil {
			return Checks{}, fmt.Errorf("%w: checks query: commit %s not found", ErrGh, oid)
		}
		obj := p.Data.Repository.Object
		if out.OID == "" {
			out.OID = obj.OID
		} else if obj.OID != out.OID {
			return Checks{}, fmt.Errorf("%w: checks query: pages name commits %s and %s",
				ErrGh, out.OID, obj.OID)
		}
		if obj.Rollup == nil {
			if cursor != "" {
				return Checks{}, fmt.Errorf("%w: checks query: rollup vanished while paging", ErrGh)
			}
			return out, nil
		}
		ctxs := obj.Rollup.Contexts
		if ctxs == nil {
			return Checks{}, fmt.Errorf("%w: checks query: no contexts", ErrGh)
		}
		if total >= 0 && ctxs.TotalCount != total {
			return Checks{}, fmt.Errorf(
				"%w: checks query: total changed from %d to %d while paging",
				ErrGh,
				total,
				ctxs.TotalCount,
			)
		}
		total = ctxs.TotalCount
		for _, n := range ctxs.Nodes {
			switch n.Typename {
			case "CheckRun":
				out.Contexts = append(out.Contexts, Context{
					Kind: n.Typename, Name: n.Name, Status: n.Status, Conclusion: n.Conclusion,
				})
			case "StatusContext":
				out.Contexts = append(out.Contexts, Context{
					Kind: n.Typename, Name: n.Context, Conclusion: n.State,
				})
			default:
				return Checks{}, fmt.Errorf("%w: checks query: unknown node %q", ErrGh, n.Typename)
			}
		}
		if !ctxs.PageInfo.HasNextPage {
			if len(out.Contexts) != total {
				return Checks{}, fmt.Errorf("%w: checks query: read %d of %d checks", ErrGh,
					len(out.Contexts), total)
			}
			return out, nil
		}
		if ctxs.PageInfo.EndCursor == "" || ctxs.PageInfo.EndCursor == cursor {
			return Checks{}, fmt.Errorf("%w: checks query: page cursor did not advance", ErrGh)
		}
		cursor = ctxs.PageInfo.EndCursor
	}
	return Checks{}, fmt.Errorf("%w: checks query: more than %d pages", ErrGh, maxCheckPages)
}

// MergeQueueArgs is the argv of the merge-queue query.
func MergeQueueArgs(repo Repo, pr int) []string {
	return []string{
		"api", "graphql", "-f", "query=" + MergeQueueQuery,
		"-f", "owner=" + repo.Owner, "-f", "name=" + repo.Name, "-F", "n=" + strconv.Itoa(pr),
	}
}

// MergeQueueEnabled reads whether the PR's base merges through a merge queue.
func (c Client) MergeQueueEnabled(ctx context.Context, repo Repo, pr int) (bool, error) {
	var v struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					IsMergeQueueEnabled *bool `json:"isMergeQueueEnabled"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := c.ghJSON(ctx, &v, MergeQueueArgs(repo, pr)...); err != nil {
		return false, err
	}
	if len(v.Errors) > 0 {
		return false, fmt.Errorf("%w: merge queue query: %s", ErrGh, v.Errors[0])
	}
	if v.Data.Repository == nil || v.Data.Repository.PullRequest == nil ||
		v.Data.Repository.PullRequest.IsMergeQueueEnabled == nil {
		return false, fmt.Errorf("%w: merge queue query: no isMergeQueueEnabled", ErrGh)
	}
	return *v.Data.Repository.PullRequest.IsMergeQueueEnabled, nil
}

// MergeArgv is the argv of `gh pr merge` on the checked head. Values use the `--flag=value` form,
// so a subject or body starting with `-` stays a value.
func MergeArgv(pr int, method, head string, a MergeArgs) []string {
	args := []string{"pr", "merge", strconv.Itoa(pr), "--" + method}
	if a.Auto {
		args = append(args, "--auto")
	}
	args = append(args, "--match-head-commit", head)
	if a.DeleteBranch {
		args = append(args, "--delete-branch")
	}
	if a.Subject != nil {
		args = append(args, "--subject="+*a.Subject)
	}
	if a.Body != nil {
		args = append(args, "--body="+*a.Body)
	}
	return args
}

// Merge runs `gh pr merge` with MergeArgv and forwards its stdout and stderr to log. Its outcome is
// a result, not an error: the caller reads the PR back whatever happened.
func (c Client) Merge(
	ctx context.Context,
	pr int,
	method, head string,
	a MergeArgs,
	log io.Writer,
) MergeResult {
	timeout := c.MergeTimeout
	if timeout <= 0 {
		timeout = DefaultMergeTimeout
	}
	res, err := c.run(ctx, timeout, proc.Cmd{
		Name: c.gh(), Args: MergeArgv(pr, method, head, a), Stderr: log,
	})
	if len(res.Stdout) > 0 {
		_, _ = log.Write(res.Stdout)
	}
	return MergeResult{ExitCode: res.ExitCode, TimedOut: res.TimedOut, Err: err}
}

// DisableAutoArgv is the argv of `gh pr merge --disable-auto`.
func DisableAutoArgv(pr int) []string {
	return []string{"pr", "merge", strconv.Itoa(pr), "--disable-auto"}
}

// DisableAuto runs `gh pr merge --disable-auto` and forwards its output to log. Like Merge, its
// outcome is a result: the caller reads the PR back whatever happened.
func (c Client) DisableAuto(ctx context.Context, pr int, log io.Writer) MergeResult {
	res, err := c.run(ctx, c.timeout(), proc.Cmd{
		Name: c.gh(), Args: DisableAutoArgv(pr), Stderr: log,
	})
	if len(res.Stdout) > 0 {
		_, _ = log.Write(res.Stdout)
	}
	return MergeResult{ExitCode: res.ExitCode, TimedOut: res.TimedOut, Err: err}
}

// readback is the JSON of a read-back.
type readback struct {
	State       string `json:"state"`
	HeadRefOid  string `json:"headRefOid"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	AutoMergeRequest json.RawMessage `json:"autoMergeRequest"`
}

func (v readback) result() Readback {
	r := Readback{State: v.State, HeadRefOid: v.HeadRefOid}
	if v.MergeCommit != nil {
		r.MergeOID = v.MergeCommit.OID
	}
	return r
}

// ReadBack reads the PR's state, head and merge commit.
func (c Client) ReadBack(ctx context.Context, pr int) (Readback, error) {
	var v readback
	err := c.ghJSON(ctx, &v, "pr", "view", strconv.Itoa(pr),
		"--json", "state,headRefOid,mergeCommit")
	if err != nil {
		return Readback{}, err
	}
	return v.result(), nil
}

// AutoReadBack is ReadBack that also reads whether an auto-merge request is active.
func (c Client) AutoReadBack(ctx context.Context, pr int) (Readback, error) {
	var v readback
	err := c.ghJSON(ctx, &v, "pr", "view", strconv.Itoa(pr),
		"--json", "state,headRefOid,mergeCommit,autoMergeRequest")
	if err != nil {
		return Readback{}, err
	}
	r := v.result()
	if r.AutoMerge, err = autoMergeActive(v.AutoMergeRequest); err != nil {
		return Readback{}, err
	}
	return r, nil
}

// ghJSON runs gh with args and decodes its stdout into v.
func (c Client) ghJSON(ctx context.Context, v any, args ...string) error {
	res, err := c.run(ctx, c.timeout(), proc.Cmd{Name: c.gh(), Args: args})
	what := strings.Join(args[:min(whatWords, len(args))], " ")
	switch {
	case err != nil:
		return fmt.Errorf("%w: %s: %w", ErrGh, what, err)
	case res.TimedOut:
		return fmt.Errorf("%w: %s: %w", ErrGh, what, proc.ErrTimedOut)
	case res.ExitCode != 0:
		return fmt.Errorf("%w: %s: exit %d: %s", ErrGh, what, res.ExitCode,
			oneLine(res.Stderr))
	case res.WaitDelayExceeded:
		return fmt.Errorf("%w: %s: %w", ErrGh, what, proc.ErrWaitDelay)
	case res.Truncated:
		return fmt.Errorf("%w: %s: output over the size cap", ErrGh, what)
	}
	dec := json.NewDecoder(bytes.NewReader(res.Stdout))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s: decode: %w", ErrGh, what, err)
	}
	// Only JSON whitespace may follow the value: a second Decode must reach the end. Decoder.More
	// would accept a stray `]` or `}`, bytes.TrimSpace a vertical tab.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %s: trailing output after JSON", ErrGh, what)
	}
	return nil
}

// run runs cmd in c.Dir with c.Env under timeout.
func (c Client) run(ctx context.Context, timeout time.Duration, cmd proc.Cmd) (proc.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd.Dir, cmd.Env = c.Dir, c.Env
	res, err := c.Runner.Run(ctx, cmd)
	if err != nil {
		return res, fmt.Errorf("run %s: %w", cmd.Name, err)
	}
	return res, nil
}

func (c Client) gh() string {
	if c.Gh == "" {
		return "gh"
	}
	return c.Gh
}

func (c Client) git() string {
	if c.Git == "" {
		return "git"
	}
	return c.Git
}

func (c Client) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

// oneLine collapses the whitespace of b, line ends included, so a reason stays on one line.
func oneLine(b []byte) string {
	return strings.Join(strings.Fields(string(b)), " ")
}

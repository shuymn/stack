// Package merge implements `stack pr merge`: it merges a pull request only on the head SHA that was
// checked, after the PR state, merge method, fixup commits, the repository's `.claude/merge-check`,
// every check of that SHA and the merge-queue setting were verified, and then reads the result
// back.
//
// Each step refuses on anything it cannot verify; nothing after a refusal runs, so `gh pr merge` is
// invoked only when every step passed. GitHub itself rejects the merge when the head moved after the
// checks (`--match-head-commit`).
package merge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shuymn/stack/internal/checkpub"
	"github.com/shuymn/stack/internal/github"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/publictext"
)

const (
	// viewAttempts is how often the PR is read while GitHub reports its mergeability as UNKNOWN.
	viewAttempts = 5
	// defaultRetryDelay is the wait between those reads.
	defaultRetryDelay = 3 * time.Second
	// DefaultMergeCheckTimeout bounds `.claude/merge-check`, which may run benchmarks.
	DefaultMergeCheckTimeout = 30 * time.Minute
	// readBackTimeout bounds the read-back, which runs even after stack was told to stop.
	readBackTimeout = 30 * time.Second
)

var (
	// ErrInvalid reports an argument that fails validation (exit 2).
	ErrInvalid = errors.New("invalid argument")
	// ErrRefused reports a step that refused the merge or could not be verified (exit 1).
	ErrRefused = errors.New("refused")
	// ErrUnknown, with ErrRefused, reports that the merge call ran but its outcome is not confirmed.
	ErrUnknown = errors.New("result unknown")

	prRe   = regexp.MustCompile(`^[0-9]+$`)
	headRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	// fixupRe matches the headlines that `git rebase --autosquash` folds.
	fixupRe = regexp.MustCompile(`^(?:fixup|amend|squash)! `)
)

// Methods are the merge methods, as flag names.
var Methods = []string{"merge", "squash", "rebase"}

// Options is one `stack pr merge` invocation.
type Options struct {
	// PR is the PR number as given; Head the expected head SHA or a 7+ hex prefix of it.
	PR   string
	Head string
	// Methods are the method flags given; at most one is allowed, none means the repository's only one.
	Methods []string
	// Merge holds --delete-branch, --subject and --body.
	Merge         github.MergeArgs
	AllowNoChecks bool
	// GHRepo is the value of GH_REPO, which must be empty.
	GHRepo string
	// Terms are the private terms that --subject and --body must not contain.
	Terms []string

	Client github.Client
	// RetryDelay is the wait between PR reads while mergeability is UNKNOWN; zero means 3 s.
	RetryDelay time.Duration
	// MergeCheckTimeout bounds `.claude/merge-check`; zero means DefaultMergeCheckTimeout.
	MergeCheckTimeout time.Duration

	// Stdout receives the `MERGED <head> <merge-oid>` line; Stderr notes and the output of
	// merge-check and gh pr merge.
	Stdout io.Writer
	Stderr io.Writer
}

// request is a validated invocation.
type request struct {
	pr     int
	want   string
	method string
}

// Validate checks the arguments (step 1) without running anything.
func Validate(o Options) error {
	_, err := validate(o)
	return err
}

func validate(o Options) (request, error) {
	var r request
	if !prRe.MatchString(o.PR) {
		return r, fmt.Errorf("%w: pass the PR number, not %q", ErrInvalid, o.PR)
	}
	n, err := strconv.Atoi(o.PR)
	if err != nil {
		return r, fmt.Errorf("%w: PR number %q: %w", ErrInvalid, o.PR, err)
	}
	if !headRe.MatchString(o.Head) {
		return r, fmt.Errorf("%w: pass the head SHA (7-40 lower-case hex digits), not %q",
			ErrInvalid, o.Head)
	}
	if o.GHRepo != "" {
		return r, fmt.Errorf("%w: run inside the PR's repository instead of setting GH_REPO",
			ErrInvalid)
	}
	if len(o.Methods) > 1 {
		return r, fmt.Errorf("%w: pass one merge method, not --%s", ErrInvalid,
			strings.Join(o.Methods, " --"))
	}
	r = request{pr: n, want: o.Head}
	if len(o.Methods) == 1 {
		r.method = o.Methods[0]
	}
	var hits []string
	for _, f := range []struct {
		flag string
		text *string
	}{{"--subject", o.Merge.Subject}, {"--body", o.Merge.Body}} {
		if f.text == nil {
			continue
		}
		for _, h := range publictext.LocalPaths(*f.text) {
			hits = append(hits, fmt.Sprintf("%s: local path '%s'", f.flag, h))
		}
		for _, t := range checkpub.FindTerms(o.Terms, *f.text) {
			hits = append(hits, fmt.Sprintf("%s: private term '%s'", f.flag, t))
		}
	}
	if len(hits) > 0 {
		return r, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(hits, "; "))
	}
	return r, nil
}

// Run validates o and runs every step; it writes `MERGED <head> <merge-oid>` to Stdout only when the
// read-back shows the PR merged on the checked head. Validation errors wrap ErrInvalid, every later
// refusal ErrRefused.
func Run(ctx context.Context, o Options) error {
	r, err := validate(o)
	if err != nil {
		return err
	}
	c := o.Client

	// The repository's merge-check lives in the checkout, so running inside one is a precondition.
	root, err := c.Toplevel(ctx)
	if err != nil {
		return refuse("could not find the repository: %v", err)
	}
	if root == "" {
		return refuse("not inside a checkout of the PR's repository: run stack pr merge there")
	}
	c.Dir = filepath.FromSlash(root)

	head, err := checkPR(ctx, c, r, o.retryDelay())
	if err != nil {
		return err
	}

	repo, err := c.RepoView(ctx)
	if err != nil {
		return refuse("could not read the repository: %v", err)
	}
	if r.method == "" {
		if r.method, err = onlyMethod(repo); err != nil {
			return err
		}
	}

	if r.method != "squash" {
		if err := checkFixups(ctx, c, r.pr); err != nil {
			return err
		}
	}

	if err := mergeCheck(ctx, o, c, r.pr, head); err != nil {
		return err
	}

	queue, err := c.MergeQueueEnabled(ctx, repo, r.pr)
	if err != nil {
		return refuse("could not read whether the base uses a merge queue: %v", err)
	}
	if queue {
		return refuse("the base merges through a merge queue, which stack pr merge does not handle")
	}

	// The checks are read last, right before the merge, so a rerun during the steps above is seen.
	if err := checkChecks(ctx, c, repo, head, o.AllowNoChecks); err != nil {
		return err
	}

	res := c.Merge(ctx, r.pr, r.method, head, o.Merge, o.Stderr)
	// The merge may have happened even when stack is being stopped: read back on a short deadline
	// of its own.
	backCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBackTimeout)
	defer cancel()
	back, err := c.ReadBack(backCtx, r.pr)
	if err != nil {
		return unknown(r.pr, "gh pr merge %s; the read-back failed: %v", res, err)
	}
	switch {
	case back.State == "OPEN":
		return refuse("PR %d is not merged on %s (%s); gh pr merge %s", r.pr, head, back, res)
	case back.State != "MERGED" || back.HeadRefOid != head || !github.IsFullSHA(back.MergeOID):
		return unknown(r.pr, "read back %s, not MERGED on %s; gh pr merge %s", back, head, res)
	}
	if _, err := fmt.Fprintf(o.Stdout, "MERGED %s %s\n", head, back.MergeOID); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	if !res.OK() {
		_, _ = fmt.Fprintf(o.Stderr, "stack pr merge: PR %d is merged, but gh pr merge %s "+
			"(a cleanup step such as --delete-branch can fail after the merge); do not retry\n",
			r.pr, res)
	}
	return nil
}

func (o Options) retryDelay() time.Duration {
	if o.RetryDelay <= 0 {
		return defaultRetryDelay
	}
	return o.RetryDelay
}

// unknown reports a merge attempt whose outcome the read-back cannot confirm: a refusal that tells
// the caller to look at the PR instead of retrying.
func unknown(pr int, format string, args ...any) error {
	return fmt.Errorf("%w: %w for PR %d — check the PR before retrying: %s", ErrRefused,
		ErrUnknown, pr, fmt.Sprintf(format, args...))
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// checkPR reads the PR, retrying while its mergeability is UNKNOWN, and returns the full head SHA
// once the head matches, the PR is open, ready and mergeable (step 3).
func checkPR(ctx context.Context, c github.Client, r request, delay time.Duration) (string, error) {
	var pr github.PR
	for attempt := 1; ; attempt++ {
		var err error
		pr, err = c.PRView(ctx, r.pr)
		if err != nil {
			return "", refuse("could not read PR %d: %v", r.pr, err)
		}
		if pr.Mergeable != "UNKNOWN" || attempt == viewAttempts {
			break
		}
		if err := sleep(ctx, delay); err != nil {
			return "", refuse("waiting for the mergeability of PR %d: %v", r.pr, err)
		}
	}
	switch {
	case !github.IsFullSHA(pr.HeadRefOid):
		return "", refuse("PR %d has no usable head SHA (%q)", r.pr, pr.HeadRefOid)
	case !strings.HasPrefix(pr.HeadRefOid, r.want):
		return "", refuse("head is %s, not %s: check the new head first", pr.HeadRefOid, r.want)
	case pr.State != "OPEN":
		return "", refuse("PR %d is not open (%s)", r.pr, pr.State)
	case pr.IsDraft:
		return "", refuse("PR %d is a draft", r.pr)
	case pr.Mergeable != "MERGEABLE":
		return "", refuse("PR %d is not mergeable (%s; UNKNOWN means GitHub has not decided yet, "+
			"retry)", r.pr, pr.Mergeable)
	}
	return pr.HeadRefOid, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// onlyMethod returns the single merge method the repository allows (step 4).
func onlyMethod(repo github.Repo) (string, error) {
	var allowed []string
	for _, m := range []struct {
		name string
		ok   bool
	}{
		{"merge", repo.MergeCommitAllowed},
		{"squash", repo.SquashMergeAllowed},
		{"rebase", repo.RebaseMergeAllowed},
	} {
		if m.ok {
			allowed = append(allowed, m.name)
		}
	}
	if len(allowed) != 1 {
		return "", refuse("the repository allows %d merge methods (%s): pass --merge, --squash or "+
			"--rebase", len(allowed), strings.Join(allowed, ", "))
	}
	return allowed[0], nil
}

// checkFixups refuses when a commit of the PR is left for `git rebase --autosquash` (step 5).
func checkFixups(ctx context.Context, c github.Client, pr int) error {
	headlines, err := c.CommitHeadlines(ctx, pr)
	if err != nil {
		return refuse("could not read the commits of PR %d: %v", pr, err)
	}
	var marked []string
	for _, h := range headlines {
		if fixupRe.MatchString(h) {
			marked = append(marked, strconv.Quote(h))
		}
	}
	if len(marked) > 0 {
		return refuse("fold these commits first (git rebase --autosquash), then check the new "+
			"head: %s", strings.Join(marked, ", "))
	}
	return nil
}

// mergeCheck runs `<root>/.claude/merge-check <pr> <head>` when it exists (step 6). A file that is
// present but not runnable on this OS, or that cannot be verified, refuses: a placed check is
// required.
func mergeCheck(ctx context.Context, o Options, c github.Client, pr int, head string) error {
	path, err := proc.ResolveExecutable(filepath.Join(c.Dir, ".claude"), "merge-check")
	switch {
	case errors.Is(err, proc.ErrNotFound):
		return nil
	case err != nil:
		return refuse("the repository's merge-check cannot run: %v", err)
	}
	timeout := o.MergeCheckTimeout
	if timeout <= 0 {
		timeout = DefaultMergeCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := c.Runner.Run(ctx, proc.Cmd{
		Name: path, Args: []string{strconv.Itoa(pr), head}, Dir: c.Dir, Env: c.Env,
		Stderr: o.Stderr,
	})
	if len(res.Stdout) > 0 {
		_, _ = o.Stderr.Write(res.Stdout)
	}
	switch {
	case err != nil:
		return refuse("%s did not run: %v", path, err)
	case res.TimedOut:
		return refuse("%s timed out after %s", path, timeout)
	case res.ExitCode != 0:
		return refuse("%s refused (exit %d)", path, res.ExitCode)
	case res.WaitDelayExceeded:
		return refuse("%s left its output open after exiting", path)
	case res.StderrErr != nil:
		return refuse("%s: forwarding its stderr failed: %v", path, res.StderrErr)
	}
	return nil
}

// checkChecks refuses unless every check run and commit status of head passed, or there are none
// and allowNoChecks is set (step 7).
func checkChecks(
	ctx context.Context,
	c github.Client,
	repo github.Repo,
	head string,
	allowNoChecks bool,
) error {
	checks, err := c.CommitChecks(ctx, repo, head)
	if err != nil {
		return refuse("could not read the checks of %s: %v", head, err)
	}
	if checks.OID != head {
		return refuse("the checks query answered for commit %q, not %s", checks.OID, head)
	}
	if len(checks.Contexts) == 0 {
		if allowNoChecks {
			return nil
		}
		return refuse("no checks ran on %s yet; wait for CI, or pass --allow-no-checks for a "+
			"repository without CI", head)
	}
	var bad []string
	for _, chk := range checks.Contexts {
		if !passed(chk) {
			state := chk.Conclusion
			if chk.Status != "" {
				state = chk.Status + "/" + chk.Conclusion
			}
			bad = append(bad, fmt.Sprintf("%s (%s)", chk.Name, strings.TrimSuffix(state, "/")))
		}
	}
	if len(bad) > 0 {
		return refuse("not every check on %s passed (pending: wait and rerun): %s", head,
			strings.Join(bad, ", "))
	}
	return nil
}

func passed(c github.Context) bool {
	if c.Kind == "StatusContext" {
		return c.Conclusion == "SUCCESS"
	}
	if c.Status != "COMPLETED" {
		return false
	}
	switch c.Conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return true
	}
	return false
}

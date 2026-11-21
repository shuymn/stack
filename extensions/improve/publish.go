package improve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// createShare: gh pr create gets at most 1/createShare of the step it shares with the PR read.
const createShare = 2

// verdictSchema is the Codex verdict schema (legacy improve.py VERDICT).
const verdictSchema = `{"type":"object","additionalProperties":false,"required":["high"],` +
	`"properties":{"high":{"type":"array","items":{"type":"string"}}}}`

// review is the legacy Codex review prompt (improve.py:93-109).
const review = `Review this proposed change to an agent's instructions before it is published as a pull request. Report as HIGH only
problems that should block it: judgment the instructions lose, a guard that is loosened, case-specific nouns that read
as a general rule, or a relative link that no longer resolves. Return {"high": []} when there are none.

## Evidence brief
%s
## Commit message
%s
## PR title
%s
## PR body
%s
## Diff
%s`

// writeTemp creates a new file of the run's temp directory. An existing path, a symlink a hook
// planted included, is refused rather than written through.
func (a *attempt) writeTemp(name, content string) (string, *stop) {
	p := filepath.Join(a.tmp, name)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", halt("failed:check", "cannot create %s", name)
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", halt("failed:check", "cannot write %s", name)
	}
	return p, nil
}

// publish is step 12.
func (a *attempt) publish() *stop {
	if a.st.Outcome == outNoChange {
		return nil
	}
	s := a.s
	branch := "improve/" + s.week
	ref := "refs/heads/" + branch
	// 12.1 Commit.
	msgFile, err := a.writeTemp("message", a.message+"\n")
	if err != nil {
		return err
	}
	if out := s.git(a.worktree, "commit", "-a", "-F", msgFile); !out.ok() {
		return halt("failed:commit", "git commit: %s", out.describe())
	}
	// 12.2 Pin.
	out := s.git(a.worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if !out.ok() {
		return halt("failed:commit", "cannot resolve the commit: %s", out.describe())
	}
	a.oid = strings.TrimSpace(string(out.stdout))
	a.st.OID = a.oid
	// The title and body files are written once, after every commit hook has run, and never
	// rebuilt.
	if a.titleFile, err = a.writeTemp("title", a.title); err != nil {
		return err
	}
	if a.bodyFile, err = a.writeTemp("body", a.body); err != nil {
		return err
	}
	// 12.3 Verify.
	if err := a.verify(); err != nil {
		return err
	}
	// 12.4 Credential check of the published bytes.
	if err := a.credentialCheck(); err != nil {
		return err
	}
	// 12.5 Public check.
	if err := a.publicCheck(branch); err != nil {
		return err
	}
	if err := a.readChecked(); err != nil {
		return err
	}
	a.hook("check")
	// 12.6 Codex check.
	if err := a.codex(); err != nil {
		return err
	}
	a.hook("codex")
	// 12.7 Record.
	a.st.Branch, a.st.Title, a.st.Body = branch, a.title, a.body
	if err := a.save(); err != nil {
		return halt("failed:check", "cannot record the candidate in the stamp")
	}
	// 12.8 Push.
	if s.left()-s.o.limits.reserve < s.o.limits.pushPR {
		return halt("failed:deadline", "not enough time left to push and open the PR")
	}
	pushed, line := a.push(ref)
	a.hook("pushed")
	if pushed == pushRejected {
		return halt("failed:push", "%s", line)
	}
	if pushed == pushAmbiguous {
		return a.ambiguous(branch, nil, "push result unclear: "+line)
	}
	// 12.9 PR: create and the read share one step, and create gets at most half of it, so the
	// read always has time. The body goes on stdin from the checked bytes; no file is read again.
	slotEnd := s.o.clock().Add(min(s.o.limits.step, s.left()-s.o.limits.reserve))
	create := s.child("gh", command{
		args: []string{
			"pr", "create", "--repo", ghHost + "/" + s.slug, "--base", s.branchBase,
			"--head", branch,
			"--title", a.title, "--body-file", "-",
		},
		env: s.ghEnv(), stdin: []byte(a.body), timeout: slotEnd.Sub(s.o.clock()) / createShare,
	})
	read := a.readPRs(branch, slotEnd.Sub(s.o.clock()))
	if read.err == nil {
		if p, ok := a.unique(read.prs, branch); ok {
			a.setPR(p)
			return nil
		}
	}
	note := "gh pr create " + create.describe()
	if create.ok() {
		note = "gh pr create succeeded"
	}
	return a.ambiguous(branch, &read, note)
}

// verify is step 12.3: one commit on base_oid changing only allowed existing 100644 files.
func (a *attempt) verify() *stop {
	s := a.s
	out := s.git(a.worktree, "rev-list", "--parents", "-n", "1", a.oid)
	if !out.ok() {
		return halt("failed:check", "git rev-list: %s", out.describe())
	}
	if strings.TrimSpace(string(out.stdout)) != a.oid+" "+s.baseOID {
		return halt("proposal:commit-scope", "the commit is not one commit on the base")
	}
	out = s.git(a.worktree, "diff-tree", "-r", "-z", "--no-renames", "--raw", s.baseOID, a.oid)
	if !out.ok() {
		return halt("failed:check", "git diff-tree: %s", out.describe())
	}
	toks := strings.Split(strings.TrimSuffix(string(out.stdout), "\x00"), "\x00")
	if len(toks) < 2 || len(toks)%2 != 0 {
		return halt("proposal:commit-scope", "the commit changes nothing readable")
	}
	for i := 0; i < len(toks); i += 2 {
		f := strings.Fields(strings.TrimPrefix(toks[i], ":"))
		p := toks[i+1]
		// Raw entry: old mode, new mode, old oid, new oid, status.
		if len(f) != 5 || f[0] != "100644" || f[1] != "100644" || f[4] != "M" {
			return halt("proposal:commit-scope", "not a 100644 modification: %s", p)
		}
		if !matchAny(s.paths, p) {
			return halt("proposal:commit-scope", "outside the allowed paths: %s", p)
		}
	}
	return nil
}

// credentialCheck is step 12.4: the diff, the commit message, the title and the body.
func (a *attempt) credentialCheck() *stop {
	s := a.s
	diff := s.git(a.worktree, "diff", "--text", "--no-ext-diff", "--no-textconv", "--no-color",
		s.baseOID, a.oid)
	msg := s.git(a.worktree, "show", "-s", "--encoding=UTF-8", "--format=%B", a.oid)
	if !diff.ok() || !msg.ok() {
		return halt("failed:check", "cannot read the commit for the credential check")
	}
	a.message = string(msg.stdout)
	if slices.ContainsFunc([]string{string(diff.stdout), a.message, a.title, a.body}, s.leaks) {
		return halt("proposal:credential", "the commit, title or body contains an endpoint "+
			"credential (a commit hook may have added it)")
	}
	return nil
}

// publicCheck is step 12.5.
func (a *attempt) publicCheck(branch string) *stop {
	s := a.s
	out := s.child("stack", command{
		args: []string{
			"check", "public", s.baseOID + ".." + a.oid, "--title-file", a.titleFile,
			"--body-file", a.bodyFile, "--branch", branch,
		},
		dir: a.worktree, timeout: s.o.limits.step,
		env: append(slices.Clone(s.o.Env), noReplace),
	})
	switch {
	case out.ok():
		return nil
	case out.err == nil && !out.timedOut && !out.pipesCut && out.exit == 1:
		a.st.CheckHits = string(out.stderr)
		return halt(
			"proposal:public-check",
			"stack check public found text that must not be published",
		)
	default:
		return halt("failed:check", "stack check public: %s", out.describe())
	}
}

// readChecked reads the title and body files once, after the public check read them; Codex and
// gh get these bytes. Files that no longer hold what was written are refused.
func (a *attempt) readChecked() *stop {
	title, terr := os.ReadFile(a.titleFile)
	body, berr := os.ReadFile(a.bodyFile)
	if terr != nil || berr != nil {
		return halt("failed:check", "cannot read the checked title or body")
	}
	if string(title) != a.title || string(body) != a.body {
		return halt("proposal:public-check", "the title or body changed around the public check")
	}
	a.title, a.body = string(title), string(body)
	return nil
}

// codex is step 12.6: a read-only review of the pinned candidate; it can only stop.
func (a *attempt) codex() *stop {
	s := a.s
	avail := func() time.Duration { return s.left() - s.o.limits.reserve - s.o.limits.pushPR }
	if avail() < s.o.limits.codexMin {
		return halt("failed:deadline", "not enough time left for the Codex check")
	}
	diff := s.git(a.worktree, "diff", "--text", "--no-ext-diff", "--no-textconv", "--no-color",
		s.baseOID, a.oid)
	if !diff.ok() {
		return halt("failed:check", "cannot read the diff for the Codex check")
	}
	empty := filepath.Join(a.tmp, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		return halt("failed:check", "cannot create the Codex directory")
	}
	schema, err := a.writeTemp("verdict.schema.json", verdictSchema)
	if err != nil {
		return err
	}
	verdict := filepath.Join(a.tmp, "verdict.json")
	prompt := fmt.Sprintf(review, a.briefTxt, a.message, a.title, a.body, string(diff.stdout))
	a.hook("codex-input")
	// The timeout is what is left after preparing the input.
	timeout := avail()
	if timeout <= 0 {
		return halt("failed:deadline", "not enough time left for the Codex check")
	}
	s.logf("codex timeout %s", timeout.Round(time.Second))
	a.st.CodexRuns++
	start := time.Now()
	out := s.child("codex", command{
		args: []string{
			"exec", "--ephemeral", "--skip-git-repo-check", "-s", "read-only", "-c",
			"approval_policy=never", "-C", empty, "--output-schema", schema, "-o", verdict, "-",
		},
		dir: empty, stdin: []byte(prompt), timeout: timeout,
	})
	s.logf("codex wall time %s (%s)", time.Since(start).Round(time.Second), out.describe())
	if !out.ok() {
		return halt("proposal:codex", "codex %s", out.describe())
	}
	data, rerr := os.ReadFile(verdict)
	if rerr != nil {
		return halt("proposal:codex", "codex left no verdict")
	}
	high, ok := parseVerdict(data)
	if !ok {
		return halt("proposal:codex", "codex left an invalid verdict")
	}
	if len(high) > 0 {
		a.st.CodexHigh = high
		return halt("proposal:codex-high", "%s", strings.Join(high, "; "))
	}
	return nil
}

// parseVerdict accepts exactly {"high": [strings]}.
func parseVerdict(data []byte) ([]string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || len(m) != 1 {
		return nil, false
	}
	raw, ok := m["high"]
	if !ok {
		return nil, false
	}
	var high []string
	if err := json.Unmarshal(raw, &high); err != nil || high == nil {
		return nil, false
	}
	return high, true
}

type pushResult int

const (
	pushOK pushResult = iota
	pushRejected
	pushAmbiguous
)

// push is step 12.8: only the pinned OID, to a new branch, confirmed by the porcelain line. It
// runs in the worktree, so a pre-push hook never runs in the owner's checkout, and leaves a full
// step for the PR.
func (a *attempt) push(ref string) (pushResult, string) {
	s := a.s
	c := s.gitCmd(a.worktree, "push", "--porcelain", "--no-follow-tags",
		"--recurse-submodules=no", "origin", "--force-with-lease="+ref+":", a.oid+":"+ref)
	c.timeout = min(c.timeout, s.left()-s.o.limits.reserve-s.o.limits.step)
	out := s.child("git", c)
	line := ""
	for l := range strings.SplitSeq(string(out.stdout), "\n") {
		f := strings.Split(l, "\t")
		if len(f) >= 2 && strings.HasSuffix(f[1], ":"+ref) {
			line = strings.TrimSpace(l)
			break
		}
	}
	switch {
	case line == "":
		return pushAmbiguous, "push " + out.describe() + " without a line for " + ref
	case line[0] == '*' && out.ok():
		return pushOK, line
	case line[0] == '*':
		return pushAmbiguous, "new ref reported but push " + out.describe()
	default:
		return pushRejected, line
	}
}

// prRead is the answer of the one PR read of step 12.9.
type prRead struct {
	prs []pr
	err error
}

// readPRs is the one PR read of step 12.9.
func (a *attempt) readPRs(branch string, timeout time.Duration) prRead {
	s := a.s
	owner, _, _ := strings.Cut(s.slug, "/")
	var prs []pr
	if err := s.ghJSONWithin(timeout, &prs, "api", "--hostname", ghHost,
		"repos/"+s.slug+"/pulls?state=all&head="+owner+":"+branch+"&per_page=100"); err != nil {
		return prRead{err: err}
	}
	if prs == nil {
		return prRead{err: fmt.Errorf("%w: malformed answer", errGh)}
	}
	return prRead{prs: prs}
}

// matching returns the PRs that match head repo, ref, OID and base.
func (a *attempt) matching(prs []pr, branch string) []pr {
	return slices.DeleteFunc(slices.Clone(prs), func(p pr) bool {
		return !strings.EqualFold(p.headRepo(), a.s.slug) || p.Head.Ref != branch ||
			p.Head.SHA != a.oid || p.Base.Ref != a.s.branchBase
	})
}

// unique is the one matching PR, when there is exactly one and its URL is usable.
func (a *attempt) unique(prs []pr, branch string) (pr, bool) {
	m := a.matching(prs, branch)
	if len(m) != 1 || !a.prURL(m[0].HTMLURL) {
		return pr{}, false
	}
	return m[0], true
}

// prURL reports whether u is the URL of a pull request of the target.
func (a *attempt) prURL(u string) bool {
	prefix := "https://github.com/" + a.s.slug + "/pull/"
	if len(u) <= len(prefix) || !strings.EqualFold(u[:len(prefix)], prefix) {
		return false
	}
	_, err := strconv.ParseUint(u[len(prefix):], 10, 64)
	return err == nil
}

func (a *attempt) setPR(p pr) {
	a.st.PR = p.HTMLURL
	a.st.Outcome = prefixPR + p.HTMLURL
}

// ambiguous is step 12.10: read-only queries, no resend. read is the step 12.9 read, or nil when
// step 12.9 never ran (the push was ambiguous), in which case it is made now.
func (a *attempt) ambiguous(branch string, read *prRead, note string) *stop {
	s := a.s
	if read == nil {
		r := a.readPRs(branch, s.o.limits.step)
		read = &r
	}
	prs, ok := read.prs, read.err == nil
	ls := s.git(s.repo, "ls-remote", "origin", "refs/heads/"+branch)
	remote := "ls-remote failed: " + ls.describe()
	atOID := false
	if ls.ok() {
		fields := strings.Fields(string(ls.stdout))
		switch {
		case len(fields) == 0:
			remote = "branch absent on origin"
		default:
			remote = "branch at " + fields[0]
			atOID = len(fields) == 2 && fields[0] == a.oid //nolint:mnd // oid and ref
		}
	}
	shown := make([]string, 0, len(prs))
	for _, p := range prs {
		shown = append(shown, fmt.Sprintf("#%d %s head %s:%s@%s base %s", p.Number, p.HTMLURL,
			p.headRepo(), p.Head.Ref, p.Head.SHA, p.Base.Ref))
	}
	reads := "PR read failed"
	if ok {
		reads = strconv.Itoa(len(prs)) + " PR(s): " + strings.Join(shown, "; ")
	}
	facts := note + "; " + remote + "; " + reads
	if ok {
		if p, ok := a.unique(prs, branch); ok {
			a.setPR(p)
			return nil
		}
		if len(prs) == 0 && ls.ok() && atOID {
			return halt(
				outPushedNoPR,
				"%s; create it with: gh pr create --repo github.com/%s --base %s --head %s "+
					"--title %q --body-file <the stamp's body>",
				facts,
				s.slug,
				s.branchBase,
				branch,
				a.title,
			)
		}
	}
	return halt(outPublishUnknown, "%s", facts)
}

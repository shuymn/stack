// Package improve is the stack-improve extension (docs/design/phase3-cycle.md §3.4): off until the
// owner enables it, it makes at most one model attempt per ISO week on the public evidence observe
// wrote, under one run lock, and publishes a change only as one pull request after every gate
// passed. It answers the cycle stage with pass or warn, never block.
package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
)

// Statuses of a response.
const (
	StatusPass = "pass"
	StatusWarn = "warn"
)

// Finding kinds.
const (
	kindLeftover    = "leftover-worktree"
	kindCostUnknown = "cost-unknown"
	kindOutcome     = "improve-outcome"
	kindInternal    = "internal-error"
)

const (
	unknownCost = "unknown"
	detailLen   = 300
	redactMark  = "[redacted]"
	// maxContext and maxFindings keep the encoded response far below core's 1 MiB limit; the
	// stamp keeps the full detail.
	maxContext  = 16 << 10
	maxFindings = 50
)

// Time budget (design §3.9).
const (
	defaultBudget     = 29 * time.Minute
	defaultReserve    = 60 * time.Second
	defaultFinal      = 10 * time.Second
	defaultModelMin   = 5 * time.Minute
	defaultAfterModel = 9 * time.Minute
	defaultCodexMin   = 5 * time.Minute
	defaultPushPR     = 2 * time.Minute
	defaultStep       = 60 * time.Second
)

// limits are the time budget; zero values take the §3.9 defaults. Tests shorten them.
type limits struct {
	budget, reserve time.Duration
	// final is what cleanup leaves for the stamp and the response: the worktree removal runs inside
	// the reserve, not before it.
	final                time.Duration
	modelMin, afterModel time.Duration
	codexMin, pushPR     time.Duration
	// step bounds each pre-model gh and git call and each after-model step (commit, checks, push,
	// PR and its read).
	step time.Duration
}

// Request is the part of the extension protocol request improve reads.
type Request struct {
	Stage string `json:"stage"`
	Repo  string `json:"repo"`
}

// Options configure one improve run. Zero values take the defaults.
type Options struct {
	// Home is the user's home directory; config, state, settings and latest.json derive from it.
	Home string
	// Env is the environment children inherit (repository-location variables are dropped); nil
	// means os.Environ().
	Env []string
	// LookPath finds git, gh, stack, claude and codex; nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Now is the wall clock of the week, the freshness bound and PR ages; nil means time.Now.
	Now func() time.Time
	// Location is the zone of the ISO week; nil means time.Local.
	Location *time.Location
	// Stderr receives progress lines, redacted; nil discards them.
	Stderr io.Writer

	// Test seams.
	goos   string
	clock  func() time.Time
	limits limits
	// hook, when set, runs at named points of the attempt (worktree, model, check, codex-input,
	// codex, pushed, cleanup).
	hook func(point string)
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

func (o Options) withDefaults() Options {
	if o.Env == nil {
		o.Env = os.Environ()
	}
	o.Env = dropRepoVars(o.Env)
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.Local //nolint:gosmopolitan // the week is the owner's local week (design §9 D2)
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.goos == "" {
		o.goos = runtime.GOOS
	}
	if o.clock == nil {
		o.clock = time.Now
	}
	l := &o.limits
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&l.budget, defaultBudget},
		{&l.reserve, defaultReserve},
		{&l.final, defaultFinal},
		{&l.modelMin, defaultModelMin},
		{&l.afterModel, defaultAfterModel},
		{&l.codexMin, defaultCodexMin},
		{&l.pushPR, defaultPushPR},
		{&l.step, defaultStep},
	} {
		if *d.v <= 0 {
			*d.v = d.def
		}
	}
	return o
}

// state is one run.
type state struct {
	o        Options
	ctx      context.Context //nolint:containedctx // one run's parent context
	deadline time.Time
	now      time.Time
	creds    []string
	findings []Finding

	repo, slug, branchBase string
	paths                  []string
	baseOID                string
	week, stampPath        string
	stateDir               string
	tools                  map[string]string
	settings               userSettings
	settingsErr            error
}

// Run answers one cycle request. It never answers block.
func Run(ctx context.Context, o Options, req Request) Response {
	o = o.withDefaults()
	s := &state{
		o: o, ctx: ctx, deadline: o.clock().Add(o.limits.budget), now: o.Now(),
		repo: req.Repo, tools: map[string]string{},
		stateDir: filepath.Join(o.Home, ".local", "state", "stack", "improve"),
	}
	s.creds = envCreds(o.Env)
	status, text := s.guardedRun()
	return s.respond(status, text)
}

// guardedRun is run with a panic turned into an internal-error finding.
func (s *state) guardedRun() (string, string) {
	status, text := StatusWarn, "improve stopped on an internal error"
	func() {
		defer func() {
			if r := recover(); r != nil {
				s.logf("internal error: %v\n%s", r, debug.Stack())
				s.finding(kindInternal, "stack-improve", fmt.Sprintf("internal error: %v", r))
			}
		}()
		status, text = s.run()
	}()
	return status, text
}

// respond builds the redacted response; any finding makes it warn.
func (s *state) respond(status, text string) Response {
	findings := make([]Finding, 0, min(len(s.findings), maxFindings))
	for _, f := range s.findings {
		if len(findings) == maxFindings {
			s.logf("%d more findings not listed", len(s.findings)-maxFindings)
			break
		}
		findings = append(findings, Finding{
			Kind: f.Kind, Where: cut(s.redact(f.Where)),
			Detail: cut(s.redact(f.Detail)),
		})
	}
	if len(findings) > 0 {
		status = StatusWarn
	}
	return Response{
		Status: status, Context: cutBytes(s.redact(text), maxContext), Findings: findings,
	}
}

func (s *state) finding(kind, where, detail string) {
	s.findings = append(s.findings, Finding{Kind: kind, Where: where, Detail: detail})
}

// logf writes one redacted progress line.
func (s *state) logf(format string, args ...any) {
	_, _ = fmt.Fprintln(s.o.Stderr, s.redact(fmt.Sprintf(format, args...)))
}

// redact replaces every credential value with the marker.
func (s *state) redact(text string) string {
	for _, c := range s.creds {
		text = strings.ReplaceAll(text, c, redactMark)
	}
	return text
}

// leaks reports whether text holds a credential value.
func (s *state) leaks(text string) bool {
	return slices.ContainsFunc(s.creds, func(c string) bool { return strings.Contains(text, c) })
}

// left is the time to the deadline.
func (s *state) left() time.Duration { return s.deadline.Sub(s.o.clock()) }

// run performs steps 0-13 and returns the status and context.
func (s *state) run() (string, string) {
	// 0. Enabled.
	on, err := enabled(s.o.Home)
	if err != nil {
		return StatusWarn, err.Error()
	}
	if !on {
		return StatusPass, "improve disabled"
	}
	if s.o.goos == "windows" {
		return StatusPass, "improve runs on macOS and Linux"
	}
	// 1. Target.
	if s.repo == "" {
		return StatusPass, "not in a repository"
	}
	// The credential set is complete before the first child, so every stderr line is masked.
	s.settings, s.settingsErr = readSettings(s.o.Home)
	s.creds = append(s.creds, s.settings.creds...)
	if status, text, ok := s.target(); !ok {
		return status, text
	}
	// 2. Run lock.
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return StatusWarn, "cannot create the improve state directory"
	}
	lock := flock.New(filepath.Join(s.stateDir, "lock"), flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil {
		return StatusWarn, "cannot take the improve run lock"
	}
	if !locked {
		return StatusPass, "another improve run is active"
	}
	defer func() { _ = lock.Unlock() }()
	return s.locked()
}

// target is step 1: origin, repository visibility and the settings boundary.
func (s *state) target() (string, string, bool) {
	// Exactly one URL: with several, fetch would use the first and push every one of them.
	out := s.git(s.repo, "config", "--get-all", "remote.origin.url")
	urls := strings.Split(strings.TrimSuffix(string(out.stdout), "\n"), "\n")
	if out.ok() && len(urls) == 1 {
		s.slug = slugOf(urls[0])
	}
	if s.slug == "" {
		return StatusPass, "target is not a public GitHub repository (origin is not exactly one GitHub URL)",
			false
	}
	var view struct {
		NameWithOwner    string `json:"nameWithOwner"`
		Visibility       string `json:"visibility"`
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := s.ghJSON(&view, "repo", "view", ghHost+"/"+s.slug, "--json",
		"nameWithOwner,visibility,defaultBranchRef"); err != nil {
		return StatusWarn, "gh repo view failed: " + err.Error(), false
	}
	if view.Visibility != "PUBLIC" || !strings.EqualFold(view.NameWithOwner, s.slug) {
		return StatusPass, "target is not a public GitHub repository", false
	}
	if !validBranch(view.DefaultBranchRef.Name) {
		return StatusWarn, "gh repo view returned no usable default branch", false
	}
	s.branchBase = view.DefaultBranchRef.Name
	if s.settingsErr != nil {
		return StatusWarn, s.settingsErr.Error(), false
	}
	if s.settings.additional {
		return StatusWarn, "~/.claude/settings.json lists permissions.additionalDirectories, " +
			"which would widen the model's file-tool boundary; improve stopped", false
	}
	return "", "", true
}

// validBranch rejects names git could read as an option or a refspec.
func validBranch(b string) bool {
	return b != "" && !strings.HasPrefix(b, "-") && !strings.ContainsAny(b, ": \t\n~^?*[\\")
}

// locked runs steps 3-13 under the run lock.
func (s *state) locked() (string, string) {
	// 3. Base and config.
	ref := "refs/remotes/origin/" + s.branchBase
	// --refmap= stops the configured remote.origin.fetch from updating other refs; --no-prune and
	// --no-prune-tags override fetch.prune and fetch.pruneTags.
	if out := s.git(s.repo, "fetch", "--no-tags", "--no-write-fetch-head",
		"--no-recurse-submodules", "--refmap=", "--no-prune", "--no-prune-tags", "origin",
		"+refs/heads/"+s.branchBase+":"+ref); !out.ok() {
		return StatusWarn, "git fetch of origin " + s.branchBase + " failed: " + out.describe()
	}
	out := s.git(s.repo, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if !out.ok() {
		return StatusWarn, "cannot resolve " + ref + ": " + out.describe()
	}
	s.baseOID = strings.TrimSpace(string(out.stdout))
	out = s.git(s.repo, "ls-tree", "-z", s.baseOID, "--", ".stack/improve.toml")
	if !out.ok() {
		return StatusWarn, "git ls-tree failed: " + out.describe()
	}
	if !strings.Contains(string(out.stdout), " blob ") {
		return StatusPass, s.slug + " has no .stack/improve.toml"
	}
	out = s.git(s.repo, "show", s.baseOID+":.stack/improve.toml")
	if !out.ok() {
		return StatusWarn, "cannot read .stack/improve.toml: " + out.describe()
	}
	paths, err := repoConfig(out.stdout)
	if err != nil {
		return StatusWarn, err.Error()
	}
	s.paths = paths
	// 4. Open PR gate.
	if status, text, ok := s.openPRs(); !ok {
		return status, text
	}
	// 5. Week stamp.
	s.week = weekName(s.now.In(s.o.Location))
	s.stampPath = filepath.Join(s.stateDir, s.week)
	if status, text, ok := s.existingStamp(); !ok {
		return status, text
	}
	// 6. Evidence.
	ev := readEvidence(latestPath(s.o.Home), s.now)
	if ev.stop != "" {
		if ev.warn {
			return StatusWarn, ev.stop
		}
		return StatusPass, ev.stop
	}
	if s.modelTime() < s.o.limits.modelMin {
		return StatusPass, "not enough time left for the model"
	}
	// 7-13.
	return s.attempt(ev.public)
}

// modelTime is the model's timeout: what is left minus the reserve and the after-model steps.
func (s *state) modelTime() time.Duration {
	return s.left() - s.o.limits.reserve - s.o.limits.afterModel
}

// pr is the part of a REST pull request improve reads.
//
//nolint:tagliatelle // GitHub REST uses snake_case.
type pr struct {
	Number    int    `json:"number"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	Head      struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p pr) headRepo() string {
	if p.Head.Repo == nil {
		return ""
	}
	return p.Head.Repo.FullName
}

const (
	day       = 24 * time.Hour
	prAgeWarn = 14 * day
)

// openPRs is step 4: any open improve/ or kaizen/ PR of the target itself, on every page.
func (s *state) openPRs() (string, string, bool) {
	var pages [][]pr
	if err := s.ghJSON(&pages, "api", "--hostname", ghHost, "--paginate", "--slurp",
		"repos/"+s.slug+"/pulls?state=open&per_page=100"); err != nil {
		return StatusWarn, "open PR check failed: " + err.Error(), false
	}
	if pages == nil {
		return StatusWarn, "open PR check failed: " + errGh.Error() + ": malformed answer", false
	}
	for _, page := range pages {
		for _, p := range page {
			if !strings.HasPrefix(p.Head.Ref, "improve/") &&
				!strings.HasPrefix(p.Head.Ref, "kaizen/") {
				continue
			}
			if !strings.EqualFold(p.headRepo(), s.slug) {
				continue
			}
			text := fmt.Sprintf("open improve PR #%d (%s) waits for the owner", p.Number, p.HTMLURL)
			created, err := time.Parse(time.RFC3339, p.CreatedAt)
			if err != nil {
				return StatusWarn, text + "; its age is unknown", false
			}
			if age := s.now.Sub(created); age > prAgeWarn {
				return StatusWarn, fmt.Sprintf(
					"%s; open for %d days",
					text,
					int(age/day),
				), false
			}
			return StatusPass, text, false
		}
	}
	return "", "", true
}

// existingStamp is step 5.
func (s *state) existingStamp() (string, string, bool) {
	if _, err := os.Lstat(s.stampPath); errors.Is(err, fs.ErrNotExist) {
		return "", "", true
	}
	st, err := readStamp(s.stampPath)
	if err != nil || st.Outcome == outStarted || st.Outcome == "" {
		return StatusWarn, fmt.Sprintf("attempt outcome unknown for %s (%s); stamp %s",
			s.week, recorded(st), s.stampPath), false
	}
	text := fmt.Sprintf("week %s already attempted: %s; stamp %s", s.week, st.Outcome, s.stampPath)
	if finalWarn(st) {
		return StatusWarn, text, false
	}
	return StatusPass, text, false
}

// tool resolves a tool once.
func (s *state) tool(name string) (string, error) {
	if p, ok := s.tools[name]; ok {
		return p, nil
	}
	p, err := s.o.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found: %w", name, err)
	}
	s.tools[name] = p
	return p, nil
}

// child runs a tool with a timeout cut to the deadline minus the reserve (c.reserve when set), and
// passes its stderr, redacted, to improve's stderr.
func (s *state) child(name string, c command) outcome {
	p, err := s.tool(name)
	if err != nil {
		s.logf("%v", err)
		return outcome{exit: -1, err: fmt.Errorf("%s: %w: %w", name, errStart, err)}
	}
	// A git -C or claude without a directory would run in improve's own cwd, possibly the owner's
	// checkout.
	noGitDir := len(c.args) > 1 && c.args[0] == "-C" && c.args[1] == ""
	if (name == "claude" && c.dir == "") || noGitDir {
		return outcome{exit: -1, err: fmt.Errorf("%s: %w: no directory", name, errStart)}
	}
	c.name = p
	if c.env == nil {
		c.env = s.o.Env
	}
	reserve := s.o.limits.reserve
	if c.reserve > 0 {
		reserve = c.reserve
	}
	c.timeout = min(c.timeout, s.left()-reserve)
	if c.timeout <= 0 {
		return outcome{
			exit:     -1,
			timedOut: true,
			err:      fmt.Errorf("%s: %w: no time left", name, errStart),
		}
	}
	out := run(s.ctx, c)
	if text := strings.TrimSpace(string(out.stderr)); text != "" {
		for line := range strings.SplitSeq(text, "\n") {
			s.logf("[%s] %s", name, line)
		}
	}
	return out
}

// git runs git in dir.
func (s *state) git(dir string, args ...string) outcome {
	return s.child("git", s.gitCmd(dir, args...))
}

// gitCmd is a git call in dir. Replace refs are off: the checks must read the objects the push
// sends.
func (s *state) gitCmd(dir string, args ...string) command {
	return command{
		args: append([]string{"-C", dir}, args...), timeout: s.o.limits.step,
		env: append(slices.Clone(s.o.Env), "GIT_TERMINAL_PROMPT=0", noReplace),
	}
}

// noReplace turns replace refs off for git and for the git `stack check public` runs.
const noReplace = "GIT_NO_REPLACE_OBJECTS=1"

// ghHost pins every gh call to github.com, whatever GH_HOST or gh's default host says.
const ghHost = "github.com"

var ghEnvVars = []string{"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1"}

func (s *state) ghEnv() []string { return append(slices.Clone(s.o.Env), ghEnvVars...) }

var errGh = errors.New("gh failed")

// ghJSON runs gh and decodes its stdout into v; errors are fixed summaries.
func (s *state) ghJSON(v any, args ...string) error {
	return s.ghJSONWithin(s.o.limits.step, v, args...)
}

// ghJSONWithin is ghJSON with a timeout.
func (s *state) ghJSONWithin(timeout time.Duration, v any, args ...string) error {
	out := s.child("gh", command{args: args, env: s.ghEnv(), timeout: timeout})
	if !out.ok() {
		return fmt.Errorf("%w: %s", errGh, out.describe())
	}
	if err := json.Unmarshal(out.stdout, v); err != nil {
		return fmt.Errorf("%w: malformed answer", errGh)
	}
	return nil
}

// cutBytes keeps at most n bytes of text, on a rune boundary, marking a cut.
func cutBytes(text string, n int) string {
	if len(text) <= n {
		return text
	}
	i := n
	for i > 0 && !utf8.RuneStart(text[i]) {
		i--
	}
	return text[:i] + "\n[cut]"
}

// cut collapses whitespace and keeps at most detailLen characters.
func cut(text string) string {
	const n = detailLen
	r := []rune(strings.Join(strings.Fields(text), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

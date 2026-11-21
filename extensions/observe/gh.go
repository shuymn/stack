package observe

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const ghRepos = 10

var (
	reGitHub = regexp.MustCompile(`github\.com[:/]([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)
	// reFixClause and reFixRef are the legacy fix-PR rules (observe.py:42-45).
	reFixClause = regexp.MustCompile(`(?i)\bfix(?:es|ed)?\b[^#\n]{0,60}?(` + oneRef +
		`(?:[ \t]*(?:,|\band\b|\bor\b|&)?[ \t]*(?:fix(?:es|ed)?[ \t]+)?` + oneRef + `)*)`)
	reFixRef = regexp.MustCompile(`(?:\b([\w.-]+/[\w.-]+))?#(\d+)`)
	reRevert = regexp.MustCompile(`(?i)\brevert`)
	reGhVer  = regexp.MustCompile(`^gh version (\d+\.\d+\.\d+)\b`)
	// reModel is the shape of stack's gh_model (internal/ghargs GhVersion); anything else is
	// unknown, never copied into the response.
	reModel   = regexp.MustCompile(`^v\d+(?:\.\d+)*(?:[-+][0-9A-Za-z.-]+)?$`)
	ghEnvVars = []string{"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1"}
)

const oneRef = `(?:\b[\w.-]+/[\w.-]+)?#\d+`

// originRepo returns owner/name of cwd's GitHub origin, or "".
func (s *state) originRepo(git, cwd string) string {
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return ""
	}
	out := s.child(command{
		name: git, args: []string{"-C", cwd, "config", "--get", "remote.origin.url"},
		timeout: s.o.gitTimeout,
	})
	if !out.ok() {
		return ""
	}
	m := reGitHub.FindStringSubmatch(strings.TrimSpace(string(out.stdout)))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

func (s *state) ghEnv() []string {
	return append(slices.Clone(s.o.Env), ghEnvVars...)
}

// ghJSON runs gh and decodes its stdout, which must be a JSON value of the shape open ('{' for
// an object, '[' for a list); any failure is the source's first gh error, a fixed per-cause
// summary that never quotes gh's output.
func (s *state) ghJSON(gh string, args []string, open byte, into any) error {
	cmd := strings.Join(args[:2], " ")
	out := s.child(command{name: gh, args: args, env: s.ghEnv(), timeout: s.o.ghTimeout})
	if out.timedOut && s.work.Err() != nil {
		return fmt.Errorf("gh %s: %w", cmd, s.work.Err())
	}
	if !out.ok() {
		return fmt.Errorf("gh %s failed: %s", cmd, out.describe()) //nolint:err113 // fixed summary
	}
	if !opens(out.stdout, open) || json.Unmarshal(out.stdout, into) != nil {
		return fmt.Errorf("gh %s: %w", cmd, errOutput)
	}
	return nil
}

type ghPR struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	MergedAt string `json:"mergedAt"`
	URL      string `json:"url"`
}

type ghRun struct {
	DisplayTitle string `json:"displayTitle"`
	CreatedAt    string `json:"createdAt"`
	URL          string `json:"url"`
	WorkflowName string `json:"workflowName"`
}

// gh reads merged reverts, fix PRs and failed default-branch runs of the ten repositories most
// worked in (observe.py:342-378). The first gh error ends the source; repositories read before it
// keep their items.
func (s *state) gh(st *Source) error {
	gh, err := s.o.LookPath("gh")
	if err != nil {
		return errors.New("gh not found") //nolint:err113 // a fixed per-cause summary
	}
	if auth := s.child(command{
		name: gh, args: []string{"auth", "status"}, env: s.ghEnv(), timeout: s.o.ghTimeout,
	}); !auth.ok() {
		if s.work.Err() != nil {
			return fmt.Errorf("gh auth status: %w", s.work.Err())
		}
		s.finding(kindGhAuth, "gh auth status", "gh auth failed: "+auth.describe())
	}
	weights := map[string]int{}
	if git, err := s.o.LookPath("git"); err == nil {
		cwds := make([]string, 0, len(s.cwds))
		for cwd := range s.cwds {
			cwds = append(cwds, cwd)
		}
		slices.Sort(cwds)
		for _, cwd := range cwds {
			if s.work.Err() != nil {
				return fmt.Errorf("gh: %w", s.work.Err())
			}
			if repo := s.originRepo(git, cwd); repo != "" {
				weights[repo] += s.cwds[cwd]
			}
		}
	}
	repos := make([]string, 0, len(weights))
	for r := range weights {
		repos = append(repos, r)
	}
	slices.SortFunc(repos, func(a, b string) int {
		return cmp.Or(cmp.Compare(weights[b], weights[a]), cmp.Compare(a, b))
	})
	day := s.since.Format(time.DateOnly)
	for _, repo := range repos[:min(len(repos), ghRepos)] {
		st.Files++
		items, err := s.ghRepo(gh, repo, day, st)
		if err != nil {
			return err
		}
		for _, it := range items {
			s.add(it.Signal, it.Where, it.Excerpt, mustTime(it.TS), it.Provenance, it.Repo)
		}
		*st.ReposRead++
	}
	return nil
}

func (s *state) ghRepo(gh, repo, day string, st *Source) ([]Item, error) {
	var view struct {
		Visibility       string `json:"visibility"`
		DefaultBranchRef *struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := s.ghJSON(
		gh,
		[]string{"repo", "view", repo, "--json", "visibility,defaultBranchRef"},
		'{',
		&view,
	); err != nil {
		return nil, err
	}
	if view.Visibility == "" {
		return nil, fmt.Errorf("gh repo view: %w", errOutput)
	}
	prov := "synthetic"
	if strings.EqualFold(view.Visibility, "PUBLIC") {
		prov = "public"
	}
	branch := "main"
	if view.DefaultBranchRef != nil && view.DefaultBranchRef.Name != "" {
		branch = view.DefaultBranchRef.Name
	}
	var prs []ghPR
	if err := s.ghJSON(gh, []string{
		"pr", "list", "-R", repo, "--state", "merged", "--search", "merged:>=" + day,
		"--limit", "200", "--json", "number,title,body,mergedAt,url",
	}, '[', &prs); err != nil {
		return nil, err
	}
	for _, pr := range prs {
		// A list item without what the signals read is wrongly shaped: the repo is not read.
		if _, ok := parseTime(pr.MergedAt); !ok || pr.Number <= 0 {
			return nil, fmt.Errorf("gh pr list: %w", errOutput)
		}
	}
	st.Records += len(prs)
	var items []Item
	item := func(signal, where, excerpt string, t time.Time) {
		items = append(
			items,
			Item{
				Signal:     signal,
				Where:      where,
				Excerpt:    excerpt,
				TS:         iso(t),
				Provenance: prov,
				Repo:       repo,
			},
		)
	}
	numbers := map[int]bool{}
	var window []ghPR
	for _, pr := range prs {
		if t, ok := parseTime(pr.MergedAt); ok && s.inWindow(t) {
			window = append(window, pr)
			numbers[pr.Number] = true
		}
	}
	for _, pr := range window {
		merged, _ := parseTime(pr.MergedAt)
		where := pr.URL
		if where == "" {
			where = repo + "#" + strconv.Itoa(pr.Number)
		}
		if reRevert.MatchString(pr.Title) {
			item("revert", where, pr.Title, merged)
		}
		if fixed := fixedPRs(pr, repo, numbers); len(fixed) > 0 {
			refs := make([]string, len(fixed))
			for i, n := range fixed {
				refs[i] = "#" + strconv.Itoa(n)
			}
			item(
				"fix-pr",
				where,
				fmt.Sprintf("%s (fixes %s)", pr.Title, strings.Join(refs, ", ")),
				merged,
			)
		}
	}
	var runs []ghRun
	if err := s.ghJSON(gh, []string{
		"run",
		"list",
		"-R",
		repo,
		"--branch",
		branch,
		"--status",
		"failure",
		"--created",
		">=" + day,
		"--limit",
		"100",
		"--json",
		"displayTitle,createdAt,url,workflowName",
	}, '[', &runs); err != nil {
		return nil, err
	}
	for _, r := range runs {
		if _, ok := parseTime(r.CreatedAt); !ok {
			return nil, fmt.Errorf("gh run list: %w", errOutput)
		}
	}
	for _, r := range runs {
		st.Records++
		if t, ok := parseTime(r.CreatedAt); ok && s.inWindow(t) {
			where := r.URL
			if where == "" {
				where = repo
			}
			item("ci-fail-main", where, r.WorkflowName+": "+r.DisplayTitle, t)
		}
	}
	return items, nil
}

// fixedPRs are the merged in-window PRs of repo, other than pr, that pr says it fixes.
func fixedPRs(pr ghPR, repo string, numbers map[int]bool) []int {
	set := map[int]bool{}
	for _, clause := range reFixClause.FindAllStringSubmatch(pr.Title+"\n"+pr.Body, -1) {
		for _, ref := range reFixRef.FindAllStringSubmatch(clause[1], -1) {
			if ref[1] != "" && !strings.EqualFold(ref[1], repo) {
				continue
			}
			if n, err := strconv.Atoi(ref[2]); err == nil && numbers[n] && n != pr.Number {
				set[n] = true
			}
		}
	}
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// ghVersion compares the installed gh release with the one the guard models (design §3.3).
func (s *state) ghVersion() {
	installed, model := unknown, unknown
	if gh, err := s.o.LookPath("gh"); err == nil {
		out := s.child(
			command{name: gh, args: []string{"--version"}, env: s.ghEnv(), timeout: s.o.ghTimeout},
		)
		if out.ok() {
			first, _, _ := strings.Cut(string(out.stdout), "\n")
			if m := reGhVer.FindStringSubmatch(strings.TrimSpace(first)); m != nil {
				installed = "v" + m[1]
			}
		}
	}
	if stack, err := s.o.LookPath("stack"); err == nil {
		out := s.child(
			command{name: stack, args: []string{"--version", "--json"}, timeout: s.o.ghTimeout},
		)
		var v struct {
			Version *string `json:"version"`
			GhModel *string `json:"gh_model"` //nolint:tagliatelle // stack --version --json
		}
		if out.ok() && json.Unmarshal(out.stdout, &v) == nil && v.Version != nil &&
			v.GhModel != nil &&
			reModel.MatchString(*v.GhModel) {
			model = *v.GhModel
		}
	}
	s.latest.Gh.Installed, s.latest.Gh.Model = installed, model
	if installed == unknown || model == unknown || installed != model {
		s.finding(
			kindGhVersion,
			"gh --version",
			fmt.Sprintf(
				"installed gh %s, guard modelled %s: check internal/ghargs against the installed gh "+
					"(a flag the table lacks makes the guard scan the rest of a call as literal text)",
				installed,
				model,
			),
		)
	}
}

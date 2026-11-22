package semantic

import (
	"context"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// Question names (§3.2).
const (
	MetaDuty    = "meta_duty_regression"
	Unsupported = "unsupported_claim"
)

// Questions are the two noul questions of §3.2, in the order they are sent.
var Questions = []jev.Question{
	{
		Name: MetaDuty,
		Type: jev.TypeNoul,
		Instructions: "Does `diff` remove or weaken a duty the project performs on itself, without " +
			"`title`, `body` or `commits` stating and justifying that change? Example (true): an " +
			"assertion deleted under \"refactor: simplify tests\". Example (false): the same with " +
			"body \"drop the duplicate assertion; TestX covers it\".",
		Criteria: map[string]string{
			"true": "deletes or loosens a test assertion, skips or deletes a test, drops a step of " +
				"the completion check (`scripts/test`, hooks, CI), removes or narrows a safety rule " +
				"or deny, adds `--no-verify` or error suppression, or turns a must into a may in " +
				"agent instructions (`AGENTS.md`, `CLAUDE.md`, skills), and the texts say nothing " +
				"of it",
			"false": "no such change, or the texts state it and why",
		},
	},
	{
		Name: Unsupported,
		Type: jev.TypeNoul,
		Instructions: "Do `title`, `body` or `commits` claim something `diff` does not support? " +
			"Example (true): \"add tests for X\" with no `_test.go` change. Example (false): " +
			"\"rename Foo to Bar\" with exactly that rename.",
		Criteria: map[string]string{
			"true": "claims tests, docs or files the diff does not touch, an issue fixed by code " +
				"that is not changed, verification or a measured number with no evidence in the " +
				"texts or the diff, or 'no behaviour change' where the diff changes behaviour",
			"false": "every claim is visible in the diff or carries its evidence",
		},
		WholeOnly: true,
	},
}

// Texts are the PR texts of an outbound request.
type Texts struct {
	Title, Body, Branch string
}

// State is what one range gives: its commit messages and its diff split by file.
type State struct {
	Commits []string
	Units   []jev.Unit
}

// Prepare runs git log and git diff on rng.
func Prepare(ctx context.Context, g Git, rng string) (State, error) {
	commits, err := g.Commits(ctx, rng)
	if err != nil {
		return State{}, err
	}
	diff, err := g.Diff(ctx, rng)
	if err != nil {
		return State{}, err
	}
	return State{Commits: commits, Units: SplitDiff(diff)}, nil
}

// Input is the packing input of a state: the texts as fixed fields, the diff files as units.
func Input(t Texts, s State) jev.Input {
	commits := s.Commits
	if commits == nil {
		commits = []string{}
	}
	return jev.Input{
		Fixed: []jev.Field{
			{Key: "title", Value: t.Title},
			{Key: "body", Value: t.Body},
			{Key: "branch", Value: t.Branch},
			{Key: "commits", Value: commits},
		},
		UnitsKey: "diff", NameKey: "file", TextKey: "patch",
		Units:     s.Units,
		Questions: Questions,
	}
}

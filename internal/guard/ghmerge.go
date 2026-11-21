package guard

import (
	"net/http"

	"github.com/shuymn/stack/internal/ghargs"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

const mergeAdvice = "Merge with stack pr merge <pr> <head-sha> [--merge|--squash|--rebase]: it " +
	"merges only that head, once every check on it passed, and reads the result back."

// mergeFields are the GraphQL mutation fields that merge a pull request now or later.
var mergeFields = map[string]bool{"mergePullRequest": true, "enablePullRequestAutoMerge": true}

// rawMerge denies merging outside `stack pr merge`: `gh pr merge` without effective help or
// --disable-auto, `gh api -X PUT repos/<o>/<r>/pulls/<n>/merge`, and a `gh api graphql` mutation
// selecting a merge field. A query gqlparser cannot parse is not judged.
func rawMerge(calls []ghCall, opts Options) (Decision, bool) {
	for _, c := range calls {
		if c.gh.Help { // gh shows help and runs nothing
			continue
		}
		switch c.gh.Command {
		case "pr merge":
			if !c.gh.DisableAuto {
				return deny("gh pr merge is not run directly. %s", mergeAdvice)
			}
		case "api":
			if c.gh.Method == http.MethodPut && ghargs.IsPullMerge(c.gh.Endpoint) {
				return deny("gh api PUT %s merges a pull request. %s",
					c.gh.Endpoint, mergeAdvice)
			}
			for _, q := range c.gh.Queries {
				text, ok := readInput(q, c.call, opts.Home)
				if ok && q.JSON {
					text, ok = ghargs.QueryFromJSON([]byte(text))
				}
				if !ok {
					continue
				}
				if field, ok := graphqlMerge(text); ok {
					return deny("gh api graphql runs the mutation field %s. %s",
						field, mergeAdvice)
				}
			}
		}
	}
	return Decision{}, false
}

// graphqlMerge returns the merge field a mutation of the document selects, following fragment
// spreads and inline fragments of the same document. Aliases do not count, the field name does.
func graphqlMerge(query string) (string, bool) {
	doc, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return "", false
	}
	for _, op := range doc.Operations {
		if op.Operation != ast.Mutation {
			continue
		}
		if field, ok := selectsMerge(doc, op.SelectionSet, map[string]bool{}); ok {
			return field, true
		}
	}
	return "", false
}

func selectsMerge(
	doc *ast.QueryDocument,
	set ast.SelectionSet,
	seen map[string]bool,
) (string, bool) {
	for _, sel := range set {
		var sub ast.SelectionSet
		switch s := sel.(type) {
		case *ast.Field:
			if mergeFields[s.Name] {
				return s.Name, true
			}
			continue
		case *ast.InlineFragment:
			sub = s.SelectionSet
		case *ast.FragmentSpread:
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			f := doc.Fragments.ForName(s.Name)
			if f == nil {
				continue
			}
			sub = f.SelectionSet
		}
		if field, ok := selectsMerge(doc, sub, seen); ok {
			return field, true
		}
	}
	return "", false
}

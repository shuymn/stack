package ghargs

// GhVersion is the gh release whose flag declarations the table models, checked against
// pkg/cmd/<group>/<command>/*.go, internal/attachments/flags.go (--attach),
// pkg/cmdutil/repo_override.go (-R/--repo) and pkg/cmd/root/root.go (--help) of that tag.
const GhVersion = "v2.105.0"

type kind int

const (
	kBool        kind = iota // a boolean; NilBoolFlag registers the same NoOptDefVal "true"
	kString                  // a value, last one wins
	kStringArray             // repeatable, each value kept
	kStringSlice             // repeatable, comma-split
	kDuration                // a time.Duration value
)

type flagDef struct {
	name  string
	short string
	kind  kind
}

// attach is the repeatable --attach flag that internal/attachments adds.
var attach = flagDef{"attach", "", kStringArray}

// table lists every flag of each modelled command as declared in gh GhVersion. Persistent flags
// (--help on the root, -R/--repo on pr, issue and release) are added by flagSet.
var table = map[string][]flagDef{
	"pr create": {
		{"draft", "d", kBool},
		{"title", "t", kString},
		{"body", "b", kString},
		{"body-file", "F", kString},
		{"base", "B", kString},
		{"head", "H", kString},
		{"editor", "e", kBool},
		{"web", "w", kBool},
		{"fill-verbose", "", kBool},
		{"fill", "f", kBool},
		{"fill-first", "", kBool},
		{"reviewer", "r", kStringSlice},
		{"assignee", "a", kStringSlice},
		{"label", "l", kStringSlice},
		{"project", "p", kStringSlice},
		{"milestone", "m", kString},
		{"no-maintainer-edit", "", kBool},
		{"recover", "", kString},
		{"template", "T", kString},
		{"dry-run", "", kBool},
		attach,
	},
	"pr edit": {
		{"title", "t", kString},
		{"body", "b", kString},
		{"body-file", "F", kString},
		{"base", "B", kString},
		{"add-reviewer", "", kStringSlice},
		{"remove-reviewer", "", kStringSlice},
		{"add-assignee", "", kStringSlice},
		{"remove-assignee", "", kStringSlice},
		{"add-label", "", kStringSlice},
		{"remove-label", "", kStringSlice},
		{"add-project", "", kStringSlice},
		{"remove-project", "", kStringSlice},
		{"milestone", "m", kString},
		{"remove-milestone", "", kBool},
		attach,
	},
	"pr comment": {
		{"body", "b", kString},
		{"body-file", "F", kString},
		{"editor", "e", kBool},
		{"web", "w", kBool},
		{"edit-last", "", kBool},
		{"delete-last", "", kBool},
		{"yes", "", kBool},
		{"create-if-none", "", kBool},
		attach,
	},
	"pr review": {
		{"approve", "a", kBool},
		{"request-changes", "r", kBool},
		{"comment", "c", kBool},
		{"body", "b", kString},
		{"body-file", "F", kString},
	},
	"pr merge": {
		{"admin", "", kBool},
		{"delete-branch", "d", kBool},
		{"body", "b", kString},
		{"body-file", "F", kString},
		{"subject", "t", kString},
		{"merge", "m", kBool},
		{"rebase", "r", kBool},
		{"squash", "s", kBool},
		{"auto", "", kBool},
		{"disable-auto", "", kBool},
		{"match-head-commit", "", kString},
		{"author-email", "A", kString},
	},
	"issue create": {
		{"title", "t", kString},
		{"body", "b", kString},
		{"body-file", "F", kString},
		{"editor", "e", kBool},
		{"web", "w", kBool},
		{"assignee", "a", kStringSlice},
		{"label", "l", kStringSlice},
		{"project", "p", kStringSlice},
		{"milestone", "m", kString},
		{"recover", "", kString},
		{"template", "T", kString},
		{"type", "", kString},
		{"parent", "", kString},
		{"blocked-by", "", kStringSlice},
		{"blocking", "", kStringSlice},
		attach,
	},
	"issue edit": {
		{"title", "t", kString},
		{"body", "b", kString},
		{"body-file", "F", kString},
		{"add-assignee", "", kStringSlice},
		{"remove-assignee", "", kStringSlice},
		{"add-label", "", kStringSlice},
		{"remove-label", "", kStringSlice},
		{"add-project", "", kStringSlice},
		{"remove-project", "", kStringSlice},
		{"milestone", "m", kString},
		{"remove-milestone", "", kBool},
		{"type", "", kString},
		{"remove-type", "", kBool},
		{"parent", "", kString},
		{"remove-parent", "", kBool},
		{"add-sub-issue", "", kStringSlice},
		{"remove-sub-issue", "", kStringSlice},
		{"add-blocked-by", "", kStringSlice},
		{"remove-blocked-by", "", kStringSlice},
		{"add-blocking", "", kStringSlice},
		{"remove-blocking", "", kStringSlice},
		attach,
	},
	"issue comment": {
		{"body", "b", kString},
		{"body-file", "F", kString},
		{"editor", "e", kBool},
		{"web", "w", kBool},
		{"edit-last", "", kBool},
		{"delete-last", "", kBool},
		{"yes", "", kBool},
		{"create-if-none", "", kBool},
		attach,
	},
	"release create": {
		{"draft", "d", kBool},
		{"prerelease", "p", kBool},
		{"target", "", kString},
		{"title", "t", kString},
		{"notes", "n", kString},
		{"notes-file", "F", kString},
		{"discussion-category", "", kString},
		{"generate-notes", "", kBool},
		{"notes-start-tag", "", kString},
		{"latest", "", kBool},
		{"verify-tag", "", kBool},
		{"notes-from-tag", "", kBool},
		{"fail-on-no-commits", "", kBool},
	},
	"release edit": {
		{"draft", "", kBool},
		{"prerelease", "", kBool},
		{"latest", "", kBool},
		{"notes", "n", kString},
		{"title", "t", kString},
		{"discussion-category", "", kString},
		{"target", "", kString},
		{"tag", "", kString},
		{"notes-file", "F", kString},
		{"verify-tag", "", kBool},
	},
	"api": {
		{"hostname", "", kString},
		{"method", "X", kString},
		{"field", "F", kStringArray},
		{"raw-field", "f", kStringArray},
		{"header", "H", kStringArray},
		{"preview", "p", kStringSlice},
		{"include", "i", kBool},
		{"slurp", "", kBool},
		{"paginate", "", kBool},
		{"input", "", kString},
		{"silent", "", kBool},
		{"template", "t", kString},
		{"jq", "q", kString},
		{"cache", "", kDuration},
		{"verbose", "", kBool},
		{"allow-escape-sequences", "", kBool},
	},
}

// published names, per command, the text flags whose values are published and the flag naming a
// file whose content is (`-` = the call's stdin).
var published = map[string]struct {
	text     []string
	file     string
	attachOK bool
}{
	"pr create":      {[]string{"title", "body"}, "body-file", true},
	"issue create":   {[]string{"title", "body"}, "body-file", true},
	"pr edit":        {[]string{"title", "body"}, "body-file", true},
	"issue edit":     {[]string{"title", "body"}, "body-file", true},
	"pr comment":     {[]string{"body"}, "body-file", true},
	"issue comment":  {[]string{"body"}, "body-file", true},
	"pr review":      {[]string{"body"}, "body-file", false},
	"release create": {[]string{"title", "notes"}, "notes-file", false},
	"release edit":   {[]string{"title", "notes"}, "notes-file", false},
}

// aliases maps gh's built-in subcommand aliases to their command.
var aliases = map[string]string{
	"pr new":      "pr create",
	"issue new":   "issue create",
	"release new": "release create",
}

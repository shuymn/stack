package merge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shuymn/stack/internal/ext"
)

// MessageChecks are the outbound extensions `stack pr merge` requires for --subject and --body
// (docs/design/core-stages.md §4.2 step 2).
type MessageChecks struct {
	// Names is [merge].message_checks, each a valid extension name.
	Names []string
	// Ext runs the extensions.
	Ext ext.Options

	// resolve looks the names up; nil means ext.Resolve.
	resolve func([]string) []ext.Plan
	// timeout is the stage's deadline; zero means ext.Timeouts[ext.StageOutbound].
	timeout time.Duration
}

// messageError is a merge refused by its message checks: one stderr line per listed extension
// that blocked or could not answer. Error joins them so that the caller's one `stack pr merge: `
// prefix starts every line.
type messageError struct{ lines []string }

func (e messageError) Error() string {
	return strings.Join(e.lines, "\nstack pr merge: ")
}

func (e messageError) Unwrap() error { return ErrRefused }

// checkMessage runs the message checks as one outbound stage run in the repository repo when
// --subject or --body is given and the list is not empty. Every listed extension is required: a
// block, or an extension that is missing, not runnable, timed out, left unstarted or answered
// invalid output, refuses the merge. A valid warn continues, its marker on stderr.
func checkMessage(ctx context.Context, o Options) error {
	s := o.MessageChecks
	a := o.Merge
	if (a.Subject == nil && a.Body == nil) || len(s.Names) == 0 {
		return nil
	}
	resolve := s.resolve
	if resolve == nil {
		resolve = ext.Resolve
	}
	plans := resolve(s.Names)
	title, body, empty := "", "", ""
	if a.Subject != nil {
		title = *a.Subject
	}
	if a.Body != nil {
		body = *a.Body
	}
	req := ext.Request{
		Stage: ext.StageOutbound, Repo: o.repo, Title: &title, Body: &body, Branch: &empty,
	}
	request, err := json.Marshal(req)
	if err != nil { // strings only; it always encodes
		return refuse("could not encode the outbound request: %v", err)
	}
	eo := s.Ext
	eo.Timeout = s.timeout
	// Run has one result per plan that is not skipped, in plan order, so each result is judged at
	// its own position, also when a name is listed twice.
	results := ext.Run(ctx, eo, ext.StageOutbound, request, plans).Results

	var lines []string
	for _, p := range plans {
		if p.Skipped() {
			lines = append(lines, fmt.Sprintf("outbound could not check the message: %s: "+
				"not installed (%s%s not found on PATH)", p.Name, ext.Prefix, p.Name))
			continue
		}
		r := results[0]
		results = results[1:]
		switch {
		case r.Error != "":
			lines = append(lines, fmt.Sprintf("outbound could not check the message: %s: %s",
				p.Name, r.Error))
		case r.Status == ext.StatusBlock:
			lines = append(lines, fmt.Sprintf("outbound blocked: %s: %s", p.Name,
				strings.TrimSpace(r.Context)))
		case r.Status == ext.StatusWarn:
			_, _ = io.WriteString(o.Stderr, r.Marker()+"\n")
		}
	}
	if len(lines) > 0 {
		return messageError{lines}
	}
	return nil
}

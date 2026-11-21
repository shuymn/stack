package merge

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/proc"
)

// MergedStage is the merged stage that runs after a verified merge
// (docs/design/core-stages.md §4.2 step 3).
type MergedStage struct {
	// Names is [stages].merged, each a valid extension name.
	Names []string
	// Kits returns the kits of the request. It runs only when a runnable candidate exists, within
	// the stage's window; nil sends [].
	Kits func(ctx context.Context) *[]ext.Kit
	// Ext runs the extensions; the stage sets its Timeout.
	Ext ext.Options

	// resolve looks the names up; nil means ext.Resolve.
	resolve func([]string) []ext.Plan
	// timeout is the stage's window; zero means ext.Timeouts[ext.StageMerged].
	timeout time.Duration
}

// runMerged runs the merged stage for the verified merge m in the repository repo and writes each
// result that has something to show to w. It does nothing after a cancel, and nothing at all, not
// even a lookup of `.kits`, when every selected extension is missing. Only when a runnable
// candidate exists does the stage's window open: one end, 10 s from there, armed once, which bounds the
// `.kits` read, the request encoding, every extension and the reaping of their pipes, so the
// runner's deadline is that end less its pipe-reaping allowance. Write errors are ignored: the
// merge stands.
func runMerged(ctx context.Context, s MergedStage, w io.Writer, repo string, m ext.Merge) {
	if ctx.Err() != nil || len(s.Names) == 0 {
		return
	}
	resolve := s.resolve
	if resolve == nil {
		resolve = ext.Resolve
	}
	plans := resolve(s.Names)
	if !slices.ContainsFunc(plans, func(p ext.Plan) bool { return !p.Skipped() }) {
		return // every name missing
	}
	// Found but not runnable only: no request and no window, just their warn markers.
	var request []byte
	o := s.Ext
	if ext.AnyToRun(plans) {
		timeout := s.timeout
		if timeout <= 0 {
			timeout = ext.Timeouts[ext.StageMerged]
		}
		end := time.Now().Add(timeout)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, end.Add(-proc.DefaultWaitDelay))
		defer cancel()
		// Deadline check 1, before the `.kits` read: past the runner's deadline nothing is read.
		kits := &[]ext.Kit{}
		if ctx.Err() == nil && s.Kits != nil {
			kits = s.Kits(ctx)
		}
		req := ext.Request{Stage: ext.StageMerged, Repo: repo, Kits: kits, Merge: &m}
		var err error
		if request, err = json.Marshal(req); err != nil { // strings and an int; it always encodes
			return
		}
		// Timeout names the stage's window in warn reasons; ctx already ends at the runner's
		// deadline.
		o.Timeout = timeout
	}
	// Deadline check 2: ext.Stream checks ctx before it starts each extension, so an extension
	// not started by then is left unstarted with a warn.
	var results []ext.Result
	ext.Stream(ctx, o, ext.StageMerged, request, plans, func(r ext.Result) {
		results = append(results, r)
	})
	for _, r := range results {
		var b strings.Builder
		if mk := r.Marker(); mk != "" {
			b.WriteString(mk + "\n")
		}
		if c := strings.TrimSpace(r.Context); c != "" {
			b.WriteString(c + "\n")
		}
		if b.Len() > 0 {
			_, _ = io.WriteString(w, b.String())
		}
	}
}

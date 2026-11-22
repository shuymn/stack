package observe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
)

// maxListedChanges bounds the replay-change findings; all are counted in replay.changed.
const maxListedChanges = 20

// prepCheck is how many calls the replay preparation handles between deadline checks.
const prepCheck = 1024

// payload is one unique hook input replayed once and compared with every record that produced it.
// Its request body is built from the newest record only when a worker runs it.
type payload struct {
	records []*toolCall
	result  string
	// late marks a payload cut by the deadline while it ran.
	late bool
}

var errChanged = errors.New("the transcript line changed")

// hookArgs is the argv of both installed PreToolUse hooks.
var hookArgs = []string{"hook", "claude", "pre-tool-use"}

// replay pipes each recorded guard-wired call to the installed hooks, `stack hook claude
// pre-tool-use` and `stack-guard hook claude pre-tool-use` (docs/design/core-stages.md §4.5,
// §4.10), and compares their combined decision with the one recorded in the transcript (design
// §3.3 replay). Without stack-guard on PATH the calls replay through stack only, a warn finding
// names it, and a recorded deny that stack alone no longer makes is not comparable: the rule may
// be one stack-guard holds.
func (s *state) replay(st *Source) error {
	stack, err := s.o.LookPath("stack")
	if err != nil {
		return errOff
	}
	guard, err := s.o.LookPath("stack-guard")
	if err != nil {
		guard = ""
		s.finding(
			kindReplayHookMissing,
			"stack-guard",
			"stack-guard not on PATH: replayed through stack only; a recorded deny stack no longer "+
				"makes is not comparable",
		)
	}
	totals := &s.latest.Replay
	byDigest := map[[sha256.Size]byte]*payload{}
	var order []*payload
	// Newest first, by the full timestamp: the most recent calls are replayed when the cap or the
	// deadline cuts.
	calls := slices.Clone(s.calls)
	slices.SortStableFunc(calls, func(a, b *toolCall) int { return b.ts.Compare(a.ts) })
	cwdOK := map[string]bool{}
	for i, c := range calls {
		if i%prepCheck == 0 && s.work.Err() != nil {
			// The rest is never run: counted as cut by time, like a payload not started.
			for _, rest := range calls[i:] {
				if compared(rest) {
					st.Records++
					totals.TruncatedTime++
				}
			}
			break
		}
		if !compared(c) {
			continue
		}
		st.Records++
		ok, seen := cwdOK[c.cwd]
		if !seen {
			ok = dirExists(c.cwd)
			cwdOK[c.cwd] = ok
		}
		if !ok || !c.inputOK {
			totals.Unrecoverable++
			continue
		}
		p := byDigest[c.digest]
		if p == nil {
			p = &payload{}
			byDigest[c.digest] = p
			order = append(order, p)
		}
		p.records = append(p.records, c)
	}
	run := order[:min(len(order), s.o.replayCap)]
	for _, p := range order[len(run):] {
		totals.TruncatedCap += len(p.records)
	}

	env := append(slices.Clone(s.o.Env), "STACK_DEBUG=1")
	jobs := make(chan *payload)
	var wg sync.WaitGroup
	for range s.o.replayWorkers {
		wg.Go(func() {
			for p := range jobs {
				if s.work.Err() != nil {
					p.late = true
					continue
				}
				body, err := requestBody(p.records[0])
				if err != nil {
					p.result = decNotComparable
					continue
				}
				p.result, p.late = s.replayHooks(stack, guard, env, body)
			}
		})
	}
	for _, p := range run {
		jobs <- p
	}
	close(jobs)
	wg.Wait()

	for _, p := range run {
		s.compare(p, guard == "")
	}
	if totals.TruncatedTime > 0 {
		return fmt.Errorf("replay: %w", context.DeadlineExceeded)
	}
	return nil
}

// compare counts the replayed decision of p against each of its records. guardless marks a replay
// without stack-guard: a recorded deny replayed as no-deny may be a rule only stack-guard holds.
func (s *state) compare(p *payload, guardless bool) {
	totals := &s.latest.Replay
	switch {
	case p.late:
		totals.TruncatedTime += len(p.records)
		return
	case p.result == decNotComparable:
		totals.NotComparable += len(p.records)
		return
	}
	for _, c := range p.records {
		if guardless && p.result == decNoDeny && c.recorded != decNoDeny {
			totals.NotComparable++
			continue
		}
		totals.Compared++
		if p.result == c.recorded {
			continue
		}
		totals.Changed++
		switch {
		case c.recorded == decNoDeny:
			totals.ChangedByKind.NewDeny++
		case p.result == decNoDeny:
			totals.ChangedByKind.LostDeny++
		default:
			totals.ChangedByKind.RuleChanged++
		}
		if totals.Changed <= maxListedChanges {
			s.finding(kindReplayChange, fmt.Sprintf("%s:%d", c.path, c.line),
				fmt.Sprintf("%s: %s -> %s", c.tool, c.recorded, p.result))
		}
	}
}

// replayHooks runs the payload through stack, then stack-guard unless stack denied or guard is
// "". The decision is a deny when either denies, with the rule of the first that denied (the
// settings order); otherwise not comparable when either is. late reports a hook cut by the run
// deadline.
func (s *state) replayHooks(stack, guard string, env []string, body []byte) (string, bool) {
	result := decNoDeny
	for _, name := range []string{stack, guard} {
		if name == "" {
			continue
		}
		out := s.child(command{
			name: name, args: hookArgs, env: env, stdin: body, timeout: s.o.replayTimeout,
		})
		if out.timedOut && s.work.Err() != nil {
			return "", true
		}
		switch d := replayedDecision(out); {
		case strings.HasPrefix(d, denyPrefix):
			return d, false
		case d == decNotComparable:
			result = d
		}
	}
	return result, false
}

// compared: only deny:<rule> and no-deny are compared.
func compared(c *toolCall) bool {
	return c.recorded == decNoDeny || strings.HasPrefix(c.recorded, denyPrefix)
}

func dirExists(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// requestBody re-reads the tool_use of c from its transcript line and builds the hook payload. A
// line whose input no longer has c's digest is not comparable.
func requestBody(c *toolCall) ([]byte, error) {
	input, err := readInput(c)
	if err != nil {
		return nil, err
	}
	if payloadDigest(c.cwd, c.tool, input) != c.digest {
		return nil, errChanged
	}
	body, err := json.Marshal(map[string]any{
		"session_id": c.sessionID, "transcript_path": c.path, "cwd": c.cwd,
		"hook_event_name": "PreToolUse", "tool_name": c.tool, "tool_input": input,
	})
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	return body, nil
}

func readInput(c *toolCall) (json.RawMessage, error) {
	f, err := os.Open(c.path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(f, maxLine+1)).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	var r record
	if err := json.Unmarshal(line, &r); err != nil || r.Message == nil {
		return nil, errChanged
	}
	var blocks []block
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return nil, errChanged
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID == c.id {
			return b.Input, nil
		}
	}
	return nil, errChanged
}

// replayedDecision reads the hook's answer (design §3.3 replay step 2). Output not read in full
// (stderr past its cap, pipes cut) may hide a `stack hook:` line, so it is not comparable.
func replayedDecision(out outcome) string {
	if !out.ok() || out.stderrCut {
		return decNotComparable
	}
	sc := bufio.NewScanner(bytes.NewReader(out.stderr))
	sc.Buffer(nil, maxStderr+1)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "stack hook:") {
			return decNotComparable // the hook failed open
		}
	}
	if sc.Err() != nil {
		return decNotComparable
	}
	if len(bytes.TrimSpace(out.stdout)) == 0 {
		return decNoDeny
	}
	var resp struct {
		Hook struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(out.stdout, &resp) != nil || resp.Hook.Decision != "deny" {
		return decNotComparable
	}
	rule, ok := guardRule(resp.Hook.Reason)
	if !ok {
		return decNotComparable
	}
	return denyPrefix + rule
}

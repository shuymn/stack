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
	// core and guard are the decisions of stack and stack-guard (decMissing when stack-guard is
	// not on PATH).
	core, guard string
	// late marks a payload cut by the deadline while it ran.
	late bool
}

// decMissing is the decision of a hook that is not installed.
const decMissing = "missing"

// coreRules are the rules the core hook holds since v11 (docs/design/core-stages.md §4.5); every
// other rule is stack-guard's.
var coreRules = map[string]bool{"raw-merge": true}

var errChanged = errors.New("the transcript line changed")

// hookArgs is the argv of both installed PreToolUse hooks.
var hookArgs = []string{"hook", "claude", "pre-tool-use"}

// replay pipes each recorded guard-wired call to both installed hooks, `stack hook claude
// pre-tool-use` and `stack-guard hook claude pre-tool-use` (docs/design/core-stages.md §4.5,
// §4.10), and compares their decisions with the one recorded in the transcript (design §3.3
// replay). Without stack-guard on PATH the calls replay through stack only and a warn finding
// names it (judge).
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
			"stack-guard not on PATH: replayed through stack only; a recorded deny of a rule "+
				"stack-guard holds is not comparable",
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
	// Each worker runs the two hooks of a payload at once: at most 2 × replayWorkers children.
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
					p.core, p.guard = decNotComparable, decNotComparable
					continue
				}
				p.core, p.guard, p.late = s.replayHooks(stack, guard, env, body)
			}
		})
	}
	for _, p := range run {
		jobs <- p
	}
	close(jobs)
	wg.Wait()

	for _, p := range run {
		s.compare(p)
	}
	if totals.TruncatedTime > 0 {
		return fmt.Errorf("replay: %w", context.DeadlineExceeded)
	}
	return nil
}

// compare counts the replayed decisions of p against each of its records.
func (s *state) compare(p *payload) {
	totals := &s.latest.Replay
	if p.late {
		totals.TruncatedTime += len(p.records)
		return
	}
	for _, c := range p.records {
		result, ok := judge(c.recorded, p.core, p.guard)
		if !ok {
			totals.NotComparable++
			continue
		}
		totals.Compared++
		if result == c.recorded {
			continue
		}
		totals.Changed++
		switch {
		case c.recorded == decNoDeny:
			totals.ChangedByKind.NewDeny++
		case result == decNoDeny:
			totals.ChangedByKind.LostDeny++
		default:
			totals.ChangedByKind.RuleChanged++
		}
		if totals.Changed <= maxListedChanges {
			s.finding(kindReplayChange, fmt.Sprintf("%s:%d", c.path, c.line),
				fmt.Sprintf("%s: %s -> %s", c.tool, c.recorded, result))
		}
	}
}

// judge returns the replayed decision to compare with recorded, or false when it is not
// comparable. Claude Code runs both hooks and denies when either denies, so a recorded rule
// either hook denies now is unchanged; otherwise the replayed decision is the first deny (core,
// then stack-guard, the settings order) or no-deny. A hook that was not comparable leaves a
// no-deny unknown. A recorded deny no hook made again is judged by the hook holding its rule
// (coreRules): not comparable when that hook was not comparable or is missing. A missing
// stack-guard leaves a recorded no-deny compared through stack alone, named by the
// replay-hook-missing warn.
func judge(recorded, core, guard string) (string, bool) {
	var denies []string
	for _, d := range []string{core, guard} {
		if strings.HasPrefix(d, denyPrefix) {
			denies = append(denies, d)
		}
	}
	if slices.Contains(denies, recorded) {
		return recorded, true
	}
	failed := core == decNotComparable || guard == decNotComparable
	if recorded != decNoDeny {
		holder := guard
		if coreRules[strings.TrimPrefix(recorded, denyPrefix)] {
			holder = core
		}
		if holder == decNotComparable || holder == decMissing {
			return "", false
		}
	}
	if len(denies) > 0 {
		return denies[0], true
	}
	return decNoDeny, !failed
}

// replayHooks runs the payload through stack and stack-guard at once (stack-guard is decMissing
// when guard is ""). late reports a hook cut by the run deadline.
func (s *state) replayHooks(stack, guard string, env []string, body []byte) (string, string, bool) {
	run := func(name string) (string, bool) {
		out := s.child(command{
			name: name, args: hookArgs, env: env, stdin: body, timeout: s.o.replayTimeout,
		})
		if out.timedOut && s.work.Err() != nil {
			return "", true
		}
		return replayedDecision(out), false
	}
	guardDec, guardLate := decMissing, false
	var wg sync.WaitGroup
	if guard != "" {
		wg.Go(func() { guardDec, guardLate = run(guard) })
	}
	core, coreLate := run(stack)
	wg.Wait()
	return core, guardDec, coreLate || guardLate
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

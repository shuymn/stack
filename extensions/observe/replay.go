package observe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
)

// maxListedChanges bounds the replay-change findings; all are counted in replay.changed.
const maxListedChanges = 20

// payload is one unique hook input replayed once and compared with every record that produced it.
type payload struct {
	key     string
	body    []byte
	records []*toolCall
	result  string
	// late marks a payload cut by the deadline while it ran.
	late bool
}

// replay pipes each recorded guard-wired call to the installed `stack hook claude pre-tool-use`
// and compares its decision with the one recorded in the transcript (design §3.3 replay).
func (s *state) replay(st *Source) error {
	stack, err := s.o.LookPath("stack")
	if err != nil {
		return errOff
	}
	totals := &s.latest.Replay
	byKey := map[string]*payload{}
	var order []*payload
	// Newest first: the most recent calls are replayed when the cap or the deadline cuts.
	calls := slices.Clone(s.calls)
	slices.SortStableFunc(calls, func(a, b *toolCall) int { return strings.Compare(b.ts, a.ts) })
	for _, c := range calls {
		if c.recorded != decNoDeny && !strings.HasPrefix(c.recorded, denyPrefix) {
			continue // only deny:<rule> and no-deny are compared
		}
		st.Records++
		if !recoverable(c) {
			totals.Unrecoverable++
			continue
		}
		key := c.cwd + "\x00" + c.tool + "\x00" + compact(c.input)
		p := byKey[key]
		if p == nil {
			body, err := json.Marshal(map[string]any{
				"session_id": c.sessionID, "transcript_path": c.path, "cwd": c.cwd,
				"hook_event_name": "PreToolUse", "tool_name": c.tool, "tool_input": c.input,
			})
			if err != nil {
				totals.Unrecoverable++
				continue
			}
			p = &payload{key: key, body: body}
			byKey[key] = p
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
				out := s.child(command{
					name: stack, args: []string{"hook", "claude", "pre-tool-use"}, env: env,
					stdin: p.body, timeout: s.o.replayTimeout,
				})
				if out.timedOut && s.work.Err() != nil {
					p.late = true
					continue
				}
				p.result = replayedDecision(out)
			}
		})
	}
	for _, p := range run {
		jobs <- p
	}
	close(jobs)
	wg.Wait()

	for _, p := range run {
		if p.late {
			totals.TruncatedTime += len(p.records)
			continue
		}
		for _, c := range p.records {
			if p.result == decNotComparable {
				totals.NotComparable++
				continue
			}
			totals.Compared++
			if p.result == c.recorded {
				continue
			}
			totals.Changed++
			if totals.Changed <= maxListedChanges {
				s.finding(kindReplayChange, fmt.Sprintf("%s:%d", c.path, c.line),
					fmt.Sprintf("%s: %s -> %s", c.tool, c.recorded, p.result))
			}
		}
	}
	if totals.TruncatedTime > 0 {
		return fmt.Errorf("replay: %w", context.DeadlineExceeded)
	}
	return nil
}

// recoverable: the cwd still exists and the input holds what the guard needs for the tool.
func recoverable(c *toolCall) bool {
	if c.cwd == "" {
		return false
	}
	if info, err := os.Stat(c.cwd); err != nil || !info.IsDir() {
		return false
	}
	var in map[string]json.RawMessage
	if json.Unmarshal(c.input, &in) != nil || in == nil {
		return false
	}
	if c.tool == "Bash" {
		var cmd string
		if json.Unmarshal(in["command"], &cmd) != nil || cmd == "" {
			return false
		}
	}
	return true
}

func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// replayedDecision reads the hook's answer (design §3.3 replay step 2).
func replayedDecision(out outcome) string {
	if !out.ok() {
		return decNotComparable
	}
	sc := bufio.NewScanner(bytes.NewReader(out.stderr))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "stack hook:") {
			return decNotComparable // the hook failed open
		}
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
	if json.Unmarshal(out.stdout, &resp) != nil || resp.Hook.Decision != "deny" ||
		!strings.HasPrefix(resp.Hook.Reason, guardPrefix) {
		return decNotComparable
	}
	rule, _, ok := strings.Cut(resp.Hook.Reason[len(guardPrefix):], ")")
	if !ok || rule == "" {
		return decNotComparable
	}
	return denyPrefix + rule
}

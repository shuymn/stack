package improve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	latestSchema = 1
	freshness    = 24 * time.Hour
	futureSlack  = 5 * time.Minute
)

// item is the part of an observe latest.json item improve reads (schema in
// extensions/observe, design §3.3).
type item struct {
	ID         string `json:"id"`
	Signal     string `json:"signal"`
	Where      string `json:"where"`
	Excerpt    string `json:"excerpt"`
	Provenance string `json:"provenance"`
}

// latest is the part of latest.json improve reads.
type latest struct {
	Version *int `json:"version"`
	Window  struct {
		Until string `json:"until"`
	} `json:"window"`
	Sources struct {
		Gh struct {
			Status string `json:"status"`
		} `json:"gh"`
	} `json:"sources"`
	Items []item `json:"items"`
}

// evidence is the result of reading latest.json: either public items or an early answer.
type evidence struct {
	public   []item
	ghStatus string
	// stop is set when there is nothing to attempt; warn tells pass from warn.
	stop string
	warn bool
}

func latestPath(home string) string {
	return filepath.Join(home, ".local", "state", "stack", "observe", "latest.json")
}

// readEvidence applies design §3.4 step 6 to latest.json at now.
func readEvidence(path string, now time.Time) evidence {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return evidence{stop: "no observation (" + path + " is missing)"}
	}
	if err != nil {
		return evidence{stop: "cannot read latest.json", warn: true}
	}
	var l latest
	if err := json.Unmarshal(data, &l); err != nil {
		return evidence{stop: "latest.json is not valid JSON", warn: true}
	}
	if l.Version == nil || *l.Version != latestSchema {
		return evidence{stop: "latest.json has an unknown version", warn: true}
	}
	until, err := time.Parse(time.RFC3339, l.Window.Until)
	if err != nil {
		return evidence{stop: "latest.json window.until is not an RFC 3339 time", warn: true}
	}
	if until.After(now.Add(futureSlack)) {
		return evidence{
			stop: "latest.json window.until " + l.Window.Until + " is in the future",
			warn: true,
		}
	}
	if now.Sub(until) > freshness {
		return evidence{stop: "stale observation (window ends " + l.Window.Until + ")"}
	}
	ev := evidence{ghStatus: l.Sources.Gh.Status}
	seen := map[string]bool{}
	for _, it := range l.Items {
		if it.Provenance == "public" && rePublic.MatchString(it.Where) && it.ID != "" &&
			!seen[it.ID] {
			seen[it.ID] = true
			ev.public = append(ev.public, it)
		}
	}
	if len(ev.public) == 0 {
		ev.stop = "no public evidence"
		if ev.ghStatus != "ok" {
			ev.stop += fmt.Sprintf(" (gh source %s)", orUnknown(ev.ghStatus))
		}
	}
	return ev
}

func orUnknown(s string) string {
	if s == "" {
		return "missing"
	}
	return s
}

// brief is the public evidence brief: id, signal, URL and excerpt of public items only
// (legacy improve.py:589-593).
func brief(public []item) string {
	var b strings.Builder
	b.WriteString("## Evidence (public GitHub items of the last 7 days)\n\n")
	for _, it := range public {
		fmt.Fprintf(&b, "- id %s | %s | %s | %s\n", it.ID, it.Signal, it.Where,
			strings.Join(strings.Fields(it.Excerpt), " "))
	}
	return b.String()
}

package public

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shuymn/stack/internal/gittest"
)

// stageResult decodes a stage answer and checks that it is one valid response.
func stageResult(t *testing.T, r result) response {
	t.Helper()
	var resp response
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &resp) != nil {
		t.Fatalf("stage = %+v, want one response", r)
	}
	return resp
}

func outboundRequest(repo, rng, title, body, branch string) string {
	data, err := json.Marshal(request{
		Stage: "outbound", Repo: repo, Range: rng, Title: title, Body: body, Branch: branch,
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}

// TestStage covers C14: a hit in the range, the title, the body or the branch blocks with one hit
// line and one finding per hit; its own errors block; an empty range scans the texts only; outside
// outbound it passes silently.
func TestStage(t *testing.T) {
	r := checkRepo(t, "fix: path\n\nFound under "+local("Users", "erin")+"work.")
	c1 := r.Git("rev-parse", "--short=9", "HEAD")
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "stack", "private-terms"), []byte("Zebra\n"))
	stage := func(home, req string) response {
		t.Helper()
		return stageResult(t, runMain(t, home, req))
	}
	hit := local("home", "bob")

	for _, tt := range []struct {
		name, req, want string
	}{
		{
			"range", outboundRequest(r.Dir, "base..HEAD", "", "", ""),
			"check public: commit " + c1 + " message: local path '" + local("Users", "erin") + "'",
		},
		{
			"title", outboundRequest(r.Dir, "", "docs: zebra\n", "", ""),
			"check public: title: private term 'Zebra'",
		},
		{
			"body", outboundRequest(r.Dir, "", "", "see\n"+hit+"x\n", ""),
			"check public: body: local path '" + hit + "'",
		},
		{
			"branch", outboundRequest("", "", "", "", "kaizen/Zebra"),
			"check public: branch: private term 'Zebra'",
		},
	} {
		resp := stage(home, tt.req)
		wantFinding := []finding{{Hit: strings.TrimPrefix(tt.want, hitPrefix)}}
		if resp.Status != "block" || resp.Context != tt.want ||
			fmt.Sprint(resp.Findings) != fmt.Sprint(wantFinding) {
			t.Errorf("%s: %+v, want block with %q", tt.name, resp, tt.want)
		}
	}

	clean := outboundRequest(r.Dir, "", "docs: tidy", "Edits home/dot_claude.", "kaizen/x")
	if resp := stage(home, clean); resp.Status != "pass" || resp.Context != "" {
		t.Errorf("clean texts, no range: %+v", resp)
	}
	// Outside outbound nothing is scanned and nothing is said, even with a broken setup.
	for _, s := range []string{"entry", "merged", "cycle"} {
		req := strings.Replace(outboundRequest(r.Dir, "", "zebra", "", ""), "outbound", s, 1)
		if resp := stage("", req); resp.Status != "pass" || resp.Context != "" {
			t.Errorf("stage %s: %+v", s, resp)
		}
	}

	errorCases := []struct {
		name  string
		setup func(home string)
		req   string
		want  string
	}{
		{"unreadable terms", func(home string) {
			writeFile(t, filepath.Join(home, ".config", "stack", "private-terms"), []byte("\xff\n"))
		}, clean, "not UTF-8"},
		{"broken safety.toml", func(home string) {
			writeFile(t, filepath.Join(home, ".config", "stack", "safety.toml"),
				[]byte("private_terms = [\n"))
		}, clean, "safety.toml"},
		{
			"unresolvable range", func(string) {}, outboundRequest(r.Dir, "base..nope", "", "", ""),
			"git failed",
		},
		{
			"not a range", func(string) {}, outboundRequest(r.Dir, "HEAD", "", "", ""),
			"RANGE must be",
		},
		{"invalid request", func(string) {}, `{"stage":`, "cannot read the request"},
	}
	for _, tt := range errorCases {
		h := t.TempDir()
		tt.setup(h)
		resp := stage(h, tt.req)
		if resp.Status != "block" || !strings.HasPrefix(resp.Context, "stack-public: ") ||
			!strings.Contains(resp.Context, tt.want) || len(resp.Findings) != 1 ||
			resp.Findings[0].Error != resp.Context {
			t.Errorf("%s: %+v, want block with %q", tt.name, resp, tt.want)
		}
	}
	if resp := stage("", clean); resp.Status != "block" ||
		!strings.Contains(resp.Context, "home directory unknown") {
		t.Errorf("no home: %+v", resp)
	}
}

// largeRange commits a file whose distinct local paths make over 1 MiB of hit lines.
func largeRange(t *testing.T) (*gittest.Repo, int) {
	t.Helper()
	r := gittest.New(t)
	r.Commit("chore: base")
	r.Tag("base")
	const n = 6000
	var b strings.Builder
	pad := strings.Repeat("x", 180)
	for i := range n {
		b.WriteString("see " + local("home", fmt.Sprintf("u%05d%s", i, pad)) + "\n")
	}
	r.Write("paths.md", b.String())
	r.Commit("docs: paths")
	return r, n
}

// TestStageLarge covers C14's cap: a range whose distinct hits make over 1 MiB of diagnostics gets
// a valid block under 512 KiB, its hits in scan order and the `N more hits` trailer; the real
// `stack run outbound --ext public` exits 1 on it, and `stack-public check` prints every hit.
func TestStageLarge(t *testing.T) {
	needBuilt(t)
	r, n := largeRange(t)
	home := t.TempDir()
	req := outboundRequest(r.Dir, "base..HEAD", "", "", "")
	out := runBinary(t, r, home, r.Dir, req, built.public)
	if len(out.stdout) >= maxResponse {
		t.Fatalf("response of %d bytes, want under %d", len(out.stdout), maxResponse)
	}
	resp := stageResult(t, out)
	lines := strings.Split(resp.Context, "\n")
	trailer := lines[len(lines)-1]
	kept := lines[:len(lines)-1]
	more, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(trailer, hitPrefix),
		" more hits"))
	if resp.Status != "block" || err != nil || more == 0 || len(kept)+more != n ||
		len(resp.Findings) != len(kept) {
		t.Fatalf("block of %d lines, trailer %q, %d findings; want %d hits in all",
			len(kept), trailer, len(resp.Findings), n)
	}
	if want := "u00000"; !strings.Contains(kept[0], want) ||
		!strings.Contains(kept[len(kept)-1], fmt.Sprintf("u%05d", len(kept)-1)) {
		t.Errorf("hits not in scan order: first %q, last %q", kept[0], kept[len(kept)-1])
	}

	run := runBinary(t, r, home, r.Dir, "", built.stack, "run", "outbound", "--ext", "public",
		"--range", "base..HEAD")
	if run.code != 1 || !strings.Contains(run.stdout, `"status":"block"`) ||
		!strings.Contains(run.stdout, strconv.Itoa(more)+" more hits") {
		t.Fatalf("stack run outbound = code %d, stderr %q, stdout %d bytes", run.code, run.stderr,
			len(run.stdout))
	}

	check := runBinary(t, r, home, r.Dir, "", built.public, "check", "base..HEAD")
	if check.code != exitRefused || strings.Count(check.stderr, "\ncheck public: ")+1 != n ||
		len(check.stderr) <= 1<<20 {
		t.Fatalf("stack-public check = code %d, %d hit lines in %d bytes; want all %d",
			check.code, strings.Count(check.stderr, "check public: "), len(check.stderr), n)
	}
}

// TestHitsCut checks the cap's pieces: a hit line over 1,000 bytes is cut on a UTF-8 boundary, and
// a block with an error keeps the error line before the trailer.
func TestHitsCut(t *testing.T) {
	long := hitPrefix + "body: private term '" + strings.Repeat("\u79d8", 400) + "'"
	h := &hits{}
	if _, err := h.Write([]byte(long + "\n" + hitPrefix + "branch: local path 'x'\n")); err != nil {
		t.Fatal(err)
	}
	if len(h.lines) != 2 || len(h.lines[0]) > maxLine || !utf8.ValidString(h.lines[0]) ||
		!strings.HasPrefix(long, h.lines[0]) || len(h.lines[0]) < maxLine-utf8.UTFMax {
		t.Fatalf("lines = %d, first of %d bytes", len(h.lines), len(h.lines[0]))
	}
	h.more = 3
	resp := h.block(os.ErrDeadlineExceeded)
	lines := strings.Split(resp.Context, "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[2], "stack-public: ") ||
		lines[3] != hitPrefix+"3 more hits" || len(resp.Findings) != 3 {
		t.Errorf("block = %+v", resp)
	}
}

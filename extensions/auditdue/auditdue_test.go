package auditdue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// childEnv makes the test binary act as stack-audit-due: it answers the request on stdin with the
// home directory childHome names, for the two-process test.
const (
	childEnv  = "AUDITDUE_TEST_CHILD"
	childHome = "AUDITDUE_TEST_HOME"
)

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		if err := json.NewEncoder(os.Stdout).
			Encode(Respond(os.Stdin, os.Getenv(childHome))); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// newKit makes a kit dir with the given files and returns it.
func newKit(t *testing.T, slug string, files map[string]string) Kit {
	t.Helper()
	dir := filepath.Join(t.TempDir(), slug)
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		write(t, filepath.Join(dir, name), body)
	}
	return Kit{Slug: slug, Dir: dir}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newHome makes a home dir, with audit-due.toml holding cfg unless cfg is "".
func newHome(t *testing.T, cfg string) string {
	t.Helper()
	home := t.TempDir()
	if cfg != "" {
		dir := filepath.Join(home, ".config", "stack")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "audit-due.toml"), cfg)
	}
	return home
}

func merge(home, oid string, kits ...Kit) Response {
	return Run(Request{Kits: kits, Merge: &Merge{PR: 1, Head: "h", OID: oid}}, home)
}

func stored(t *testing.T, k Kit) state {
	t.Helper()
	s, ok, err := load(filepath.Join(k.Dir, StateFile))
	if err != nil || !ok {
		t.Fatalf("state file of %s: ok=%v err=%v", k.Slug, ok, err)
	}
	return s
}

func dueK(n int, slug string) string {
	return fmt.Sprintf(
		"audit due (%d merges since the last due signal): run steward on kit %s",
		n,
		slug,
	)
}

const dueState = "audit due (merged without a state update since the previous merge): " +
	"run steward on kit demo"

func wantResp(t *testing.T, got Response, status, context string) {
	t.Helper()
	if got.Status != status || got.Context != context {
		t.Fatalf("got status %q context %q findings %v; want status %q context %q",
			got.Status, got.Context, got.Findings, status, context)
	}
	if status == StatusPass && len(got.Findings) != 0 {
		t.Fatalf("pass with findings %v", got.Findings)
	}
}

// snapshot is every file under dir with its bytes.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // G122: a test's own temp dir.
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		files[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sameFiles(t *testing.T, before, after map[string]string) {
	t.Helper()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("files changed:\nbefore %v\nafter  %v", before, after)
	}
}

// C8: without a merge field or with empty kits, pass with empty context and no file touched.
func TestSilent(t *testing.T) {
	existing := `{"merges":3,"state":"x","oid":"o"}` + "\n"
	cases := []struct {
		name, request string
		listed        bool
	}{
		{"no merge field", `{"stage":"merged","repo":"","range":"","prompt":"","kits":%s}`, true},
		{"null merge", `{"stage":"merged","kits":%s,"merge":null}`, true},
		{
			"empty kits",
			`{"stage":"merged","kits":[],"merge":{"pr":1,"head":"h","oid":"o2"}}`,
			false,
		},
		{"no kits", `{"stage":"merged","merge":{"pr":1,"head":"h","oid":"o2"}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := newHome(t, "merges = 1\n")
			withFile := newKit(t, "a", map[string]string{"state.md": "s", StateFile: existing})
			fresh := newKit(t, "b", map[string]string{"state.md": "s"})
			req := c.request
			if c.listed {
				kits, err := json.Marshal([]Kit{withFile, fresh})
				if err != nil {
					t.Fatal(err)
				}
				req = fmt.Sprintf(req, kits)
			}
			roots := []string{home, withFile.Dir, fresh.Dir}
			before := make([]map[string]string, len(roots))
			for i, r := range roots {
				before[i] = snapshot(t, r)
			}
			wantResp(t, Respond(strings.NewReader(req), home), StatusPass, "")
			for i, r := range roots {
				sameFiles(t, before[i], snapshot(t, r))
			}
		})
	}
}

// C9: K merges since the last due line print one line and reset the count.
func TestK(t *testing.T) {
	// edit changes the kit state so that only the count condition can fire.
	edit := func(t *testing.T, k Kit, oid string) {
		t.Helper()
		write(t, filepath.Join(k.Dir, "state.md"), "state before "+oid)
	}
	run := func(t *testing.T, home string, merges int, k Kit) []Response {
		t.Helper()
		var out []Response
		for i := 1; i <= merges; i++ {
			oid := fmt.Sprintf("oid%d", i)
			edit(t, k, oid)
			out = append(out, merge(home, oid, k))
		}
		return out
	}

	t.Run("default", func(t *testing.T) {
		home := newHome(t, "")
		k := newKit(t, "demo", nil)
		got := run(t, home, 6, k)
		for i, r := range got {
			want := ""
			if i == 4 {
				want = dueK(5, "demo")
			}
			wantResp(t, r, StatusPass, want)
		}
		if s := stored(t, k); s.Merges != 1 || s.OID != "oid6" {
			t.Fatalf("state after 6 merges = %+v", s)
		}
	})

	t.Run("reset written", func(t *testing.T) {
		home := newHome(t, "merges = 2\n")
		k := newKit(t, "demo", nil)
		got := run(t, home, 2, k)
		wantResp(t, got[0], StatusPass, "")
		wantResp(t, got[1], StatusPass, dueK(2, "demo"))
		h, err := hash(k.Dir)
		if err != nil {
			t.Fatal(err)
		}
		if s := stored(t, k); s != (state{Merges: 0, State: h, OID: "oid2"}) {
			t.Fatalf("state after due = %+v, want {0 %s oid2}", s, h)
		}
	})

	t.Run("K = 1 on the first merge", func(t *testing.T) {
		home := newHome(t, "merges = 1\n")
		k := newKit(t, "demo", nil)
		for _, r := range run(t, home, 3, k) {
			wantResp(t, r, StatusPass, dueK(1, "demo"))
		}
	})

	for _, cfg := range []string{
		"merges = 0\n",
		"merges = -3\n",
		"merges = \"2\"\n",
		"merges = 2.5\n",
		"merges = 99999999999\n",
		"merges = [\n",
	} {
		t.Run("invalid "+strings.TrimSpace(cfg), func(t *testing.T) {
			home := newHome(t, cfg)
			k := newKit(t, "demo", nil)
			for i, r := range run(t, home, 5, k) {
				want := ""
				if i == 4 {
					want = dueK(5, "demo")
				}
				wantResp(t, r, StatusWarn, want)
				if len(r.Findings) != 1 || r.Findings[0].Kind != kindConfig ||
					!strings.Contains(r.Findings[0].Detail, "using merges = 5") {
					t.Fatalf("findings = %v, want one config finding", r.Findings)
				}
			}
		})
	}

	t.Run("no merges key", func(t *testing.T) {
		home := newHome(t, "other = 1\n")
		k := newKit(t, "demo", nil)
		got := run(t, home, 5, k)
		wantResp(t, got[3], StatusPass, "")
		wantResp(t, got[4], StatusPass, dueK(5, "demo"))
	})

	t.Run("two kits", func(t *testing.T) {
		home := newHome(t, "merges = 2\n")
		a := newKit(t, "a", nil)
		b := newKit(t, "b", nil)
		step := func(oid string, kits ...Kit) Response {
			for _, k := range kits {
				edit(t, k, oid)
			}
			return merge(home, oid, kits...)
		}
		wantResp(t, step("o1", a, b), StatusPass, "")
		// Both due at once: one line each, in kits order.
		wantResp(t, step("o2", b, a), StatusPass, dueK(2, "b")+"\n"+dueK(2, "a"))
		// Separate counts: only a advances, then only a is due.
		wantResp(t, step("o3", a), StatusPass, "")
		wantResp(t, step("o4", a, b), StatusPass, dueK(2, "a"))
		if s := stored(t, b); s.Merges != 1 {
			t.Fatalf("b count = %d, want 1", s.Merges)
		}
	})

	t.Run("two processes at once", func(t *testing.T) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		home := newHome(t, "merges = 1000\n")
		k := newKit(t, "demo", nil)
		edit(t, k, "seed")
		wantResp(t, merge(home, "seed", k), StatusPass, "")
		for round := range 5 {
			start := stored(t, k).Merges
			edit(t, k, strconv.Itoa(round))
			lines := twoAtOnce(t, exe, home, k, round)
			data, err := os.ReadFile(filepath.Join(k.Dir, StateFile))
			if err != nil {
				t.Fatal(err)
			}
			var s state
			if err := json.Unmarshal(data, &s); err != nil {
				t.Fatalf("round %d: state file not valid JSON: %q", round, data)
			}
			// Overlapping reads count the pair once; one after the other, the second merge
			// follows the first without a state update and is due by that rule, a reset.
			switch {
			case s.Merges == start+1 && len(lines) == 0:
			case s.Merges == 0 && len(lines) == 1 && lines[s.OID] == dueState:
			default:
				t.Fatalf("round %d: from count %d to %+v with lines %v", round, start, s, lines)
			}
		}
		entries, err := os.ReadDir(k.Dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != StateFile && e.Name() != "state.md" {
				t.Fatalf("leftover file %s", e.Name())
			}
		}
	})
}

// twoAtOnce runs two child processes on one kit with different merge OIDs, both started before
// either gets its request, and returns the due lines by merge OID.
func twoAtOnce(t *testing.T, exe, home string, k Kit, round int) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type child struct {
		cmd *exec.Cmd
		in  *os.File
		out bytes.Buffer
	}
	children := make([]*child, 2)
	oid := func(i int) string { return fmt.Sprintf("r%d-%d", round, i) }
	for i := range children {
		c := &child{cmd: exec.CommandContext(ctx, exe, "-test.run=^$")}
		c.cmd.Env = append(os.Environ(), childEnv+"=1", childHome+"="+home)
		c.cmd.Stdout = &c.out
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		c.cmd.Stdin = r
		c.in = w
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		children[i] = c
	}
	for i, c := range children {
		req, err := json.Marshal(Request{
			Kits:  []Kit{k},
			Merge: &Merge{PR: i, Head: "h", OID: oid(i)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.in.Write(req); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range children {
		_ = c.in.Close()
	}
	lines := map[string]string{}
	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("child: %v", err)
		}
		var resp Response
		if err := json.Unmarshal(c.out.Bytes(), &resp); err != nil {
			t.Fatalf("child response %q: %v", c.out.String(), err)
		}
		// Windows may refuse a rename over a file the other process has open; that is a
		// warn for this process while the other still counts.
		if resp.Status != StatusPass && (runtime.GOOS != "windows" || resp.Status != StatusWarn) {
			t.Fatalf("child response %+v", resp)
		}
		if resp.Context != "" {
			lines[oid(i)] = resp.Context
		}
	}
	return lines
}

// C10: a merge after the previous counted merge without a kit state change prints one line; the
// first merge of a kit does not.
func TestStateUnchanged(t *testing.T) {
	cases := []struct {
		name  string
		files []string
	}{
		{"state.md only", []string{"state.md"}},
		{"NORTH.md only", []string{"NORTH.md"}},
		{"both", []string{"NORTH.md", "state.md"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := newHome(t, "merges = 100\n")
			files := map[string]string{}
			for _, f := range c.files {
				files[f] = "v0 " + f
			}
			k := newKit(t, "demo", files)
			stateFile := filepath.Join(k.Dir, StateFile)

			// First merge: silent, count starts at 1.
			wantResp(t, merge(home, "o1", k), StatusPass, "")
			if s := stored(t, k); s.Merges != 1 || s.OID != "o1" {
				t.Fatalf("first merge state = %+v", s)
			}
			// The same OID again is not counted.
			before, err := os.ReadFile(stateFile)
			if err != nil {
				t.Fatal(err)
			}
			wantResp(t, merge(home, "o1", k), StatusPass, "")
			after, err := os.ReadFile(stateFile)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("same OID changed the state file: %q → %q", before, after)
			}
			// No state update since o1: due, reset.
			wantResp(t, merge(home, "o2", k), StatusPass, dueState)
			if s := stored(t, k); s.Merges != 0 || s.OID != "o2" {
				t.Fatalf("state after due = %+v", s)
			}
			// Each kit state file edited on its own (changed bytes) keeps it silent.
			for i, f := range c.files {
				write(t, filepath.Join(k.Dir, f), "v1 "+f)
				wantResp(t, merge(home, fmt.Sprintf("e%d", i), k), StatusPass, "")
			}
			// A file appearing is a change too.
			write(t, filepath.Join(k.Dir, "NORTH.md"), "added or changed")
			wantResp(t, merge(home, "o3", k), StatusPass, "")
			wantResp(t, merge(home, "o4", k), StatusPass, dueState)
		})
	}

	t.Run("first merge of a kit with a state", func(t *testing.T) {
		home := newHome(t, "merges = 100\n")
		k := newKit(t, "demo", map[string]string{"state.md": "s", "NORTH.md": "n"})
		wantResp(t, merge(home, "o1", k), StatusPass, "")
	})
}

func TestHash(t *testing.T) {
	k := newKit(t, "demo", map[string]string{"state.md": "body\x00x", "other.md": "ignored"})
	got, err := hash(k.Dir)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("NORTH.md\x00absent\x00state.md\x00body\x00x\x00"))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}

// C11: a corrupt or unwritable state file warns with one finding for that kit; the other kits
// are still processed.
func TestErrors(t *testing.T) {
	for _, corrupt := range []string{
		"{not json",
		"",
		"null",
		"[]",
		`{"merges":-1,"state":"","oid":""}`,
		`{"merges":"1","state":"","oid":""}`,
		`{"merges":1.5,"state":"","oid":""}`,
		`{"merges":1,"state":""}`,
		`{"merges":1,"state":"","oid":""} {}`,
	} {
		t.Run("corrupt "+corrupt, func(t *testing.T) {
			home := newHome(t, "")
			k := newKit(t, "demo", map[string]string{"state.md": "s", StateFile: corrupt})
			got := merge(home, "o1", k)
			wantResp(t, got, StatusWarn, "")
			if len(got.Findings) != 1 || got.Findings[0].Where != "demo" ||
				!strings.Contains(got.Findings[0].Detail, "kit demo") {
				t.Fatalf("findings = %v, want one naming kit demo", got.Findings)
			}
			if s := stored(t, k); s.Merges != 1 || s.OID != "o1" {
				t.Fatalf("replaced state = %+v, want a fresh count of 1", s)
			}
			// Replaced: no corrupt warn next time; due by the state rule.
			wantResp(t, merge(home, "o2", k), StatusPass, dueState)
		})
	}

	broken := map[string]func(t *testing.T) Kit{
		"kit dir missing": func(t *testing.T) Kit {
			t.Helper()
			return Kit{Slug: "broken", Dir: filepath.Join(t.TempDir(), "gone")}
		},
		"state file is a directory": func(t *testing.T) Kit {
			t.Helper()
			k := newKit(t, "broken", nil)
			if err := os.Mkdir(filepath.Join(k.Dir, StateFile), 0o750); err != nil {
				t.Fatal(err)
			}
			return k
		},
		"kit dir is a file": func(t *testing.T) Kit {
			t.Helper()
			path := filepath.Join(t.TempDir(), "file")
			write(t, path, "x")
			return Kit{Slug: "broken", Dir: path}
		},
		"no dir": func(*testing.T) Kit { return Kit{Slug: "broken"} },
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		broken["kit dir read-only"] = func(t *testing.T) Kit {
			t.Helper()
			k := newKit(t, "broken", map[string]string{
				StateFile: `{"merges":1,"state":"x","oid":"o0"}`,
			})
			if err := os.Chmod(k.Dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(k.Dir, 0o700) })
			return k
		}
	}
	names := make([]string, 0, len(broken))
	for name := range broken {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			home := newHome(t, "merges = 1\n")
			bad := broken[name](t)
			good := newKit(t, "good", nil)
			got := merge(home, "o1", bad, good)
			wantResp(t, got, StatusWarn, dueK(1, "good"))
			if len(got.Findings) != 1 || got.Findings[0].Kind != kindKit ||
				got.Findings[0].Where != "broken" ||
				!strings.Contains(got.Findings[0].Detail, "kit broken") {
				t.Fatalf("findings = %v, want one naming kit broken", got.Findings)
			}
			if bad.Dir != "" {
				entries, _ := os.ReadDir(bad.Dir)
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".audit-due-") {
						t.Fatalf("leftover temp file %s", e.Name())
					}
				}
			}
		})
	}

	t.Run("invalid request", func(t *testing.T) {
		got := Respond(strings.NewReader("{"), t.TempDir())
		if got.Status != StatusWarn || len(got.Findings) != 1 ||
			got.Findings[0].Kind != kindRequest {
			t.Fatalf("got %+v", got)
		}
	})
}

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/kit"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestMain(m *testing.M) {
	proctest.Main()
	os.Exit(m.Run())
}

// fixture is a fake home with config defaults; nothing touches the real home.
type fixture struct {
	t    *testing.T
	home string
	cfg  config.Config
	env  map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, home: t.TempDir(), cfg: config.Defaults(), env: map[string]string{}}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) kitDir(slug string) string {
	return filepath.Join(f.home, ".local", "state", "stack", "kits", slug)
}

// testID is the session id the fixtures open kits under.
const testID = "s1"

// open records slug for testID, as `stack kit open` does, and writes the kit's files.
func (f *fixture) open(slug string, files map[string]string) {
	f.t.Helper()
	if err := (kit.Store{Home: f.home}).Add(context.Background(), testID, slug); err != nil {
		f.t.Fatal(err)
	}
	dir := f.kitDir(slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	for name, content := range files {
		write(f.t, filepath.Join(dir, filepath.FromSlash(name)), content)
	}
}

func (f *fixture) close(slug string) {
	f.t.Helper()
	if err := (kit.Store{Home: f.home}).Remove(context.Background(), testID, slug); err != nil {
		f.t.Fatal(err)
	}
}

// rulesPath is the default worker body under the fixture home.
func (f *fixture) rulesPath() string {
	return filepath.Join(f.home, ".claude", "agents", "opus-worker.md")
}

func (f *fixture) rules(content string) {
	f.t.Helper()
	write(f.t, f.rulesPath(), content)
}

// transcript writes lines as a JSONL transcript and returns its path.
func (f *fixture) transcript(lines ...string) string {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "transcript.jsonl")
	write(f.t, path, strings.Join(lines, "\n")+"\n")
	return path
}

// userLine is a transcript record whose content is the JSON string of prompt.
func userLine(prompt string) string {
	type message struct {
		Content string `json:"content"`
	}
	b, err := json.Marshal(struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}{"user", message{prompt}})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// start runs the hook and returns its additionalContext ("" for no output) and swallowed errors.
func (f *fixture) start(in hookio.Input) (string, []error) {
	f.t.Helper()
	out, errs := Start(f.t.Context(), Params{
		Input:  in,
		Home:   f.home,
		Getenv: func(k string) string { return f.env[k] },
		Config: f.cfg,
		Runner: proc.Exec{},
	})
	if out == nil {
		return "", errs
	}
	var doc struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		f.t.Fatalf("hook output %q: %v", out, err)
	}
	if doc.HookSpecificOutput.HookEventName != "SessionStart" {
		f.t.Fatalf("hook output %q: wrong event", out)
	}
	ctx := doc.HookSpecificOutput.AdditionalContext
	if n := utf8.RuneCountInString(ctx); n > Budget {
		f.t.Fatalf("context is %d chars, over %d", n, Budget)
	}
	return ctx, errs
}

func compactIn(id string) hookio.Input {
	return hookio.Input{SessionID: id, Source: "compact", HookEventName: "SessionStart"}
}

// mustContain fails unless s holds every want, in order.
func mustContain(t *testing.T, s string, want ...string) {
	t.Helper()
	last := -1
	for _, w := range want {
		i := strings.Index(s, w)
		if i < 0 || i <= last {
			t.Fatalf("context lacks %q in order:\n%s", squash(w), squash(s))
		}
		last = i
	}
}

func mustLack(t *testing.T, s string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(s, u) {
			t.Fatalf("context holds %q:\n%s", u, squash(s))
		}
	}
}

// squash shortens runs of one repeated character for failure messages.
func squash(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); {
		j := i
		for j < len(rs) && rs[j] == rs[i] {
			j++
		}
		if j-i > 8 {
			fmt.Fprintf(&b, "%c{%d}", rs[i], j-i)
		} else {
			b.WriteString(string(rs[i:j]))
		}
		i = j
	}
	return b.String()
}

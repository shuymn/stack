package cli

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestBudgetStatus covers P1 and P19 at the command level: no config and no kits print "no kits",
// an oversized state file still exits 0, `[kit].root` is honoured and the session's kits are marked.
func TestBudgetStatus(t *testing.T) {
	home := t.TempDir()
	if r := runIn(t, home, nil, "budget", "status"); r.code != exitOK || r.stdout != "no kits\n" ||
		r.stderr != "" {
		t.Fatalf("fresh home = %+v", r)
	}

	root := filepath.Join(t.TempDir(), "kits")
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
		[]byte("version = 1\n[kit]\nroot = "+strconv.Quote(root)+"\n"))
	env := map[string]string{"STACK_SESSION_ID": "s1"}
	if r := runIn(t, home, env, "kit", "open", "demo"); r.code != exitOK {
		t.Fatalf("kit open = %+v", r)
	}
	writeFile(t, filepath.Join(root, "demo", "state.md"), []byte(strings.Repeat("x", 5000)))

	r := runIn(t, home, env, "budget", "status")
	want := "kit root: " + root + "\n" +
		"  kit   NORTH.md  state.md  all\n" +
		"* demo  -         5000      0\n" +
		"* open in this session\n" +
		"note: demo/state.md is 5000 bytes, over 3000\n"
	if r.code != exitOK || r.stdout != want || r.stderr != "" {
		t.Errorf("oversized = %+v\nwant %q", r, want)
	}

	// An invalid session id only costs the marks and the legend.
	r = runIn(t, home, map[string]string{"STACK_SESSION_ID": "a/b"}, "budget", "status")
	want = "kit root: " + root + "\n" +
		"  kit   NORTH.md  state.md  all\n" +
		"  demo  -         5000      0\n" +
		"note: demo/state.md is 5000 bytes, over 3000\n"
	if r.code != exitOK || r.stdout != want ||
		!strings.Contains(r.stderr, "session kits are not marked") {
		t.Errorf("invalid id = %+v\nwant %q", r, want)
	}

	// A kit root that cannot be listed is shown with its reason, exit 0.
	file := filepath.Join(t.TempDir(), "kits")
	writeFile(t, file, []byte("not a dir"))
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
		[]byte("version = 1\n[kit]\nroot = "+strconv.Quote(file)+"\n"))
	r = runIn(t, home, nil, "budget", "status")
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "kit root: "+file+"\ncannot list kits: ") {
		t.Errorf("unlistable root = %+v", r)
	}

	// A broken config is a warning; the default kit root is shown, exit 0.
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"), []byte("version = \n"))
	writeFile(t, filepath.Join(home, ".local", "state", "stack", "kits", "dflt", "NORTH.md"),
		[]byte("n\n"))
	r = runIn(t, home, nil, "budget", "status")
	want = "kit root: " + filepath.Join(home, ".local", "state", "stack", "kits") + "\n" +
		"  kit   NORTH.md  state.md  all\n" +
		"  dflt  2         -         0\n"
	if r.code != exitOK || r.stdout != want ||
		!strings.Contains(r.stderr, "using the default kit root") {
		t.Errorf("broken config = %+v\nwant %q", r, want)
	}
}

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gofrs/flock"
	"github.com/shuymn/stack/internal/paths"
)

// lockHolderEnv makes the re-executed test binary hold a `.kits` lock (TestMain).
const lockHolderEnv = "STACK_CLI_TEST_HOLD_LOCK"

func TestMain(m *testing.M) {
	if path := os.Getenv(lockHolderEnv); path != "" {
		os.Exit(holdLock(path))
	}
	os.Exit(m.Run())
}

// holdLock takes the lock at path, prints "locked" and holds it until stdin closes.
func holdLock(path string) int {
	lock := flock.New(path)
	if err := lock.Lock(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_, _ = fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// tree lists every path under root, slash-separated and relative, directories with a trailing /.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func kitsFile(home string) string { return paths.SessionKitsFile(home, "demo-1") }

func readKits(t *testing.T, home string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(kitsFile(home))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// TestKitOpenClose covers C16 at the command level.
func TestKitOpenClose(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	withID := map[string]string{"STACK_SESSION_ID": "demo-1"}
	kitDir := filepath.Join(home, ".local", "state", "stack", "kits", "demo")

	r := runIn(t, home, withID, "kit", "open", "demo")
	if r.code != exitOK || r.stderr != "" ||
		r.stdout != "[new kit: "+kitDir+"; write state.md here]\n" {
		t.Fatalf("open = %+v", r)
	}
	if info, err := os.Stat(kitDir); err != nil || !info.IsDir() {
		t.Fatalf("kit dir not created: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("kit dir mode = %v, want 0700", info.Mode().Perm())
	}

	if err := os.WriteFile(
		filepath.Join(kitDir, "state.md"),
		[]byte("the state\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	r = runIn(t, home, withID, "kit", "open", "demo")
	if r.code != exitOK || !strings.Contains(r.stdout, "the state") {
		t.Fatalf("second open = %+v", r)
	}
	if r := runIn(t, home, withID, "kit", "open", "other"); r.code != exitOK {
		t.Fatalf("open other = %+v", r)
	}
	if got, _ := readKits(t, home); got != "demo\nother\n" {
		t.Fatalf(".kits = %q, want demo once, then other", got)
	}

	for range 2 { // idempotent
		if r := runIn(
			t,
			home,
			withID,
			"kit",
			"close",
			"demo",
		); r.code != exitOK ||
			r.stdout != "" ||
			r.stderr != "" {
			t.Fatalf("close = %+v", r)
		}
	}
	if got, _ := readKits(t, home); got != "other\n" {
		t.Fatalf(".kits after close = %q", got)
	}
	if r := runIn(t, home, withID, "kit", "close", "other"); r.code != exitOK {
		t.Fatalf("close other = %+v", r)
	}
	if _, ok := readKits(t, home); ok {
		t.Fatal("emptied .kits still exists")
	}
	if _, err := os.Stat(filepath.Join(kitDir, "state.md")); err != nil {
		t.Fatalf("close touched the kit dir: %v", err)
	}

	want := []string{
		".local/", ".local/state/", ".local/state/stack/",
		".local/state/stack/kits/", ".local/state/stack/kits/demo/",
		".local/state/stack/kits/demo/state.md", ".local/state/stack/kits/other/",
		".local/state/stack/sessions/", ".local/state/stack/sessions/demo-1.lock",
	}
	if got := tree(t, home); !slices.Equal(got, want) {
		t.Fatalf("home tree = %q, want %q", got, want)
	}
}

func TestKitOpenWithoutSessionID(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	r := runIn(t, home, nil, "kit", "open", "demo")
	kitDir := filepath.Join(home, ".local", "state", "stack", "kits", "demo")
	if r.code != exitOK || r.stdout != "[new kit: "+kitDir+"; write state.md here]\n" ||
		r.stderr != "stack: STACK_SESSION_ID unset; demo not recorded and will not be restored "+
			"after compaction\n" {
		t.Fatalf("open without id = %+v", r)
	}
	if _, err := os.Stat(paths.SessionsDir(home)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open without id recorded something: %v", err)
	}

	r = runIn(t, home, nil, "kit", "close", "demo")
	if r.code != exitUsage || !strings.Contains(r.stderr, "STACK_SESSION_ID unset") {
		t.Fatalf("close without id = %+v", r)
	}
}

func TestKitInvalidInput(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	withID := map[string]string{"STACK_SESSION_ID": "demo-1"}
	for _, slug := range []string{"demo.", "con.txt", "..", "../x", "Demo"} {
		for _, sub := range []string{"open", "close"} {
			r := runIn(t, home, withID, "kit", sub, slug)
			if r.code != exitUsage || r.stdout != "" ||
				!strings.Contains(r.stderr, "invalid kit slug") {
				t.Errorf("kit %s %q = %+v, want exit 2", sub, slug, r)
			}
		}
	}
	for _, id := range []string{"../x", "a b"} {
		r := runIn(t, home, map[string]string{"STACK_SESSION_ID": id}, "kit", "open", "demo")
		if r.code != exitUsage || r.stdout != "" ||
			!strings.Contains(r.stderr, "invalid STACK_SESSION_ID") {
			t.Errorf("open with id %q = %+v, want exit 2", id, r)
		}
	}
	if got := tree(t, home); len(got) != 0 {
		t.Fatalf("invalid input wrote %q", got)
	}
}

func TestKitOpenConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := paths.ConfigFile(home)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		cfg,
		[]byte("[kit]\nroot = \"~/k\"\n[handover]\nX = \"~/h.md\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "h.md"), []byte("HANDOVER"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runIn(t, home, map[string]string{"X": "1"}, "kit", "open", "demo")
	if r.code != exitOK || !strings.Contains(r.stdout, filepath.Join(home, "k", "demo")) ||
		!strings.Contains(r.stdout, "HANDOVER") {
		t.Fatalf("open with config = %+v", r)
	}

	if err := os.WriteFile(cfg, []byte("[kit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runIn(t, home, nil, "kit", "open", "demo")
	if r.code != exitUsage || r.stdout != "" || !strings.Contains(r.stderr, cfg) {
		t.Fatalf("open with broken config = %+v", r)
	}
}

// TestKitLocked holds the `.kits` lock in a separate process past the 5 s timeout.
func TestKitLocked(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	withID := map[string]string{"STACK_SESSION_ID": "demo-1"}
	if r := runIn(t, home, withID, "kit", "open", "a"); r.code != exitOK {
		t.Fatalf("open a = %+v", r)
	}
	before, _ := readKits(t, home)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(exe, "-test.run=^$")
	holder.Env = append(os.Environ(), lockHolderEnv+"="+paths.SessionLockFile(home, "demo-1"))
	stdin, _ := holder.StdinPipe()
	stdout, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = holder.Wait()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("lock holder: %q, %v", line, err)
	}

	subs := []string{"open", "close"}
	results := make([]result, len(subs))
	var wg sync.WaitGroup
	for i, sub := range subs { // both wait out the timeout at once
		wg.Go(func() { results[i] = runIn(t, home, withID, "kit", sub, "b") })
	}
	wg.Wait()
	for i, r := range results {
		if r.code != exitRefused || r.stdout != "" ||
			r.stderr != "stack kit "+subs[i]+": demo-1.kits is locked by another process\n" {
			t.Errorf("kit %s under a held lock = %+v, want exit 1 with the reason and no context",
				subs[i], r)
		}
	}
	if after, _ := readKits(t, home); after != before {
		t.Fatalf(".kits changed: %q -> %q", before, after)
	}
	if _, err := os.Stat(
		filepath.Join(home, ".local", "state", "stack", "kits", "b"),
	); !errors.Is(
		err,
		fs.ErrNotExist,
	) {
		t.Fatalf("open under a held lock created the kit dir: %v", err)
	}
}

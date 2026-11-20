package budget

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/kit"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// status runs Status over root with home's sessions dir and id.
func status(t *testing.T, root, home, id string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Status(t.Context(), &stdout, Options{
		Root:      root,
		Home:      home,
		SessionID: id,
		Stderr:    &stderr,
	})
	return stdout.String(), stderr.String(), err
}

// TestStatus covers C27: exact byte sizes and per-role counts with an `all` column, an `all`-only
// register, an oversized state file as a note without an error, session marks, and no kits.
func TestStatus(t *testing.T) {
	t.Parallel()

	t.Run("table", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		root := filepath.Join(t.TempDir(), "kits")
		// alpha: CRLF counts as written; 3,001 bytes is over the threshold; every role kind plus
		// items without a string role and a non-object, which are not counted.
		writeFile(t, filepath.Join(root, "alpha", "NORTH.md"), "north\r\n\r\n")
		writeFile(t, filepath.Join(root, "alpha", "state.md"), strings.Repeat("s", 3001))
		writeFile(t, filepath.Join(root, "alpha", "attention", "items.json"), `{"items": [
			{"id": "a", "role": "orchestrator"}, {"id": "b", "role": "orchestrator"},
			{"id": "c", "role": "worker"}, {"id": "d", "role": "all"},
			{"id": "e"}, {"id": "f", "role": 7}, {"id": "g", "role": " "}, "text"]}`)
		// beta: an `all`-only register and an empty state.md.
		writeFile(t, filepath.Join(root, "beta", "state.md"), "")
		writeFile(t, filepath.Join(root, "beta", "attention", "items.json"),
			`{"items": [{"role": "all"}, {"role": "all"}]}`)
		// gamma: malformed items; exactly 3,000 bytes is not over.
		writeFile(t, filepath.Join(root, "gamma", "state.md"), strings.Repeat("g", 3000))
		writeFile(t, filepath.Join(root, "gamma", "attention", "items.json"), `{"items": [`)
		// Neither a file nor an invalid slug is a kit.
		writeFile(t, filepath.Join(root, "notes.txt"), "x")
		writeFile(t, filepath.Join(root, "Upper", "state.md"), "x")

		// Open alpha and a kit whose dir is gone; only kits under the root are listed.
		store := kit.Store{Home: home}
		for _, slug := range []string{"alpha", "gone"} {
			if err := store.Add(t.Context(), "s-1", slug); err != nil {
				t.Fatal(err)
			}
		}

		out, errOut, err := status(t, root, home, "s-1")
		want := "kit root: " + root + "\n" +
			"  kit    NORTH.md  state.md  orchestrator  worker  all\n" +
			"* alpha  9         3001      2             1       1\n" +
			"  beta   -         0         0             0       2\n" +
			"  gamma  -         3000      ?             ?       ?\n" +
			"* open in this session\n" +
			"note: alpha/state.md is 3001 bytes, over 3000\n" +
			"note: gamma/attention/items.json is not valid JSON\n"
		if err != nil || out != want || errOut != "" {
			t.Errorf("Status = %q, %q, %v\nwant %q", out, errOut, err, want)
		}

		// Without a session id nothing is marked.
		out, _, err = status(t, root, home, "")
		if err != nil || strings.Contains(out, "*") {
			t.Errorf("no session: %q, %v", out, err)
		}
	})

	t.Run("all-only register", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "solo", "attention", "items.json"),
			`{"items": [{"role": "all"}, {"role": "all"}, {"role": "all"}]}`)
		out, _, err := status(t, root, t.TempDir(), "")
		want := "kit root: " + root + "\n" +
			"  kit   NORTH.md  state.md  all\n" +
			"  solo  -         -         3\n"
		if err != nil || out != want {
			t.Errorf("Status = %q, %v\nwant %q", out, err, want)
		}
	})

	t.Run("no kits", func(t *testing.T) {
		t.Parallel()
		empty := t.TempDir()
		writeFile(t, filepath.Join(empty, "notes.txt"), "x")
		for name, root := range map[string]string{
			"missing": filepath.Join(t.TempDir(), "kits"),
			"empty":   empty,
		} {
			out, errOut, err := status(t, root, t.TempDir(), "s-1")
			if err != nil || out != "no kits\n" || errOut != "" {
				t.Errorf("%s root: %q, %q, %v", name, out, errOut, err)
			}
		}
	})

	t.Run("unreadable root", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "kits")
		writeFile(t, root, "not a dir")
		if out, _, err := status(t, root, t.TempDir(), ""); err == nil || out != "" {
			t.Errorf("Status = %q, %v, want an error", out, err)
		}
	})
}

// TestRoleCell quotes role names that would break the table.
func TestRoleCell(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"worker":    "worker",
		"two word":  `"two word"`,
		"tab\trole": `"tab\trole"`,
	} {
		if got := cell(in); got != want {
			t.Errorf("cell(%q) = %q, want %q", in, got, want)
		}
	}
}

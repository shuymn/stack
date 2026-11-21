package guard

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the .golden files of TestGolden")

// Placeholders in testdata: the payload's cwd, the home directory, a local path publictext reports,
// and the default T3 secrets entry. They keep machine paths out of the repository.
const (
	phCwd       = "@@CWD@@"
	phHome      = "@@HOME@@"
	phLocal     = "@@LOCAL@@"
	phT3Secrets = "@@T3SECRETS@@"
)

// TestGolden runs each testdata/<name>.json PreToolUse payload through the built stack-guard and
// compares its stdout with testdata/<name>.golden (empty for allow) and its exit code with 0. Files
// under testdata/<name>.files/ are copied into the payload's cwd first. Each deny's reason names its
// rule. The core binary, which keeps these rules until their removal (docs/design/core-stages.md §7
// PR 10), must answer the same with its own prefix, so the two copies cannot drift apart.
func TestGolden(t *testing.T) {
	payloads, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(payloads) == 0 {
		t.Fatalf("no golden payloads: %v", err)
	}
	local := strings.TrimSuffix(localPath(), "/notes.md")
	for _, p := range payloads {
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		t.Run(name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			repl := strings.NewReplacer(phCwd, jsonPath(cwd), phHome, jsonPath(home),
				phLocal, local, phT3Secrets, t3Secrets)
			copyFiles(t, filepath.Join("testdata", name+".files"), cwd, repl)

			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			stdin := repl.Replace(string(raw))
			r := guardBin(t, home, stdin)
			if r.code != 0 {
				t.Errorf("stack-guard exit %d, want 0 (stderr %q)", r.code, r.stderr)
			}
			if rule := deniedBy(r, "stack-guard"); rule != "" && !strings.Contains(name, rule) {
				t.Errorf("deny by %s, but the golden name %q names another rule", rule, name)
			}
			back := strings.NewReplacer(jsonPath(cwd), phCwd, jsonPath(home), phHome,
				local, phLocal)
			got := back.Replace(r.stdout)

			golden := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal([]byte(got), want) {
				t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
			}
			if strings.HasPrefix(name, "allow-") != (len(want) == 0) {
				t.Errorf("golden %q: allow- names must have empty output, others a deny", name)
			}

			rc := coreBin(t, home, stdin)
			if rc.code != 0 {
				t.Errorf("core hook exit %d, want 0 (stderr %q)", rc.code, rc.stderr)
			}
			core := back.Replace(rc.stdout)
			wantCore := strings.Replace(string(want), `"stack-guard (`, `"stack guard (`, 1)
			if core != wantCore {
				t.Errorf("core hook drifted from stack-guard:\n%s\nwant:\n%s", core, wantCore)
			}
		})
	}
}

// jsonPath is p as it appears inside a JSON string: temp dirs are ASCII, so only the Windows
// backslash needs escaping.
func jsonPath(p string) string {
	return strings.ReplaceAll(p, `\`, `\\`)
}

// copyFiles copies the tree under src into dst, replacing placeholders in file contents.
func copyFiles(t *testing.T, src, dst string, repl *strings.Replacer) {
	t.Helper()
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return
	}
	from := os.DirFS(src)
	to, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = to.Close() }()
	err = fs.WalkDir(from, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(from, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if err := to.MkdirAll(path.Dir(p), 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", p, err)
		}
		if err := to.WriteFile(p, []byte(repl.Replace(string(data))), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

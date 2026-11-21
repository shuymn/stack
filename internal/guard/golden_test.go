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

	"github.com/shuymn/stack/internal/hookio"
)

var update = flag.Bool("update", false, "rewrite the .golden files of TestGolden")

// phCwd stands for the payload's cwd in testdata, which keeps machine paths out of the repository.
const phCwd = "@@CWD@@"

// TestGolden runs each testdata/<name>.json PreToolUse payload through the decision and the hook
// encoder and compares stdout with testdata/<name>.golden (empty for allow). Files under
// testdata/<name>.files/ are copied into the payload's cwd first. Each deny's reason names its rule.
func TestGolden(t *testing.T) {
	payloads, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(payloads) == 0 {
		t.Fatalf("no golden payloads: %v", err)
	}
	for _, p := range payloads {
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			repl := strings.NewReplacer(phCwd, jsonPath(cwd))
			copyFiles(t, filepath.Join("testdata", name+".files"), cwd, repl)

			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			in, err := hookio.Decode(strings.NewReader(repl.Replace(string(raw))))
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			var out []byte
			if d, ok := Decide(in, Options{Home: t.TempDir()}); ok {
				if !strings.Contains(name, d.Rule) {
					t.Errorf("deny by %s, but the golden name %q names another rule", d.Rule, name)
				}
				if out, err = hookio.Deny(d.Reason); err != nil {
					t.Fatal(err)
				}
			}
			got := strings.ReplaceAll(string(out), jsonPath(cwd), phCwd)

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

package publictext

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/internal/gittest"
)

// p builds a path from parts at run time, so this file publishes no literal local path.
func p(sep string, parts ...string) string {
	return strings.Join(parts, sep)
}

func TestLocalPath(t *testing.T) {
	bs := `\`
	// Han names are escaped (gosmopolitan): name = 名前 "name", docs = 資料 "documents", secret = 秘密.
	const name, docs = "\u540d\u524d", "\u8cc7\u6599"
	usersA := p("/", "", "Users", "a") // one-character name
	homeBob := p("/", "", "home", "bob")
	volData := p("/", "", "Volumes", "Data")
	winBack := p(bs, "C:", "Users", "erin")
	winJSON := p(bs+bs, "D:", "users", "erin")
	winSlash := p("/", "c:", "USERS", "erin")
	gitBash := p("/", "", "c", "Users", "erin")
	wsl := p("/", "", "mnt", "d", "Users", "erin")
	unicodeName := p("/", "", "home", name)
	claudeTmp := p("/", "", "tmp", "claude-501", "")
	folders := p("/", "", "private", "var", "folders", "xy")

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"one-character name, end of text", usersA, []string{usersA}},
		{"trailing slash kept", "see " + homeBob + "/work.", []string{homeBob + "/"}},
		{"volume", "copied from " + volData + "/x", []string{volData + "/"}},
		{"windows backslash", winBack + bs + "x", []string{winBack + bs}},
		{"windows JSON-escaped", `"` + winJSON + bs + bs + `x"`, []string{winJSON + bs + bs}},
		{"windows slash, case-folded", winSlash + "/x", []string{winSlash + "/"}},
		{"git bash", "cd " + gitBash, []string{gitBash}},
		{"wsl", wsl + "/src", []string{wsl + "/"}},
		{"unicode name", unicodeName + "/x", []string{unicodeName + "/"}},
		{"after a non-name rune", "(" + homeBob + ")", []string{homeBob}},
		{"after a CJK punctuation", "「" + homeBob + "」", []string{homeBob}},
		{"claude temp", "ran in " + claudeTmp + "x", []string{claudeTmp}},
		{"claude temp mid-token", "x" + claudeTmp, []string{claudeTmp}},
		{"macOS temp", folders + "/T", []string{folders + "/"}},
		{"two hits in order", homeBob + " and " + usersA, []string{homeBob, usersA}},
		{"after a bare CR", "note\rsee " + homeBob, []string{homeBob}},

		{"after a CJK letter", docs + p("/", "", "home", "demo"), nil},
		{"placeholder dots", "/home/...", nil},
		{"placeholder ellipsis", "/home/…", nil},
		{"placeholder angle", "/home/<name>", nil},
		{"placeholder windows", `C:\Users\...` + ` and C:\Users\<name>\`, nil},
		{"placeholder braces", "/Users/{user}/x and /Users/$USER and /Users/%USERNAME%", nil},
		{"placeholder temp", "/private/var/folders/…", nil},
		{"repository path", "home/dot_x", nil},
		{"tilde", "~/x and ~/.claude/", nil},
		{"after a letter", "x" + homeBob, nil},
		{"after a dot", "." + homeBob, nil},
		{"after an underscore", "dot_" + p("/", "", "home", "x"), nil},
		{"after a dash", "-" + homeBob, nil},
		{"drive after a letter", "x" + winBack, nil},
		{"posix case-sensitive", p("/", "", "users", "bob"), nil},
		{"other root", p("/", "", "opt", "home", "bob"), nil},
		{"claude temp without digits", "/tmp/claude-x/", nil},
		{"claude temp without slash", "/tmp/claude-501", nil},
		{"git bash without users", "/c/Program Files/x", nil},
		{"drive without users", `C:\Windows\x`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LocalPaths(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("LocalPaths(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRepoTree runs the matcher over every tracked file and the design document: the repository
// publishes no local path.
func TestRepoTree(t *testing.T) {
	git := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		res, err := proc.Exec{}.Run(ctx, proc.Cmd{
			Name: "git", Args: args, Env: gittest.CleanEnv(os.Environ()), MaxStdout: 64 << 20,
		})
		if err != nil || res.ExitCode != 0 || res.TimedOut || res.WaitDelayExceeded ||
			res.Truncated {
			t.Fatalf("git %q: %v %+v", args, err, res)
		}
		return string(res.Stdout)
	}
	root := strings.TrimSpace(git("rev-parse", "--show-toplevel"))
	// Untracked files that are not ignored are included, so a file is checked before its commit.
	out := git("-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	files := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	design := filepath.ToSlash(filepath.Join("docs", "design", "phase2-core.md"))
	if !slices.Contains(files, design) {
		files = append(files, design)
	}
	if !slices.Contains(files, "docs/architecture.md") {
		t.Fatalf("docs/architecture.md is not tracked; ls-files = %d files", len(files))
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted in the working tree
		}
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue // binary
		}
		if hits := LocalPaths(string(data)); hits != nil {
			t.Errorf("%s: local paths %q", f, hits)
		}
	}
}

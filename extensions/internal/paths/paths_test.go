package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sep is the OS separator as a string, for expected values built without cleaning.
const sep = string(filepath.Separator)

func TestExpand(t *testing.T) {
	home := t.TempDir()
	tests := []struct {
		in, want string
	}{
		{"~", home},
		{"~/", home},
		{"~/x/../y", home + sep + filepath.FromSlash("x/../y")},
		{`~\x`, home + sep + "x"},
		{
			"~/.config/stack/config.toml",
			home + sep + filepath.FromSlash(".config/stack/config.toml"),
		},
		{"~user", "~user"},
		{"~user/x", "~user/x"},
		{"x/~", "x/~"},
		{".dev.vars", ".dev.vars"},
		{"a//b/", "a//b/"},
	}
	for _, tt := range tests {
		if got := Expand(home, tt.in); got != tt.want {
			t.Errorf("Expand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTildeWindowsSeparators(t *testing.T) {
	if got, ok := tilde(`C:\h`, "~/a/../b/c.md", true); !ok || got != `C:\h\a\..\b\c.md` {
		t.Errorf("tilde windows = %q, %v", got, ok)
	}
	if got, _ := tilde("/h", "~/a/b", false); got != "/h/a/b" {
		t.Errorf("tilde posix = %q", got)
	}
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	tests := []struct {
		name   string
		cwd    string
		in     string
		expand bool
		want   string
		ok     bool
	}{
		{
			"relative joined to cwd",
			cwd,
			"a/../b",
			false,
			cwd + sep + filepath.FromSlash("a/../b"),
			true,
		},
		{"tilde expanded", cwd, "~/x", true, home + sep + "x", true},
		{"bare tilde expanded", cwd, "~", true, home, true},
		{"quoted tilde is a name", cwd, "~", false, cwd + sep + "~", true},
		{"tilde user is a name", cwd, "~user", true, cwd + sep + "~user", true},
		{
			"absolute kept as written",
			"",
			home + sep + ".." + sep + "b",
			false,
			home + sep + ".." + sep + "b",
			true,
		},
		{"relative with unknown cwd", "", "x", false, "", false},
		{"relative under shell root", ShellRoot, "x", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Resolve(home, tt.cwd, tt.in, tt.expand)
			if got != tt.want || ok != tt.ok {
				t.Fatalf(
					"Resolve(%q, %q) = %q, %v; want %q, %v",
					tt.cwd,
					tt.in,
					got,
					ok,
					tt.want,
					tt.ok,
				)
			}
		})
	}
}

func TestResolvePOSIXRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX root")
	}
	if got, ok := Resolve("", "", "/", false); got != "/" || !ok {
		t.Fatalf("Resolve(/) = %q, %v; want /, true", got, ok)
	}
}

func TestResolveWindowsGitBash(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Git Bash forms are mapped on Windows only")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(file)
	gitBash := "/" + strings.ToLower(vol[:1]) + filepath.ToSlash(file[len(vol):])
	got, ok := Resolve("", "", gitBash, false)
	if !ok {
		t.Fatalf("Resolve(%q) not ok", gitBash)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "ok" {
		t.Fatalf("ReadFile(Resolve(%q) = %q) = %q, %v", gitBash, got, data, err)
	}

	for _, in := range []string{"/", "/./", "/usr/.."} {
		if got, ok := Resolve("", dir, in, false); got != ShellRoot || !ok {
			t.Errorf("Resolve(%q) = %q, %v; want ShellRoot", in, got, ok)
		}
	}
	for _, in := range []string{"/usr/bin", `\x`, "C:body.txt"} {
		if got, ok := Resolve("", dir, in, false); ok {
			t.Errorf("Resolve(%q) = %q, ok; want unknown", in, got)
		}
	}
	if got, ok := Resolve("", dir, "/c", false); got != `C:\` || !ok {
		t.Errorf("Resolve(/c) = %q, %v; want C:\\", got, ok)
	}
}

// TestResolveKeepsComponents checks that Resolve returns `..` as written. On Unix it also opens the
// result: lexical `..` removal would skip the symlink and name a different file. Windows itself
// removes `..` before following links, so only the string is checked there.
func TestResolveKeepsComponents(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "f.txt"), []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	symlinked := os.Symlink(filepath.Join(target, "sub"), link) == nil
	tests := []struct{ in, want string }{
		{"link/../f.txt", dir + sep + filepath.FromSlash("link/../f.txt")},
		{link + sep + ".." + sep + "f.txt", link + sep + ".." + sep + "f.txt"},
	}
	for _, tt := range tests {
		got, ok := Resolve("", dir, tt.in, false)
		if got != tt.want || !ok {
			t.Errorf("Resolve(%q) = %q, %v; want %q, true", tt.in, got, ok, tt.want)
			continue
		}
		if runtime.GOOS == "windows" || !symlinked {
			continue
		}
		data, err := os.ReadFile(got)
		if err != nil || string(data) != "real" {
			t.Errorf("ReadFile(Resolve(%q) = %q) = %q, %v; want real", tt.in, got, data, err)
		}
	}
}

func TestResolveRules(t *testing.T) {
	tests := []struct {
		windows bool
		cwd, in string
		want    string
		ok      bool
	}{
		{false, "/w", "/var/../usr/bin/true", "/var/../usr/bin/true", true},
		{false, "/w/", "x", "/w/x", true},
		{false, "/w", `C:\x`, `/w/C:\x`, true},
		{true, `C:\w`, "C:body.txt", "", false},
		{true, `C:\w`, "c:", "", false},
		{true, `C:\w`, `\x`, "", false},
		{true, `C:\w`, `x\..\y`, `C:\w\x\..\y`, true},
		{true, `C:\w`, `D:\a\..\b`, `D:\a\..\b`, true},
		{true, `C:\w`, "C:/a", "C:/a", true},
		{true, `C:\w`, `\\host\share\x`, `\\host\share\x`, true},
		{true, `C:\w`, "//host/share/x", "//host/share/x", true},
		{true, `C:\w`, "/c/a/../b", `C:\a\..\b`, true},
		{true, `C:\w`, "/c/..", ShellRoot, true},
		{true, `C:\w`, "/", ShellRoot, true},
		{true, `C:\w`, "/usr/bin", "", false},
		{true, ShellRoot, "x", "", false},
	}
	for _, tt := range tests {
		got, ok := resolve("", tt.cwd, tt.in, false, tt.windows)
		if got != tt.want || ok != tt.ok {
			t.Errorf("resolve(windows=%v, %q, %q) = %q, %v; want %q, %v",
				tt.windows, tt.cwd, tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestGitBashToWindows(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/", ShellRoot, true},
		{"/./", ShellRoot, true},
		{"/c", `C:\`, true},
		{"/c/", `C:\`, true},
		{"/D/a/b/../c", `D:\a\b\..\c`, true},
		{"/c/..", ShellRoot, true},
		{"/c/a/../..", ShellRoot, true},
		{"/c/a/../../d/x", `D:\x`, true},
		{"/c/../usr", "", false},
		{"/usr/..", ShellRoot, true},
		{"/usr/../c/x", `C:\x`, true},
		{"/usr/bin", "", false},
		{"/1/x", "", false},
	}
	for _, tt := range tests {
		got, ok := gitBashToWindows(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("gitBashToWindows(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// winUsers builds a Windows profile path at run time so no literal local path sits in the source.
func winUsers(drive, users, name string, rest ...string) string {
	return strings.Join(append([]string{drive + ":", users, name}, rest...), `\`)
}

func TestHome(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", dir)
	} else {
		t.Setenv("HOME", dir)
	}
	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	if got != dir {
		t.Fatalf("Home() = %q, want %q", got, dir)
	}
	if want := filepath.Join(dir, "x"); Expand(got, `~\x`) != want {
		t.Fatalf(`Expand(Home(), ~\x) = %q, want %q`, Expand(got, `~\x`), want)
	}
}

// sep is the OS separator as a string, for expected values built without cleaning.
const sep = string(filepath.Separator)

func TestExpand(t *testing.T) {
	home := t.TempDir()
	tests := []struct {
		in, want string
	}{
		{"~", home},
		{"~/", home},
		{"~/x/../y", home + sep + "x/../y"},
		{`~\x`, home + sep + "x"},
		{"~/.config/stack/config.toml", home + sep + ".config/stack/config.toml"},
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
		{"relative joined to cwd", cwd, "a/../b", false, cwd + sep + "a/../b", true},
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
		{"link/../f.txt", dir + sep + "link/../f.txt"},
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

func TestCompare(t *testing.T) {
	t.Run("component boundaries", func(t *testing.T) {
		for _, fs := range []struct{ windows, fold bool }{{false, false}, {false, true}, {true, true}} {
			n := func(p string) string { return norm(p, fs.windows, fs.fold) }
			cases := []struct {
				p, root string
				want    bool
			}{
				{".t3/userdata/secrets-example", ".t3/userdata/secrets", false},
				{".t3/userdata/secrets", ".t3/userdata/secrets", true},
				{".t3/userdata/secrets/k", ".t3/userdata/secrets", true},
				{".t3/userdata/secrets/../x", ".t3/userdata/secrets", false},
				{"/a/b", "/", true},
				{"/ab", "/a", false},
				{"/a", "", false},
			}
			for _, c := range cases {
				if got := within(n(c.p), n(c.root)); got != c.want {
					t.Errorf("%+v within(%q, %q) = %v, want %v", fs, c.p, c.root, got, c.want)
				}
			}
			if hasComponent(n("/x/.git/hooks"), n(".git")) != true ||
				hasComponent(n("/x/.github"), n(".git")) {
				t.Errorf("%+v hasComponent mismatch", fs)
			}
		}
	})

	t.Run("windows folds case and separators", func(t *testing.T) {
		upper := winUsers("C", "Users", "Name", ".T3")
		lower := strings.ToLower(strings.ReplaceAll(upper, `\`, "/"))
		if norm(upper, true, true) != norm(lower, true, true) {
			t.Fatalf("norm(%q) != norm(%q) on windows", upper, lower)
		}
		if !within(
			norm(winUsers("c", "users", "name", ".t3", "x"), true, true),
			norm(upper, true, true),
		) {
			t.Fatal("folded child not within root")
		}
		if within(norm(`C:\a`, true, true), norm(`C:\`, true, true)) != true {
			t.Fatal(`C:\a not within C:\`)
		}
		for _, c := range []struct {
			p, root string
			want    bool
		}{
			{"//host/share/../x", "//host/share", true},
			{`\\host\share\..\..\x`, `\\host\share`, true},
			{"//host/share2/x", "//host/share", false},
			{"//host/x", "//host/share", false},
			{"C:/../x", "C:/", true},
		} {
			if got := within(norm(c.p, true, true), norm(c.root, true, true)); got != c.want {
				t.Errorf("windows within(%q, %q) = %v, want %v", c.p, c.root, got, c.want)
			}
		}
		if got := norm(`\\server\share\x\..`, true, false); got != "//server/share" {
			t.Fatalf("UNC norm = %q", got)
		}
	})

	t.Run("linux keeps case and backslashes", func(t *testing.T) {
		if norm(".DEV.VARS", false, false) == norm(".dev.vars", false, false) {
			t.Fatal(".DEV.VARS equals .dev.vars without folding")
		}
		if hasComponent(norm(`a\.dev.vars`, false, false), ".dev.vars") {
			t.Fatal("backslash split as a separator on linux")
		}
	})

	t.Run("darwin folds case only", func(t *testing.T) {
		if norm(".DEV.VARS", false, true) != norm(".dev.vars", false, true) {
			t.Fatal(".DEV.VARS differs from .dev.vars with folding")
		}
	})

	t.Run("this platform", func(t *testing.T) {
		same := Same(".DEV.VARS", ".dev.vars")
		switch runtime.GOOS {
		case "linux":
			if same {
				t.Fatal("linux: .DEV.VARS is the same file as .dev.vars")
			}
		case "darwin", "windows":
			if !same {
				t.Fatalf("%s: .DEV.VARS is not the same file as .dev.vars", runtime.GOOS)
			}
		}
		if runtime.GOOS == "windows" {
			upper := winUsers("C", "Users", "Name", ".T3")
			lower := strings.ToLower(strings.ReplaceAll(upper, `\`, "/"))
			if !Same(upper, lower) || !Within(filepath.Join(upper, "x"), lower) {
				t.Fatalf("windows: %q not the same as %q", upper, lower)
			}
		}
		if Within(".t3/userdata/secrets-example", ".t3/userdata/secrets") {
			t.Fatal("secrets-example within secrets")
		}
		if !HasComponent(filepath.Join("r", ".git", "hooks"), ".git") {
			t.Fatal("HasComponent missed .git")
		}
	})
}

func TestValidSlug(t *testing.T) {
	valid := []string{
		"demo",
		"a",
		"0x",
		"a.b",
		"a_b-c",
		"con-x",
		"comx",
		"com0",
		strings.Repeat("a", 64),
	}
	invalid := []string{
		"",
		"Demo",
		"-a",
		".a",
		"_a",
		"a/b",
		`a\b`,
		"a b",
		"..",
		"a..b",
		"demo.",
		strings.Repeat("a", 65),
		"con",
		"nul",
		"aux",
		"prn",
		"com1",
		"com9",
		"lpt1",
		"lpt9",
		"con.txt",
		"nul.x.y",
	}
	for _, s := range valid {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

func TestValidSessionID(t *testing.T) {
	valid := []string{"0b6f2c1e-8d3a-4f8e-9a51-1c2d3e4f5a6b", "A_b-9", strings.Repeat("x", 128)}
	invalid := []string{"", "a b", "a;b", "a/b", "a.b", "$(x)", "a\nb", strings.Repeat("x", 129)}
	for _, s := range valid {
		if !ValidSessionID(s) {
			t.Errorf("ValidSessionID(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if ValidSessionID(s) {
			t.Errorf("ValidSessionID(%q) = true, want false", s)
		}
	}
}

func TestLocations(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".local", "state", "stack")
	checks := map[string][2]string{
		"ConfigFile": {
			ConfigFile(home),
			filepath.Join(home, ".config", "stack", "config.toml"),
		},
		"StateRoot":   {StateRoot(home), state},
		"SessionsDir": {SessionsDir(home), filepath.Join(state, "sessions")},
		"SessionKitsFile": {
			SessionKitsFile(home, "AbC-1"),
			filepath.Join(state, "sessions", "abc-1.kits"),
		},
		"SessionLockFile": {
			SessionLockFile(home, "AbC-1"),
			filepath.Join(state, "sessions", "abc-1.lock"),
		},
		"KitDir": {
			KitDir(filepath.Join(state, "kits"), "demo"),
			filepath.Join(state, "kits", "demo"),
		},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if SessionKitsFile(home, "ABC") != SessionKitsFile(home, "abc") {
		t.Error("session ids differing in case map to different files")
	}
}

func TestIsAgentWorktree(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"/r/stack-worktrees/agent-a1", true},
		{"/r/stack-worktrees/agent-a1/sub", true},
		{"/r/-worktrees/agent-x", true},
		{"/r/stack-worktrees/agent-", false},
		{"/r/stack-worktrees/agentx", false},
		{"/r/stack-worktrees/x/agent-a1", false},
		{"/r/stack-worktree/agent-a1", false},
		{"/r/stack-worktrees-x/agent-a1", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsAgentWorktree(tt.in); got != tt.want {
			t.Errorf("IsAgentWorktree(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	if runtime.GOOS == "windows" && !IsAgentWorktree(`C:\r\stack-worktrees\agent-a1`) {
		t.Error("windows separators not recognised")
	}
}

package classify

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/internal/gittest"
)

// memEnv is a repository fixture with a Claude configuration directory and a home of its own.
type memEnv struct {
	t         *testing.T
	r         *gittest.Repo
	cfg, home string
	lookup    *Lookup
}

func newMemEnv(t *testing.T) *memEnv {
	t.Helper()
	r := gittest.New(t)
	r.Write("a.txt", "a\n")
	r.Commit("init")
	m := &memEnv{t: t, r: r, cfg: realDir(t), home: realDir(t)}
	main, err := filepath.EvalSymlinks(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Dir = main
	m.lookup = &Lookup{Runner: proc.Exec{}, Env: r.Env, Config: m.cfg, Home: m.home}
	return m
}

// index writes a memory index for the project of dir (the encoding of dir) under the config.
func (m *memEnv) index(dir, text string) {
	m.t.Helper()
	m.indexAt(filepath.Join(m.cfg, "projects", ProjectName(dir), "memory"), text)
}

func (m *memEnv) indexAt(memDir, text string) {
	m.t.Helper()
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(memDir, memoryIndexFile),
		[]byte(text),
		0o600,
	); err != nil {
		m.t.Fatal(err)
	}
}

func (m *memEnv) write(path, text string) {
	m.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		m.t.Fatal(err)
	}
}

// worktree adds a linked worktree named name next to the repository and returns its path.
func (m *memEnv) worktree(name string) string {
	m.t.Helper()
	p := filepath.Join(realDir(m.t), name)
	m.r.Git("worktree", "add", "-q", p, "-b", name)
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		m.t.Fatal(err)
	}
	return resolved
}

func (m *memEnv) find(cwd string) Memory { return m.lookup.Find(m.t.Context(), cwd) }

func wantIndex(t *testing.T, got Memory, index string) {
	t.Helper()
	if got.Outcome != "" || got.Index != index {
		t.Fatalf("memory %+v, want index %q", got, index)
	}
}

func wantMemory(t *testing.T, got Memory, outcome, reason string) {
	t.Helper()
	if got.Outcome != outcome || got.Reason != reason || got.Index != "" {
		t.Fatalf("memory %+v, want %s (%s)", got, outcome, reason)
	}
}

// TestMemoryDir is C9's memory lookup: the directory is found only as §3.2 establishes it, each
// case with a decoy index where a wrong lookup would land.
//
//nolint:maintidx // one subtest per layout
func TestMemoryDir(t *testing.T) {
	t.Run("an ordinary checkout, its subdirectory and a linked worktree share the main index",
		func(t *testing.T) {
			m := newMemEnv(t)
			wt := m.worktree("feat")
			sub := filepath.Join(m.r.Dir, "pkg", "deep")
			if err := os.MkdirAll(sub, 0o700); err != nil {
				t.Fatal(err)
			}
			m.index(m.r.Dir, "main index")
			// Decoys at the encodings of the worktree and the subdirectory.
			m.index(wt, "decoy worktree")
			m.index(sub, "decoy subdirectory")
			wantIndex(t, m.find(m.r.Dir), "main index")
			wantIndex(t, m.find(sub), "main index")
			wantIndex(t, m.find(wt), "main index")
			wantIndex(t, m.find(filepath.Join(wt, ".")), "main index")
		})

	t.Run("a cwd outside any repository is its own main checkout", func(t *testing.T) {
		m := newMemEnv(t)
		plain := realDir(t)
		m.index(plain, "plain index")
		m.index(filepath.Dir(plain), "decoy parent")
		wantIndex(t, m.find(plain), "plain index")
	})

	t.Run("a removed cwd", func(t *testing.T) {
		m := newMemEnv(t)
		gone := filepath.Join(realDir(t), "gone")
		m.index(gone, "decoy")
		wantMemory(t, m.find(gone), DupUnassessed, ReasonCwdGone)
	})

	t.Run(
		"no project directory yet, and a directory without MEMORY.md, are new",
		func(t *testing.T) {
			m := newMemEnv(t)
			got := m.find(m.r.Dir)
			wantMemory(t, got, DupNew, ReasonNoMemory)
			memDir := filepath.Join(m.cfg, "projects", ProjectName(m.r.Dir), "memory")
			if err := os.MkdirAll(memDir, 0o700); err != nil {
				t.Fatal(err)
			}
			m.lookup.cache = nil
			wantMemory(t, m.find(m.r.Dir), DupNew, ReasonNoMemory)
			m.indexAt(memDir, " \n\n")
			m.lookup.cache = nil
			wantMemory(t, m.find(m.r.Dir), DupNew, ReasonNoMemory)
		},
	)

	t.Run("an unreadable index", func(t *testing.T) {
		m := newMemEnv(t)
		memDir := filepath.Join(m.cfg, "projects", ProjectName(m.r.Dir), "memory")
		if err := os.MkdirAll(filepath.Join(memDir, memoryIndexFile), 0o700); err != nil {
			t.Fatal(err)
		}
		wantMemory(t, m.find(m.r.Dir), DupUnassessed, ReasonIndexBad)
	})

	for _, c := range []struct {
		name  string
		setup func(m *memEnv) string
	}{
		{"a separate Git directory named .git", func(m *memEnv) string {
			other := realDir(m.t)
			elsewhere := filepath.Join(other, "elsewhere", ".git")
			if err := os.MkdirAll(filepath.Dir(elsewhere), 0o700); err != nil {
				m.t.Fatal(err)
			}
			m.r.Git("clone", "-q", "--bare", ".", filepath.Join(other, "bare.git"))
			if err := os.Rename(filepath.Join(other, "bare.git"), elsewhere); err != nil {
				m.t.Fatal(err)
			}
			m.r.Git("config", "--file", filepath.Join(elsewhere, "config"), "core.bare", "false")
			top := filepath.Join(other, "top")
			m.write(filepath.Join(top, ".git"), "gitdir: "+filepath.ToSlash(elsewhere)+"\n")
			return top
		}},
		{"a gitfile pointing at another checkout's .git", func(m *memEnv) string {
			top := filepath.Join(realDir(m.t), "top")
			m.write(filepath.Join(top, ".git"), "gitdir: "+filepath.ToSlash(filepath.Join(m.r.Dir, ".git"))+"\n")
			return top
		}},
		{"a worktree not registered back", func(m *memEnv) string {
			wt := m.worktree("lost")
			back := filepath.Join(m.r.Dir, ".git", "worktrees", "lost", "gitdir")
			m.write(back, filepath.ToSlash(filepath.Join(realDir(m.t), "elsewhere", ".git"))+"\n")
			return wt
		}},
		{"a submodule", func(m *memEnv) string {
			subRepo := m.r.Sibling("subsrc")
			subRepo.Write("s.txt", "s\n")
			subRepo.Commit("sub")
			m.r.Git("-c", "protocol.file.allow=always", "submodule", "add", "-q", subRepo.Dir, "mod")
			resolved, err := filepath.EvalSymlinks(filepath.Join(m.r.Dir, "mod"))
			if err != nil {
				m.t.Fatal(err)
			}
			return resolved
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newMemEnv(t)
			cwd := c.setup(m)
			m.index(m.r.Dir, "decoy main")
			m.index(cwd, "decoy cwd")
			wantMemory(t, m.find(cwd), DupUnassessed, ReasonLayout)
		})
	}

	t.Run("an inherited GIT_DIR does not redirect the lookup", func(t *testing.T) {
		m := newMemEnv(t)
		other := gittest.New(t)
		other.Write("o", "o")
		other.Commit("o")
		m.index(m.r.Dir, "main index")
		m.lookup.Env = append(slices.Clone(m.r.Env), "GIT_DIR="+filepath.Join(other.Dir, ".git"),
			"GIT_WORK_TREE="+other.Dir)
		wantIndex(t, m.find(m.r.Dir), "main index")
	})

	t.Run("inherited discovery settings do not stop the lookup at a subdirectory",
		func(t *testing.T) {
			m := newMemEnv(t)
			sub := filepath.Join(m.r.Dir, "pkg")
			if err := os.MkdirAll(sub, 0o700); err != nil {
				t.Fatal(err)
			}
			m.index(m.r.Dir, "main index")
			m.index(sub, "decoy subdirectory")
			m.lookup.Env = append(slices.Clone(m.r.Env), "GIT_CEILING_DIRECTORIES="+m.r.Dir,
				"GIT_DISCOVERY_ACROSS_FILESYSTEM=0")
			wantIndex(t, m.find(sub), "main index")
		})

	t.Run("a user autoMemoryDirectory", func(t *testing.T) {
		for _, c := range []struct {
			name, value string
			want        func(m *memEnv) (string, string, string)
		}{
			{"absolute", "ABS", nil},
			{"with ~/", "~/notes/mem", nil},
		} {
			t.Run(c.name, func(t *testing.T) {
				m := newMemEnv(t)
				dir := filepath.Join(realDir(t), "custom")
				value := c.value
				switch c.name {
				case "absolute":
					value = filepath.ToSlash(dir)
				default:
					dir = filepath.Join(m.home, "notes", "mem")
				}
				m.indexAt(dir, "custom index")
				m.index(m.r.Dir, "decoy default")
				m.write(
					filepath.Join(m.cfg, "settings.json"),
					`{"autoMemoryDirectory":"`+value+`"}`,
				)
				wantIndex(t, m.find(m.r.Dir), "custom index")
			})
		}
		t.Run("relative", func(t *testing.T) {
			m := newMemEnv(t)
			m.index(m.r.Dir, "decoy default")
			m.write(filepath.Join(m.cfg, "settings.json"), `{"autoMemoryDirectory":"rel/mem"}`)
			wantMemory(t, m.find(m.r.Dir), DupUnassessed, ReasonSettings)
		})
		t.Run("malformed user settings", func(t *testing.T) {
			m := newMemEnv(t)
			m.index(m.r.Dir, "decoy default")
			m.write(filepath.Join(m.cfg, "settings.json"), `{"autoMemoryDirectory":`)
			wantMemory(t, m.find(m.r.Dir), DupUnassessed, ReasonSettings)
		})
	})

	t.Run("a repository setting autoMemoryDirectory is an override", func(t *testing.T) {
		for _, file := range []string{"settings.json", "settings.local.json"} {
			for _, where := range []string{"cwd", "top", "main"} {
				t.Run(file+" at "+where, func(t *testing.T) {
					m := newMemEnv(t)
					wt := m.worktree("w")
					sub := filepath.Join(wt, "pkg")
					if err := os.MkdirAll(sub, 0o700); err != nil {
						t.Fatal(err)
					}
					dir := map[string]string{"cwd": sub, "top": wt, "main": m.r.Dir}[where]
					decoy := filepath.Join(realDir(t), "override")
					m.indexAt(decoy, "decoy override")
					m.index(m.r.Dir, "decoy default")
					m.write(filepath.Join(dir, ".claude", file),
						`{"autoMemoryDirectory":"`+filepath.ToSlash(decoy)+`"}`)
					wantMemory(t, m.find(sub), DupUnassessed, ReasonOverride)
				})
			}
		}
		t.Run("a malformed repository settings file", func(t *testing.T) {
			m := newMemEnv(t)
			m.index(m.r.Dir, "decoy default")
			m.write(filepath.Join(m.r.Dir, ".claude", "settings.json"), "{not json")
			wantMemory(t, m.find(m.r.Dir), DupUnassessed, ReasonSettings)
		})
		t.Run("settings that do not set it leave the lookup alone", func(t *testing.T) {
			m := newMemEnv(t)
			m.index(m.r.Dir, "main index")
			m.write(filepath.Join(m.r.Dir, ".claude", "settings.json"), `{"model":"x"}`)
			wantIndex(t, m.find(m.r.Dir), "main index")
		})
	})
}

func TestClaudeConfig(t *testing.T) {
	get := func(v string) func(string) string {
		return func(k string) string {
			if k == claudeConfigEnv {
				return v
			}
			return ""
		}
	}
	if got := ClaudeConfig(get("/x/cfg"), "/h"); got != "/x/cfg" {
		t.Fatalf("CLAUDE_CONFIG_DIR gave %q", got)
	}
	if got, want := ClaudeConfig(get(""), "/h"), filepath.Join("/h", ".claude"); got != want {
		t.Fatalf("default gave %q, want %q", got, want)
	}
	if got := ClaudeConfig(get(""), ""); got != "" {
		t.Fatalf("no home gave %q", got)
	}
}

// TestProjectName pins the encoding on fixed POSIX and Windows paths: every byte outside
// [A-Za-z0-9] becomes '-' (an observed behaviour, §3.2).
func TestProjectName(t *testing.T) {
	// Home-like paths are built at run time, so no local path is written in this file.
	posix := "/" + strings.Join([]string{"home", "dev", "src"}, "/")
	windows := `C:\` + strings.Join([]string{"Users", "dev", "src"}, `\`)
	for in, want := range map[string]string{
		posix + "/app":     "-home-dev-src-app",
		posix + "/my.repo": "-home-dev-src-my-repo",
		windows + `\app`:   "C--Users-dev-src-app",
		"/srv/日本":          "-srv-------", //nolint:gosmopolitan // a multi-byte path
		"/a b/c_d":         "-a-b-c-d",
	} {
		if got := ProjectName(in); got != want {
			t.Errorf("ProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadedIndex(t *testing.T) {
	var many strings.Builder
	for i := range 250 {
		many.WriteString("line " + strings.Repeat("x", i%3) + "\n")
	}
	got := loadedIndex([]byte(many.String()))
	if n := strings.Count(got, "\n"); n != 200 {
		t.Fatalf("%d lines loaded, want 200", n)
	}
	long := strings.Repeat("é", 20<<10)
	got = loadedIndex([]byte(long))
	if len(got) > 25<<10 || len(got)%2 != 0 || strings.ToValidUTF8(got, "?") != got {
		t.Fatalf("%d bytes loaded, valid UTF-8 %v", len(got), strings.ToValidUTF8(got, "?") == got)
	}
	if loadedIndex([]byte("a\nb\n")) != "a\nb\n" {
		t.Fatal("a short index was cut")
	}
}

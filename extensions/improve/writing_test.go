package improve

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// guide is a writing guide the fixture repository supplies; nothing else holds its content.
const guide = "# Target writing guide\n\nKeep every rule short.\n"

// commitConfig commits cfg as .stack/improve.toml and pushes it, with setup run before.
func (f *fx) commitConfig(cfg string, setup func()) {
	f.t.Helper()
	if setup != nil {
		setup()
	}
	f.repo.Write(".stack/improve.toml", cfg)
	f.repo.Git("add", "-A")
	f.repo.Git("commit", "-q", "-m", "config")
	f.repo.Git("push", "-q", "origin", "main")
}

// symlink commits a symlink at name pointing to target.
func (f *fx) symlink(name, target string) {
	f.t.Helper()
	p := filepath.Join(f.repo.Dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatal(err)
	}
}

// failGit makes every git call that has the argument arg exit 3; other calls run the real git.
func (f *fx) failGit(arg string) {
	f.t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(f.t.TempDir(), "git")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = '" + arg + "' ] && exit 3; done\n" +
		"exec '" + gitPath + "' \"$@\"\n"
	//nolint:gosec // G306: the fake git must be executable.
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		f.t.Fatal(err)
	}
	f.tools["git"] = p
}

// wantStopped asserts a warn with context containing want, before the model and the stamp.
func (f *fx) wantStopped(resp Response, want string) {
	f.t.Helper()
	if resp.Status != StatusWarn || !strings.Contains(resp.Context, want) {
		f.t.Fatalf("resp %+v, want warn with %q", resp, want)
	}
	if len(f.calls("claude")) != 0 {
		f.t.Fatal("claude ran")
	}
	f.noStamp()
}

// I1, I7: .stack/improve.toml must be a regular file at the base commit.
func TestRepoConfigFile(t *testing.T) {
	t.Parallel()
	t.Run("symlink", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.repo.Write("real.toml", "paths = [\"skills/**\"]\n")
		f.repo.Git("rm", "-q", ".stack/improve.toml")
		f.symlink(".stack/improve.toml", "../real.toml")
		f.repo.Git("add", "-A")
		f.repo.Git("commit", "-q", "-m", "link config")
		f.repo.Git("push", "-q", "origin", "main")
		f.wantStopped(f.run(), ".stack/improve.toml is a symlink, not a regular file")
	})
	t.Run("directory", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.repo.Git("rm", "-q", ".stack/improve.toml")
		f.repo.Write(".stack/improve.toml/x", "paths = [\"skills/**\"]\n")
		f.repo.Git("add", "-A")
		f.repo.Git("commit", "-q", "-m", "config dir")
		f.repo.Git("push", "-q", "origin", "main")
		f.wantStopped(f.run(), ".stack/improve.toml is a directory, not a regular file")
	})
	t.Run("lookup fails", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.failGit(".stack/improve.toml")
		f.wantStopped(f.run(), "git ls-tree of .stack/improve.toml failed: exit 3")
	})
}

// I7: the writing key replaces the embedded guide; without it the prompt is unchanged.
func TestWritingGuide(t *testing.T) {
	t.Parallel()
	embedded := "\n## Writing rules\n\n" + writing + "\n## Task\n"
	t.Run("key absent", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
		in := f.calls("claude")[0].Stdin
		if !strings.Contains(in, embedded) || strings.Contains(in, guide) {
			t.Fatalf("stdin lacks the embedded guide:\n%s", in)
		}
	})
	for _, tc := range []struct {
		name, path, mode, env string
	}{
		{name: "100644", path: "docs/writing.md", mode: "100644"},
		{name: "100755", path: "docs/writing.md", mode: "100755"},
		// A name git would read as pathspec magic without --literal-pathspecs.
		{name: "magic name", path: ":(glob)writing.md", mode: "100644"},
		// Inherited pathspec settings git refuses next to --literal-pathspecs.
		{name: "glob env", path: "docs/writing.md", mode: "100644", env: "GIT_GLOB_PATHSPECS=1"},
		{name: "icase env", path: "docs/writing.md", mode: "100644", env: "GIT_ICASE_PATHSPECS=1"},
		{name: "noglob env", path: "docs/writing.md", mode: "100644", env: "GIT_NOGLOB_PATHSPECS=1"},
	} {
		t.Run("key set "+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			if tc.env != "" {
				f.opts.Env = append(f.opts.Env, tc.env)
			}
			f.commitConfig("paths = [\"skills/**\"]\nwriting = \""+tc.path+"\"\n", func() {
				f.repo.Write(tc.path, guide)
				if tc.mode == "100755" {
					p := filepath.Join(f.repo.Dir, filepath.FromSlash(tc.path))
					if err := os.Chmod(p, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			})
			entry := f.repo.Git("--literal-pathspecs", "ls-tree", "HEAD", "--", tc.path)
			if !strings.HasPrefix(entry, tc.mode+" blob ") {
				t.Fatalf("committed entry %q, want mode %s", entry, tc.mode)
			}
			// Only merged content counts: the work tree's copy differs from the base.
			f.repo.Write(tc.path, "uncommitted\n")
			f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
			in := f.calls("claude")[0].Stdin
			if !strings.Contains(in, "\n## Writing rules\n\n"+guide+"\n## Task\n") ||
				strings.Contains(in, "# Writing agent knowledge") ||
				strings.Contains(in, "uncommitted") {
				t.Fatalf("stdin does not hold exactly the target's guide:\n%s", in)
			}
		})
	}
	oid := func(f *fx) string { return f.repo.Git("rev-parse", "HEAD:docs/writing.md") }
	for _, tc := range []struct {
		name, writing string
		setup         func(f *fx)
		want          string
	}{
		{
			name: "inside paths", writing: "skills/a.md",
			want: `writing guide "skills/a.md" matches paths, so improve could edit its own rules`,
		},
		{
			name: "symlink", writing: "docs/writing.md",
			setup: func(f *fx) {
				f.repo.Write("docs/real.md", guide)
				f.symlink("docs/writing.md", "real.md")
			},
			want: `writing guide "docs/writing.md" is a symlink, not a regular file`,
		},
		{
			name: "directory", writing: "docs",
			setup: func(f *fx) { f.repo.Write("docs/writing.md", guide) },
			want:  `writing guide "docs" is a directory, not a regular file`,
		},
		{
			name: "missing", writing: "docs/none.md",
			want: `writing guide "docs/none.md" does not exist at the base commit`,
		},
		{
			name: "lookup fails", writing: "docs/writing.md",
			setup: func(f *fx) {
				f.repo.Write("docs/writing.md", guide)
				f.failGit("docs/writing.md")
			},
			want: `git ls-tree of writing guide "docs/writing.md" failed: exit 3`,
		},
		{
			name: "read fails", writing: "docs/writing.md",
			setup: func(f *fx) { f.repo.Write("docs/writing.md", guide) },
			want:  `cannot read writing guide "docs/writing.md": exit 3`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			f.commitConfig("paths = [\"skills/**\"]\nwriting = \""+tc.writing+"\"\n", func() {
				if tc.setup != nil {
					tc.setup(f)
				}
			})
			if tc.name == "read fails" {
				f.failGit(oid(f))
			}
			f.wantStopped(f.run(), tc.want+"; improve stopped")
		})
	}
	t.Run("invalid key", func(t *testing.T) {
		t.Parallel()
		for _, w := range []string{
			`""`, `"/abs.md"`, `"a/../b.md"`, `"../b.md"`, `"./b.md"`, `"a//b.md"`, `'a\b.md'`,
			`"a/[.md"`, "1", `["a.md"]`,
		} {
			f := newFx(t)
			f.commitConfig("paths = [\"skills/**\"]\nwriting = "+w+"\n", nil)
			resp := f.run()
			f.wantStopped(resp, ".stack/improve.toml writing ")
			f.wantStopped(resp, " is not a valid repository path")
		}
	})
}

// Keys match exactly: a key in another case is unknown and ignored, never bound instead.
func TestRepoConfigKeys(t *testing.T) {
	t.Parallel()
	dual := "Paths = [\"other/**\"]\npaths = [\"skills/**\"]\n" +
		"Writing = \"skills/a.md\"\nwriting = \"docs/writing.md\"\n"
	for i := range 256 {
		cfg, err := repoConfig([]byte(dual))
		if err != nil || !slices.Equal(cfg.paths, []string{"skills/**"}) ||
			cfg.writing != "docs/writing.md" {
			t.Fatalf("run %d: %+v, %v", i, cfg, err)
		}
	}
	for _, bad := range []string{
		"paths = [\"skills/**\"]\nwriting = 1\nWriting = \"x.md\"\n",
		"Paths = [\"skills/**\"]\n",
		"paths = [\"skills/**\", 1]\n",
	} {
		for i := range 256 {
			if _, err := repoConfig([]byte(bad)); err == nil {
				t.Fatalf("run %d: %q accepted", i, bad)
			}
		}
	}
	if cfg, err := repoConfig(
		[]byte("paths = [\"skills/**\"]\nWriting = \"x.md\"\n"),
	); err != nil ||
		cfg.writing != "" {
		t.Fatalf("other-case writing: %+v, %v", cfg, err)
	}
}

func TestParseLsTree(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 40)
	got, err := parseLsTree([]byte("100644 blob " + oid + "\ta b\tc.md\x00120000 blob " + oid +
		"\tl\x00"))
	want := []treeEntry{{"100644", "blob", oid, "a b\tc.md"}, {"120000", "blob", oid, "l"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := parseLsTree(nil); err != nil || got != nil {
		t.Fatalf("empty: %+v, %v", got, err)
	}
	for _, bad := range []string{
		"100644 blob " + oid + "\tx", // no NUL
		"100644 blob\tx\x00",
		"100644 blob " + oid + "\x00",
		"100644  blob " + oid + "\tx\x00",
		"100644 blob " + oid + "\t\x00",
	} {
		if _, err := parseLsTree([]byte(bad)); err == nil {
			t.Errorf("parseLsTree(%q) accepted", bad)
		}
	}
}

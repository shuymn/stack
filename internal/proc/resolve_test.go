package proc

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func touch(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, nil, mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the umask; set the exact mode under test.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestResolveUnix(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("execute bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	touch(t, filepath.Join(dir, "board"), 0o700)
	touch(t, filepath.Join(dir, "group-only"), 0o610)
	touch(t, filepath.Join(dir, "plain"), 0o600)
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "board"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	// Owner-only checks assume a non-root user; root may execute any file
	// with an x bit.
	if os.Geteuid() == 0 {
		t.Skip("effective-permission cases need a non-root user")
	}
	cases := []struct {
		name     string
		want     error
		wantPath bool
	}{
		{name: "board", wantPath: true},
		{name: "group-only", want: ErrNotRunnable, wantPath: true},
		{name: "link", wantPath: true},
		{name: "plain", want: ErrNotRunnable, wantPath: true},
		{name: "subdir", want: ErrNotRunnable, wantPath: true},
		{name: "missing", want: ErrNotFound},
		{name: "board.exe", want: ErrNotFound},
	}
	for _, tc := range cases {
		path, err := resolve("linux", dir, tc.name)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if wantPath := filepath.Join(dir, tc.name); tc.wantPath != (path == wantPath) {
			t.Errorf("%s: path = %q", tc.name, path)
		}
	}
}

// TestResolveWindows runs the Windows rules on every OS; they only inspect
// file names, so the result does not depend on the host.
func TestResolveWindows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		files []string
		dirs  []string
		want  error
		path  string
	}{
		{name: "board", files: []string{"board.exe"}, path: "board.exe"},
		{name: "board.exe", files: []string{"board.exe"}, path: "board.exe"},
		{name: "board", files: []string{"board.cmd", "board.exe"}, path: "board.exe"},
		{name: "board", files: []string{"board"}, want: ErrNotRunnable, path: "board"},
		{name: "board", files: []string{"board.cmd"}, want: ErrNotRunnable, path: "board.cmd"},
		{name: "board", files: []string{"board.bat"}, want: ErrNotRunnable, path: "board.bat"},
		{name: "board", files: []string{"board.ps1"}, want: ErrNotRunnable, path: "board.ps1"},
		{name: "board", files: []string{"board.com"}, want: ErrNotFound},
		{name: "board", dirs: []string{"board.exe"}, want: ErrNotRunnable, path: "board.exe"},
		{name: "board", dirs: []string{"board"}, want: ErrNotRunnable, path: "board"},
		{name: "board", want: ErrNotFound},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		for _, f := range tc.files {
			touch(t, filepath.Join(dir, f), 0o600)
		}
		for _, d := range tc.dirs {
			if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		path, err := resolve("windows", dir, tc.name)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s %v: err = %v, want %v", tc.name, tc.files, err, tc.want)
		}
		want := ""
		if tc.path != "" {
			want = filepath.Join(dir, tc.path)
		}
		if path != want {
			t.Errorf("%s %v: path = %q, want %q", tc.name, tc.files, path, want)
		}
	}
}

// TestResolveUnverifiable covers a candidate stat cannot check: it must not
// read as absent, because merge-check treats absence as a pass.
// TestResolveDanglingSymlink covers a name whose link target is missing: the
// name exists, so it must not read as absent (merge-check passes on absent).
func TestResolveDanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Symlink(
		filepath.Join(dir, "missing"),
		filepath.Join(dir, "merge-check"),
	); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.Symlink(
		filepath.Join(dir, "missing"),
		filepath.Join(dir, "board.exe"),
	); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ goos, name, path string }{
		{"linux", "merge-check", "merge-check"},
		{"windows", "merge-check", "merge-check"},
		{"windows", "board", "board.exe"},
	} {
		path, err := resolve(tc.goos, dir, tc.name)
		if want := filepath.Join(dir, tc.path); !errors.Is(err, ErrNotRunnable) || path != want {
			t.Errorf(
				"%s %s: path = %q, err = %v, want ErrNotRunnable at %q",
				tc.goos,
				tc.name,
				path,
				err,
				want,
			)
		}
	}
}

func TestResolveUnverifiable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix directory permissions enforced for a non-root user")
	}

	dir := filepath.Join(t.TempDir(), ".claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(dir, "merge-check"), 0o700)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	for _, goos := range []string{"linux", "windows"} {
		path, err := resolve(goos, dir, "merge-check")
		if !errors.Is(err, ErrUnverifiable) || path == "" {
			t.Errorf("%s: path = %q, err = %v, want ErrUnverifiable with the path", goos, path, err)
		}
	}
	path, err := lookPath("linux", []string{dir}, "merge-check")
	if !errors.Is(err, ErrUnverifiable) || path == "" {
		t.Errorf("lookPath: path = %q, err = %v, want ErrUnverifiable", path, err)
	}
}

func TestResolveInvalidName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "c:x"} {
		for _, goos := range []string{"linux", "windows"} {
			if _, err := resolve(goos, dir, name); !errors.Is(err, ErrInvalidName) {
				t.Errorf("%s %q: err = %v, want ErrInvalidName", goos, name, err)
			}
		}
	}
}

func TestLookPathWindows(t *testing.T) {
	t.Parallel()

	cmdDir, exeDir, plainDir, emptyDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	touch(t, filepath.Join(cmdDir, "stack-x.cmd"), 0o600)
	touch(t, filepath.Join(exeDir, "stack-x.exe"), 0o600)
	touch(t, filepath.Join(plainDir, "stack-x"), 0o600)

	// A .cmd earlier on PATH than the .exe: the .exe runs.
	path, err := lookPath("windows", []string{emptyDir, cmdDir, exeDir}, "stack-x")
	if err != nil || path != filepath.Join(exeDir, "stack-x.exe") {
		t.Fatalf("path = %q, err = %v", path, err)
	}
	// Only not-runnable candidates: the first one is reported.
	path, err = lookPath("windows", []string{plainDir, cmdDir}, "stack-x")
	if !errors.Is(err, ErrNotRunnable) || path != filepath.Join(plainDir, "stack-x") {
		t.Fatalf("path = %q, err = %v, want ErrNotRunnable", path, err)
	}
	// Nothing at all.
	if _, err := lookPath("windows", []string{emptyDir}, "stack-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLookPathSkipsRelativeEntries(t *testing.T) {
	t.Parallel()

	rel, err := filepath.Rel(mustGetwd(t), t.TempDir())
	if err != nil {
		t.Skip("temp dir not reachable by a relative path")
	}
	abs := filepath.Join(mustGetwd(t), rel)
	touch(t, filepath.Join(abs, "stack-x.exe"), 0o600)
	for _, dirs := range [][]string{{rel}, {"", "."}} {
		if path, err := lookPath("windows", dirs, "stack-x"); !errors.Is(err, ErrNotFound) {
			t.Errorf("dirs %q: path = %q, err = %v, want ErrNotFound", dirs, path, err)
		}
	}
}

func TestLookPathUnixSkipsNotRunnable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix execute bits enforced for a non-root user")
	}

	first, second := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(first, "stack-x"), 0o610) // executable by the group only
	touch(t, filepath.Join(second, "stack-x"), 0o700)
	path, err := lookPath("linux", []string{first, second}, "stack-x")
	if err != nil || path != filepath.Join(second, "stack-x") {
		t.Fatalf("path = %q, err = %v", path, err)
	}
	path, err = lookPath("linux", []string{first}, "stack-x")
	if !errors.Is(err, ErrNotRunnable) || path != filepath.Join(first, "stack-x") {
		t.Fatalf("path = %q, err = %v, want ErrNotRunnable", path, err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	// ErrNotFound reports that no candidate file exists.
	ErrNotFound = errors.New("executable not found")
	// ErrNotRunnable reports a file that exists under the name but that stack
	// does not run on this OS: a Unix file without an execute bit, or on
	// Windows anything but <name>.exe (no extension, .cmd, .bat, .ps1).
	ErrNotRunnable = errors.New("found but not runnable on this OS")
	// ErrInvalidName reports a name that is not a single path component.
	ErrInvalidName = errors.New("executable name is not a single path component")
)

// windowsNotRunnable lists the suffixes that make a Windows file "found but
// not runnable"; os/exec does not run batch files reliably.
var windowsNotRunnable = []string{"", ".cmd", ".bat", ".ps1"}

// ResolveExecutable finds name in dir. On Unix the file must be a regular file
// with an execute bit; on Windows only <name>.exe is runnable. The returned
// path is set on success and with ErrNotRunnable, so callers can name the file
// they refuse to run.
func ResolveExecutable(dir, name string) (string, error) {
	return resolve(runtime.GOOS, dir, name)
}

// LookPathExe walks PATH for name, returning the first runnable match.
// Relative PATH entries, including empty ones and ".", are skipped. On
// Windows only <name>.exe matches and PATHEXT is not consulted. When nothing
// is runnable but some directory holds a not-runnable candidate, the first
// such path is returned with ErrNotRunnable.
func LookPathExe(name string) (string, error) {
	return lookPath(runtime.GOOS, filepath.SplitList(os.Getenv("PATH")), name)
}

func lookPath(goos string, dirs []string, name string) (string, error) {
	notRunnable := ""
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		path, err := resolve(goos, dir, name)
		switch {
		case err == nil:
			return path, nil
		case errors.Is(err, ErrNotRunnable):
			if notRunnable == "" {
				notRunnable = path
			}
		case errors.Is(err, ErrNotFound):
		default:
			return "", err
		}
	}
	if notRunnable != "" {
		return notRunnable, fmt.Errorf("%w: %s", ErrNotRunnable, notRunnable)
	}
	return "", fmt.Errorf("%w: %s on PATH", ErrNotFound, name)
}

func resolve(goos, dir, name string) (string, error) {
	if !validName(name) {
		return "", fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if goos == "windows" {
		return resolveWindows(dir, name)
	}
	path := filepath.Join(dir, name)
	info, ok := regularFile(path)
	switch {
	case !ok:
		return "", fmt.Errorf("%w: %s", ErrNotFound, path)
	case info.Mode().Perm()&0o111 == 0:
		return path, fmt.Errorf("%w: %s", ErrNotRunnable, path)
	default:
		return path, nil
	}
}

func resolveWindows(dir, name string) (string, error) {
	base := name
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".exe") {
		base = strings.TrimSuffix(name, ext)
	}
	exe := filepath.Join(dir, base+".exe")
	if _, ok := regularFile(exe); ok {
		return exe, nil
	}
	for _, suffix := range windowsNotRunnable {
		path := filepath.Join(dir, name+suffix)
		if _, ok := regularFile(path); ok {
			return path, fmt.Errorf("%w: %s", ErrNotRunnable, path)
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, exe)
}

func regularFile(path string) (fs.FileInfo, bool) {
	// Walking a user-controlled PATH is the purpose; the name joined to
	// each entry is one validated component.
	info, err := os.Stat(path) //nolint:gosec // G703: PATH lookup by design.
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	return info, true
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\:`)
}

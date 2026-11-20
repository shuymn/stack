package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	// ErrNotFound reports that no candidate exists: every stat said so.
	ErrNotFound = errors.New("executable not found")
	// ErrNotRunnable reports an entry that exists under the name but that
	// stack does not run: a directory or other non-regular file, a Unix file
	// this process may not execute, or on Windows anything but <name>.exe
	// (no extension, .cmd, .bat, .ps1).
	ErrNotRunnable = errors.New("found but not runnable on this OS")
	// ErrUnverifiable reports that a candidate could not be checked, e.g. a
	// parent directory without search permission. It is neither "absent" nor
	// "runnable"; callers that guard a step refuse on it.
	ErrUnverifiable = errors.New("cannot verify executable")
	// ErrInvalidName reports a name that is not a single path component.
	ErrInvalidName = errors.New("executable name is not a single path component")
)

// windowsNotRunnable lists the suffixes that make a Windows file "found but
// not runnable"; os/exec does not run batch files reliably.
var windowsNotRunnable = []string{"", ".cmd", ".bat", ".ps1"}

// ResolveExecutable finds name in dir. On Unix the file must be a regular file
// that this process may execute (effective permissions, as exec.LookPath
// checks them); on Windows only a regular <name>.exe is runnable. The error
// is ErrNotFound, ErrNotRunnable or ErrUnverifiable. The returned path is set
// on success and with ErrNotRunnable or ErrUnverifiable, so callers can name
// the file they refuse to run.
func ResolveExecutable(dir, name string) (string, error) {
	return resolve(runtime.GOOS, dir, name)
}

// LookPathExe walks PATH for name, returning the first runnable match.
// Relative PATH entries, including empty ones and ".", are skipped. On
// Windows only <name>.exe matches and PATHEXT is not consulted. When nothing
// is runnable but some directory holds a not-runnable candidate, the first
// such path is returned with ErrNotRunnable; failing that, the first
// unverifiable candidate is returned with ErrUnverifiable.
func LookPathExe(name string) (string, error) {
	return lookPath(runtime.GOOS, filepath.SplitList(os.Getenv("PATH")), name)
}

func lookPath(goos string, dirs []string, name string) (string, error) {
	var notRunnable, unverifiable error
	notRunnablePath, unverifiablePath := "", ""
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		path, err := resolve(goos, dir, name)
		switch {
		case err == nil:
			return path, nil
		case errors.Is(err, ErrNotRunnable):
			if notRunnable == nil {
				notRunnable, notRunnablePath = err, path
			}
		case errors.Is(err, ErrUnverifiable):
			if unverifiable == nil {
				unverifiable, unverifiablePath = err, path
			}
		case errors.Is(err, ErrNotFound):
		default:
			return "", err
		}
	}
	if notRunnable != nil {
		return notRunnablePath, notRunnable
	}
	if unverifiable != nil {
		return unverifiablePath, unverifiable
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
	if err := statCandidate(path, true); err != nil {
		return pathUnlessAbsent(path, err), err
	}
	// Effective permission, not "any x bit": a file executable only by its
	// group is not runnable by its owner.
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, fmt.Errorf("%w: %s: %w", ErrUnverifiable, path, err)
	}
	if _, err := exec.LookPath(abs); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return path, fmt.Errorf("%w: %s", ErrNotRunnable, path)
		}
		return path, fmt.Errorf("%w: %s: %w", ErrUnverifiable, path, err)
	}
	return path, nil
}

func resolveWindows(dir, name string) (string, error) {
	base := name
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".exe") {
		base = strings.TrimSuffix(name, ext)
	}
	exe := filepath.Join(dir, base+".exe")
	if err := statCandidate(exe, true); !errors.Is(err, ErrNotFound) {
		return pathUnlessAbsent(exe, err), err
	}
	for _, suffix := range windowsNotRunnable {
		path := filepath.Join(dir, name+suffix)
		if err := statCandidate(path, false); !errors.Is(err, ErrNotFound) {
			return path, err
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, exe)
}

// statCandidate classifies path: nil for a regular file when runnable is
// allowed, ErrNotFound only when stat confirms absence, ErrNotRunnable for
// anything else that exists, and ErrUnverifiable for other stat errors.
func statCandidate(path string, runnable bool) error {
	// Walking a user-controlled PATH is the purpose; the name joined to
	// each entry is one validated component.
	info, err := os.Stat(path) //nolint:gosec // G703: PATH lookup by design.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	case err != nil:
		return fmt.Errorf("%w: %s: %w", ErrUnverifiable, path, err)
	case !runnable || !info.Mode().IsRegular():
		return fmt.Errorf("%w: %s", ErrNotRunnable, path)
	default:
		return nil
	}
}

func pathUnlessAbsent(path string, err error) string {
	if errors.Is(err, ErrNotFound) {
		return ""
	}
	return path
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\:`)
}

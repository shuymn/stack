package guard

import (
	"os"
	"path"
	"strings"

	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/shell"
)

// rmRoot denies a recursive `rm` of `/`, the home directory, a drive root or an existing `.git`
// directory. Only static operands count; a relative one under an unknown cwd is not judged, and
// child paths, globs and scripts are out of scope.
func rmRoot(calls []shell.Call, opts Options) (Decision, bool) {
	for _, c := range calls {
		if c.Name() != "rm" {
			continue
		}
		recursive, operands := rmArgs(c.Args[1:])
		if !recursive {
			continue
		}
		for _, op := range operands {
			p, ok := paths.Resolve(opts.Home, c.Cwd, op.Text, op.Tilde)
			if !ok {
				continue
			}
			if what, ok := rootKind(p, opts.Home); ok {
				return deny(RuleRmRoot, "rm would recursively delete %s (%s). Delete only what "+
					"the task needs, by its own path.", what, op.Text)
			}
		}
	}
	return Decision{}, false
}

// rmArgs reports whether rm's options include a recursive flag and returns its static operands.
// GNU rm permutes, so options count anywhere before `--`; every word after `--` is an operand.
func rmArgs(args []shell.Word) (bool, []shell.Word) {
	recursive := false
	var operands []shell.Word
	opts := true
	for _, a := range args {
		t := a.Text
		switch {
		case !a.Static:
		case opts && t == "--":
			opts = false
		case opts && t == "--recursive":
			recursive = true
		case opts && strings.HasPrefix(t, "--"):
		case opts && len(t) > 1 && t[0] == '-':
			if strings.ContainsAny(t[1:], "rR") {
				recursive = true
			}
		default:
			operands = append(operands, a)
		}
	}
	return recursive, operands
}

// rootKind names what p is when rm must not delete it.
func rootKind(p, home string) (string, bool) {
	if p == paths.ShellRoot {
		return "the Git Bash root", true
	}
	n := paths.Norm(p)
	switch {
	case n == "/":
		return "the root directory", true
	case len(n) == 3 && n[1] == ':' && n[2] == '/':
		return "a drive root", true
	case home != "" && paths.Same(p, home):
		return "the home directory", true
	case path.Base(n) == ".git":
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
			return "a .git directory", true
		}
	}
	return "", false
}

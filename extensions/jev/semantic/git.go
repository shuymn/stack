package semantic

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/shuymn/stack/extensions/internal/checkpub"
	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
)

const (
	maxLog  = 8 << 20
	maxDiff = 32 << 20
)

var errPrepDeadline = errors.New("preparation deadline")

// gitError is a git command that exited non-zero or whose output was cut.
type gitError struct {
	cmd  string
	exit int
	cut  bool
}

func (e *gitError) Error() string {
	if e.cut {
		return e.cmd + ": output cut"
	}
	return e.cmd + " exited " + strconv.Itoa(e.exit)
}

// Git runs the two git commands of §3.2 on the repository at Dir (the cwd when "").
type Git struct {
	Runner proc.Runner
	// Name is the executable; "" means git.
	Name string
	Dir  string
	// Env is the base environment; GitEnv is applied to it.
	Env []string
}

// configArgs beat repository-local config that would change the bodies (§3.2).
func configArgs() []string {
	kvs := []string{
		"core.quotePath=false", "diff.noprefix=false", "diff.mnemonicPrefix=false",
		"diff.suppressBlankEmpty=false", "diff.relative=false", "diff.renames=false",
		"diff.context=3", "diff.interHunkContext=0", "diff.algorithm=myers",
		"diff.indentHeuristic=true", "diff.orderFile=" + os.DevNull,
		"i18n.logOutputEncoding=UTF-8",
	}
	args := make([]string, 0, len(kvs)+len(kvs))
	for _, kv := range kvs {
		args = append(args, "-c", kv)
	}
	return args
}

// GitEnv is env without the repository-location variables and the config passed through the
// environment, with system config off and global config the null device.
func GitEnv(env []string) []string {
	env = checkpub.DropRepoVars(env)
	env = slices.DeleteFunc(env, func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		name = strings.ToUpper(name)
		return name == "GIT_CONFIG_PARAMETERS" || name == "GIT_CONFIG_COUNT" ||
			strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(name, "GIT_CONFIG_VALUE_") ||
			name == "GIT_CONFIG_NOSYSTEM" || name == "GIT_CONFIG_GLOBAL" ||
			name == "GIT_DIFF_OPTS"
	})
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

// Commits returns the messages of rng, newest first, LF-normalised.
func (g Git) Commits(ctx context.Context, rng string) ([]string, error) {
	out, err := g.run(ctx, "git log", maxLog, "log", "-z", "--encoding=UTF-8",
		"--no-show-signature", "--format=%B", "--end-of-options", rng)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(lf(out), "\x00")
	if fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	return fields, nil
}

// Diff returns rng's diff, LF-normalised.
func (g Git) Diff(ctx context.Context, rng string) (string, error) {
	out, err := g.run(ctx, "git diff", maxDiff, "diff", "--text", "--no-ext-diff",
		"--no-textconv", "--no-color", "--no-renames", "--no-relative",
		"--ignore-submodules=none", "--submodule=short", "--full-index", "--src-prefix=a/",
		"--dst-prefix=b/", "-U3", "--end-of-options", rng, "--")
	if err != nil {
		return "", err
	}
	return lf(out), nil
}

func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// run runs git with a timeout ChildMargin before ctx's deadline. Reaching the deadline is
// errPrepDeadline, checked before the exit status.
func (g Git) run(ctx context.Context, name string, limit int64, args ...string) (string, error) {
	cctx := ctx
	if d, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		cctx, cancel = context.WithDeadline(ctx, d.Add(-jev.ChildMargin))
		defer cancel()
	}
	exe := g.Name
	if exe == "" {
		exe = "git"
	}
	res, err := g.Runner.Run(cctx, proc.Cmd{
		Name: exe, Args: slices.Concat(configArgs(), args), Dir: g.Dir,
		Env: GitEnv(g.Env), MaxStdout: limit,
	})
	switch {
	case res.TimedOut || cctx.Err() != nil:
		return "", errPrepDeadline
	case err != nil:
		return "", &gitError{cmd: name, exit: -1}
	case res.ExitCode != 0:
		return "", &gitError{cmd: name, exit: res.ExitCode}
	case res.Truncated || res.WaitDelayExceeded:
		return "", &gitError{cmd: name, exit: res.ExitCode, cut: true}
	}
	return string(res.Stdout), nil
}

// SplitDiff splits a diff into one unit per file, in diff order.
func SplitDiff(diff string) []jev.Unit {
	var units []jev.Unit
	start := -1
	flush := func(end int) {
		if start >= 0 {
			text := diff[start:end]
			units = append(units, jev.Unit{Name: fileName(text), Text: text})
		}
	}
	for i := 0; i < len(diff); {
		if strings.HasPrefix(diff[i:], "diff --git ") {
			flush(i)
			start = i
		}
		next := strings.IndexByte(diff[i:], '\n')
		if next < 0 {
			break
		}
		i += next + 1
	}
	flush(len(diff))
	return units
}

// fileName is the path of a `diff --git a/P b/P` header (no renames, so both sides match),
// C-unquoted when git quoted it.
func fileName(section string) string {
	header, _, _ := strings.Cut(section, "\n")
	rest := strings.TrimPrefix(header, "diff --git ")
	if strings.HasPrefix(rest, `"`) {
		if name, ok := unquoteC(rest); ok {
			return strings.TrimPrefix(name, "a/")
		}
		return rest
	}
	// "a/P b/P": the path is half of what the prefixes and the space leave.
	n := (len(rest) - len("a/ b/")) / 2 //nolint:mnd // two equal sides
	if n <= 0 || !strings.HasPrefix(rest, "a/") {
		return rest
	}
	return rest[2 : 2+n]
}

// unquoteC decodes the first C-quoted string git wrote at the start of s.
func unquoteC(s string) (string, bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			return b.String(), true
		case c != '\\' || i+1 >= len(s):
			b.WriteByte(c)
		default:
			i++
			switch e := s[i]; e {
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'v':
				b.WriteByte('\v')
			default:
				if e >= '0' && e <= '3' && i+2 < len(s) {
					if v, err := strconv.ParseUint(s[i:i+3], 8, 8); err == nil {
						b.WriteByte(byte(v))
						i += 2
						continue
					}
				}
				b.WriteByte(e)
			}
		}
	}
	return "", false
}

package public

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// built holds the binaries TestMain builds: the real stack and stack-public, for the comparison
// with `stack check public` and the `stack run outbound` round trip.
var built struct {
	dir, stack, public string
	err                error
}

func TestMain(m *testing.M) {
	proctest.Main()
	cleanup := buildBinaries()
	code := gittest.Main(m)
	cleanup()
	os.Exit(code)
}

func buildBinaries() func() {
	dir, err := os.MkdirTemp("", "stack-public-test-")
	if err != nil {
		built.err = err
		return func() {}
	}
	built.dir = dir
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	built.stack = filepath.Join(dir, "stack"+exe)
	built.public = filepath.Join(dir, "stack-public"+exe)
	env := gittest.CleanEnv(os.Environ())
	for _, c := range [][]string{
		{"build", "-o", built.stack, "../../cmd/stack"},
		{"build", "-o", built.public, "./cmd/stack-public"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := exec.CommandContext(ctx, "go", c...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			built.err = fmt.Errorf("go %s: %w: %s", strings.Join(c, " "), err, out)
			break
		}
	}
	return func() { _ = os.RemoveAll(dir) }
}

func needBuilt(t *testing.T) {
	t.Helper()
	if built.err != nil {
		t.Fatalf("building the test binaries failed: %v", built.err)
	}
}

// local builds an absolute local path with a trailing slash at run time, so this file publishes no
// literal one: local("Users", "a").
func local(parts ...string) string {
	return "/" + strings.Join(parts, "/") + "/"
}

type result struct {
	code           int
	stdout, stderr string
}

// runMain runs stack-public in process with home, args and stdin.
func runMain(t *testing.T, home, stdin string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(t.Context(), args, Env{
		Stdin: strings.NewReader(stdin), Stdout: &stdout, Stderr: &stderr, Home: home,
	})
	return result{code, stdout.String(), stderr.String()}
}

// runBinary runs a built binary in dir with the fixture's environment, home as HOME and the built
// directory first on PATH.
func runBinary(
	t *testing.T,
	r *gittest.Repo,
	home, dir, stdin, name string,
	args ...string,
) result {
	t.Helper()
	env := slices.DeleteFunc(slices.Clone(r.Env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(name, "HOME") || strings.EqualFold(name, "USERPROFILE") ||
			strings.EqualFold(name, "PATH")
	})
	env = append(env, "HOME="+home, "USERPROFILE="+home,
		"PATH="+built.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := cmd.ProcessState.ExitCode()
	if err != nil && code < 0 {
		t.Fatalf("%s %q: %v", name, args, err)
	}
	return result{code, stdout.String(), stderr.String()}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkRepo makes a fixture repository with an empty base commit tagged `base` and one commit on top
// whose message is msg, and changes into it. The git that stack-public runs inherits the process
// environment, so the process gets the fixture's git variables and no inherited ones. Not
// parallel: it changes the working directory and the environment.
func checkRepo(t *testing.T, msg string) *gittest.Repo {
	t.Helper()
	r := gittest.New(t)
	r.Commit("chore: base")
	r.Tag("base")
	r.Commit(msg)
	r.SetProcessEnv(t)
	t.Chdir(r.Dir)
	return r
}

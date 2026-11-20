package kit

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// helperEnv selects a helper role when the test binary is re-executed as a contending process.
const helperEnv = "STACK_KIT_TEST_HELPER"

func TestMain(m *testing.M) {
	proctest.Main()
	if op := os.Getenv(helperEnv); op != "" {
		os.Exit(helper(op))
	}
	os.Exit(m.Run())
}

// helper runs one contending process. Arguments come from the environment:
// HOME_DIR, ID, SLUGS (comma-separated), ROUNDS.
//
//	add     adds each slug, ROUNDS times over
//	cycle   adds then removes each slug, ROUNDS times over
//	read    lists ROUNDS times; each list must contain every slug in SLUGS, without duplicates
//	hold    takes the lock, prints "locked", and holds it until stdin closes or the process dies
func helper(op string) int {
	home, id := os.Getenv("HOME_DIR"), os.Getenv("ID")
	slugs := strings.Split(os.Getenv("SLUGS"), ",")
	rounds, _ := strconv.Atoi(os.Getenv("ROUNDS"))
	s := Store{Home: home, Timeout: 30 * time.Second}
	ctx := context.Background()
	fail := func(err error) int {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch op {
	case "add", "cycle":
		for range rounds {
			for _, slug := range slugs {
				if err := s.Add(ctx, id, slug); err != nil {
					return fail(err)
				}
				if op == "cycle" {
					if err := s.Remove(ctx, id, slug); err != nil {
						return fail(err)
					}
				}
			}
		}
	case "read":
		for range rounds {
			got, err := s.List(ctx, id)
			if err != nil {
				return fail(err)
			}
			for _, slug := range slugs {
				if !slices.Contains(got, slug) {
					return fail(fmt.Errorf("read %q: missing %q", got, slug))
				}
			}
			if len(slices.Compact(slices.Sorted(slices.Values(got)))) != len(got) {
				return fail(fmt.Errorf("read %q: duplicates", got))
			}
		}
	case "hold":
		//nolint:gosec // G703: home is the test's own temp dir.
		if err := os.MkdirAll(paths.SessionsDir(home), 0o700); err != nil {
			return fail(err)
		}
		lock := flock.New(paths.SessionLockFile(home, id))
		if err := lock.Lock(); err != nil {
			return fail(err)
		}
		_, _ = fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
	default:
		return fail(fmt.Errorf("unknown helper %q", op))
	}
	return 0
}

// startHelper re-executes the test binary as a separate process running op.
func startHelper(t *testing.T, op, home, id string, slugs []string, rounds int) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(),
		helperEnv+"="+op,
		"HOME_DIR="+home,
		"ID="+id,
		"SLUGS="+strings.Join(slugs, ","),
		"ROUNDS="+strconv.Itoa(rounds),
	)
	cmd.Stderr = &strings.Builder{}
	return cmd
}

// holdLock starts a separate process holding id's lock and returns once it holds it. release
// closes its stdin so it exits; the process is also killed at cleanup.
func holdLock(t *testing.T, home, id string) (*exec.Cmd, func()) {
	t.Helper()
	cmd := startHelper(t, "hold", home, id, nil, 0)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("lock holder: %q, %v; stderr: %s", line, err, cmd.Stderr)
	}
	return cmd, func() { _ = stdin.Close() }
}

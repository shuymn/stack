package kit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/paths"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// sessionFiles lists the names in the sessions dir.
func sessionFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(paths.SessionsDir(home))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := t.TempDir()
	s := Store{Home: home}
	kits := paths.SessionKitsFile(home, "demo-1")

	for _, slug := range []string{"b", "a", "b", "c"} {
		if err := s.Add(ctx, "demo-1", slug); err != nil {
			t.Fatalf("Add(%s): %v", slug, err)
		}
	}
	if got := readFile(t, kits); got != "b\na\nc\n" {
		t.Fatalf(".kits = %q, want b, a, c once each in order", got)
	}
	if err := s.Remove(ctx, "demo-1", "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "demo-1", "never-opened"); err != nil {
		t.Fatalf("Remove of an absent slug: %v", err)
	}
	if got, err := s.List(ctx, "demo-1"); err != nil || !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("List = %q, %v; want [b c]", got, err)
	}

	// One id in two cases is one file; a different id is independent.
	if err := s.Add(ctx, "DEMO-1", "d"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, "other", "z"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(ctx, "demo-1"); !slices.Equal(got, []string{"b", "c", "d"}) {
		t.Fatalf("List(demo-1) = %q, want [b c d]", got)
	}
	if got, _ := s.List(ctx, "other"); !slices.Equal(got, []string{"z"}) {
		t.Fatalf("List(other) = %q, want [z]", got)
	}

	for _, slug := range []string{"b", "c", "d", "d"} {
		if err := s.Remove(ctx, "demo-1", slug); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(kits); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("emptied .kits still exists: %v", err)
	}
	if _, err := os.Stat(paths.SessionLockFile(home, "demo-1")); err != nil {
		t.Fatalf("lock file must never be deleted: %v", err)
	}
	if got := sessionFiles(
		t,
		home,
	); !slices.Equal(
		got,
		[]string{"demo-1.lock", "other.kits", "other.lock"},
	) {
		t.Fatalf("sessions dir = %q; no temp files may remain", got)
	}
}

func TestStoreReadsCRLFAndSkipsInvalidLines(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(paths.SessionsDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	kits := paths.SessionKitsFile(home, "id")
	if err := os.WriteFile(kits, []byte("a\r\n../x\r\n\r\na\r\nb"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Store{Home: home}.List(context.Background(), "id")
	if err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("List = %q, %v; want [a b]", got, err)
	}
}

func TestStoreRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := t.TempDir()
	s := Store{Home: home}
	for _, slug := range []string{"demo.", "con.txt", "..", "../x", "A", ""} {
		if err := s.Add(ctx, "id", slug); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("Add(%q) = %v, want ErrInvalidSlug", slug, err)
		}
		if err := s.Remove(ctx, "id", slug); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidSlug", slug, err)
		}
	}
	for _, id := range []string{"", "../x", "a b", "a/b"} {
		if err := s.Add(ctx, id, "demo"); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("Add with id %q = %v, want ErrInvalidSessionID", id, err)
		}
	}
	if _, err := os.Stat(paths.StateRoot(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid input wrote state: %v", err)
	}
}

// TestStoreProcesses runs the contenders as separate processes (the test binary re-executed), so
// the OS lock, not an in-process mutex, is what serialises them.
func TestStoreProcesses(t *testing.T) {
	t.Parallel()

	t.Run("contending writers", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		var slugs [2][]string
		for i := range 40 {
			slugs[0] = append(slugs[0], fmt.Sprintf("a%d", i))
			slugs[1] = append(slugs[1], fmt.Sprintf("b%d", i))
		}
		a := startHelper(t, "add", home, "s1", slugs[0], 1)
		b := startHelper(t, "add", home, "s1", slugs[1], 1)
		runAll(t, a, b)
		got, err := Store{Home: home}.List(context.Background(), "s1")
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Sorted(slices.Values(slices.Concat(slugs[0], slugs[1])))
		if !slices.Equal(slices.Sorted(slices.Values(got)), want) {
			t.Fatalf("after two writers, list has %d of %d slugs: %q", len(got), len(want), got)
		}
	})

	t.Run("writer against reader", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		if err := (Store{Home: home}).Add(context.Background(), "s2", "base"); err != nil {
			t.Fatal(err)
		}
		w := startHelper(t, "cycle", home, "s2", []string{"x", "y"}, 30)
		r := startHelper(t, "read", home, "s2", []string{"base"}, 120)
		runAll(t, w, r)
		got, _ := Store{Home: home}.List(context.Background(), "s2")
		if !slices.Equal(got, []string{"base"}) {
			t.Fatalf("list = %q, want [base]", got)
		}
	})

	t.Run("interleaved open and close", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		ctx := context.Background()
		if err := (Store{Home: home}).Add(ctx, "s3", "keep"); err != nil {
			t.Fatal(err)
		}
		runAll(t,
			startHelper(t, "cycle", home, "s3", []string{"x"}, 40),
			startHelper(t, "cycle", home, "s3", []string{"y"}, 40),
			startHelper(t, "add", home, "s3", []string{"z"}, 1),
		)
		got, _ := Store{Home: home}.List(ctx, "s3")
		if !slices.Equal(got, []string{"keep", "z"}) {
			t.Fatalf("list = %q, want [keep z]", got)
		}
		if names := sessionFiles(t, home); !slices.Equal(names, []string{"s3.kits", "s3.lock"}) {
			t.Fatalf("sessions dir = %q; no temp files may remain", names)
		}
	})

	t.Run("lock held past the timeout", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		ctx := context.Background()
		s := Store{Home: home, Timeout: 200 * time.Millisecond}
		if err := s.Add(ctx, "s4", "a"); err != nil {
			t.Fatal(err)
		}
		kits := paths.SessionKitsFile(home, "s4")
		before := readFile(t, kits)

		holder, _ := holdLock(t, home, "s4")
		start := time.Now()
		err := s.Add(ctx, "s4", "b")
		if !errors.Is(err, ErrLocked) || err.Error() != "s4.kits is locked by another process" {
			t.Fatalf("Add under a held lock = %v, want ErrLocked naming s4.kits", err)
		}
		if waited := time.Since(start); waited < 200*time.Millisecond {
			t.Fatalf("gave up after %v, before the timeout", waited)
		}
		if err := s.Remove(ctx, "s4", "a"); !errors.Is(err, ErrLocked) {
			t.Fatalf("Remove under a held lock = %v, want ErrLocked", err)
		}
		if _, err := s.List(ctx, "s4"); !errors.Is(err, ErrLocked) {
			t.Fatalf("List under a held lock = %v, want ErrLocked (readers take the lock too)", err)
		}
		if after := readFile(t, kits); after != before {
			t.Fatalf(".kits changed under a held lock: %q -> %q", before, after)
		}

		// A different id never shares the lock.
		if err := s.Add(ctx, "s5", "a"); err != nil {
			t.Fatalf("other id blocked: %v", err)
		}

		// The OS releases the lock when its holder dies; nothing is reclaimed by age.
		if err := holder.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = holder.Wait()
		if err := s.Add(ctx, "s4", "b"); err != nil {
			t.Fatalf("Add after the holder died: %v", err)
		}
		if got := readFile(t, kits); got != "a\nb\n" {
			t.Fatalf(".kits = %q, want a, b", got)
		}
	})

	t.Run("canceled context is not a lock timeout", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		_, release := holdLock(t, home, "s6")
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := Store{Home: home, Timeout: time.Second}.Add(ctx, "s6", "a")
		if err == nil || errors.Is(err, ErrLocked) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Add with a canceled context = %v", err)
		}
	})
}

// runAll starts every cmd, then waits for all and fails on any non-zero exit.
func runAll(t *testing.T, cmds ...*exec.Cmd) {
	t.Helper()
	for _, c := range cmds {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("contender %d: %v; stderr: %s", i, err, c.Stderr)
		}
	}
}

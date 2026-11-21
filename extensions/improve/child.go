package improve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// pipeGrace bounds the wait for stdio pipes after the child and its group are gone.
	pipeGrace = 2 * time.Second
	// maxStdout caps captured stdout; claude results and diffs stay far below it.
	maxStdout = 32 << 20
	// maxStderr caps captured stderr.
	maxStderr = 64 << 10
	// readBuffer is the pipe read size.
	readBuffer = 32 << 10
)

// errStart reports a child that never started.
var errStart = errors.New("did not start")

// repoVars are the repository-location variables a git hook exports; a child must find its
// repository from its own working directory, as internal/checkpub does for core.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// dropRepoVars returns env without the repository-location variables.
func dropRepoVars(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(
			repoVars,
			func(v string) bool { return strings.EqualFold(v, name) },
		)
	})
}

// command is one child process.
type command struct {
	name  string
	args  []string
	dir   string
	env   []string
	stdin []byte
	// timeout is cut to the context's deadline.
	timeout time.Duration
}

// outcome is what a child left behind.
type outcome struct {
	stdout, stderr []byte
	exit           int
	timedOut       bool
	// truncated: stdout passed maxStdout; stderrCut: stderr passed maxStderr; pipesCut: the pipes
	// were closed at pipeGrace before they reached EOF.
	truncated, stderrCut, pipesCut bool
	// err is non-nil when the child did not start (wrapping errStart) or its pipes failed.
	err error
}

// ok reports a child that exited 0 on its own with complete output.
func (o outcome) ok() bool {
	return o.err == nil && !o.timedOut && !o.truncated && !o.pipesCut && o.exit == 0
}

// describe summarises a failed outcome by its cause only. The child's stderr is never quoted: it
// can hold local paths and inputs, and the summary reaches findings, context and latest.json.
func (o outcome) describe() string {
	switch {
	case errors.Is(o.err, errStart):
		return "did not start"
	case o.err != nil:
		return "wait failed"
	case o.timedOut:
		return "timed out"
	case o.truncated:
		return "output too large"
	case o.pipesCut:
		return "output not fully read"
	default:
		return fmt.Sprintf("exit %d", o.exit)
	}
}

// run starts c in its own process group (macOS, Linux), waits for it at most c.timeout (cut to
// ctx), and kills the whole group at the timeout and again after the child exits, so no
// grandchild outlives the call. On Windows only the direct child is killed.
func run(ctx context.Context, c command) outcome {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return outcome{exit: -1, timedOut: true}
	}
	// The context only bounds the start; run kills the process group itself.
	//nolint:gosec // G204: no shell; the names are git, gh, stack, claude and codex from PATH, argv a list.
	cmd := exec.CommandContext(context.Background(), c.name, c.args...)
	cmd.Dir = c.dir
	cmd.Env = c.env
	setGroup(cmd)

	outR, outW, err := os.Pipe()
	if err != nil {
		return outcome{exit: -1, err: fmt.Errorf("%s: %w: %w", c.name, errStart, err)}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return outcome{exit: -1, err: fmt.Errorf("%s: %w: %w", c.name, errStart, err)}
	}
	ours, theirs := []*os.File{outR, errR}, []*os.File{outW, errW}
	var inW *os.File
	if c.stdin != nil {
		inR, w, err := os.Pipe()
		if err != nil {
			closeAll(append(ours, theirs...)...)
			return outcome{exit: -1, err: fmt.Errorf("%s: %w: %w", c.name, errStart, err)}
		}
		inW = w
		ours, theirs = append(ours, inW), append(theirs, inR)
		cmd.Stdin = inR
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		closeAll(append(ours, theirs...)...)
		return outcome{exit: -1, err: fmt.Errorf("%s: %w: %w", c.name, errStart, err)}
	}
	closeAll(theirs...)

	stdout := &capped{limit: maxStdout}
	stderr := &capped{limit: maxStderr}
	var io sync.WaitGroup
	io.Go(func() { drain(stdout, outR) })
	io.Go(func() { drain(stderr, errR) })
	if inW != nil {
		io.Go(func() {
			_, _ = inW.Write(c.stdin)
			_ = inW.Close()
		})
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	timedOut := false
	var waitErr error
	select {
	case waitErr = <-exited:
	case <-ctx.Done():
		timedOut = true
		killGroup(cmd)
		waitErr = <-exited
	}
	// The direct child is gone; kill what is left of its group (a grandchild whose parent exited
	// first), then give the pipes a bounded time to drain.
	killGroup(cmd)
	done := make(chan struct{})
	go func() {
		io.Wait()
		close(done)
	}()
	pipesCut := false
	select {
	case <-done:
	case <-time.After(pipeGrace):
		pipesCut = true
		closeAll(ours...)
		<-done
	}
	closeAll(ours...)

	out := outcome{
		stdout:    stdout.buf.Bytes(),
		stderr:    stderr.buf.Bytes(),
		exit:      cmd.ProcessState.ExitCode(),
		timedOut:  timedOut,
		truncated: stdout.over,
		pipesCut:  pipesCut,
	}
	if timedOut {
		out.exit = -1
	}
	if _, exitErr := errors.AsType[*exec.ExitError](waitErr); waitErr != nil && !exitErr {
		out.err = fmt.Errorf("%s: wait: %w", c.name, waitErr)
	}
	return out
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// drain copies r into w until EOF or a read error.
func drain(w *capped, r *os.File) {
	buf := make([]byte, readBuffer)
	for {
		n, err := r.Read(buf)
		w.write(buf[:n])
		if err != nil {
			return
		}
	}
}

// capped keeps the first limit bytes and drops the rest, so the child never blocks on a full pipe.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) write(p []byte) {
	room := c.limit - c.buf.Len()
	if len(p) > room {
		c.over = true
		p = p[:max(room, 0)]
	}
	c.buf.Write(p)
}

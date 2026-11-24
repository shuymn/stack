// Package proc runs subprocesses for stack: gh, git, kit boards,
// .claude/merge-check and extensions.
//
// Every run is bounded by its context. stack owns the child's stdio pipes:
// once the child has exited, on its own or killed when the context ended,
// stack waits at most WaitDelay for the pipes to finish and then closes its
// ends, so a grandchild that inherited them cannot hang stack. Closing the
// pipes neither kills grandchildren nor stops their I/O, and on Windows
// killing the child leaves grandchildren running. Stdin is an in-memory byte
// slice, so feeding it cannot block past that bound either.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultWaitDelay bounds how long Run and Stream wait for the pipes after
	// the child exits.
	DefaultWaitDelay = 2 * time.Second
	// defaultMaxStdout caps captured stdout when Cmd.MaxStdout is zero.
	defaultMaxStdout = 1 << 20
	// maxStderr caps stderr captured into Result.Stderr.
	maxStderr = 64 << 10
	// copyBuffer is the read size for pipes stack drains itself.
	copyBuffer = 32 << 10
)

var (
	// ErrTimedOut reports that the runner killed the child at the context
	// deadline.
	ErrTimedOut = errors.New("process timed out")
	// ErrWaitDelay reports that the stdio pipes were still open WaitDelay
	// after the child exited, usually because a grandchild inherited them.
	ErrWaitDelay = errors.New("process pipes outlived the child")
	// ErrExit reports a non-zero exit status from a Stream.
	ErrExit = errors.New("process exited with non-zero status")
	// ErrStart reports that the child never started, so it cannot have done anything.
	ErrStart = errors.New("process did not start")
)

// Cmd describes one subprocess. Args exclude the program name.
type Cmd struct {
	Name string
	Args []string
	// Dir is the working directory; empty means the current one.
	Dir string
	// Env replaces the environment when non-nil; nil inherits it.
	Env []string
	// Stdin is written to the child's standard input, which is then closed;
	// nil means the null device. It is a byte slice rather than a reader so
	// that feeding stdin cannot block past the context and WaitDelay.
	Stdin []byte
	// Stderr, when non-nil, receives the child's stderr as it arrives, without
	// a cap, from a goroutine of the runner; Result.Stderr then stays empty.
	// The writer must not block: pass os.Stderr or a prefixing wrapper of it.
	// After a write error the rest of stderr is discarded and the error is
	// reported (Result.StderrErr; joined into Stream's error). When nil, Run
	// captures stderr into Result.Stderr up to 64 KiB.
	Stderr io.Writer
	// MaxStdout caps the stdout Run captures; zero means 1 MiB.
	MaxStdout int64
}

// Result is the outcome of a run that started. Callers check TimedOut,
// WaitDelayExceeded and Truncated before using Stdout; each is recorded
// independently of the exit status.
type Result struct {
	Stdout []byte
	// Stderr is the captured stderr; empty when Cmd.Stderr was set.
	Stderr []byte
	// ExitCode is the child's exit status. It is -1 on every OS when the
	// runner killed the child because the context ended (Windows would
	// otherwise report 1), and when a signal ended the child.
	ExitCode int
	// TimedOut reports that the runner killed the child at the context
	// deadline.
	TimedOut bool
	// WaitDelayExceeded reports that a stdio pipe was still open WaitDelay
	// after the child exited, so stack closed its end and output may be
	// incomplete.
	WaitDelayExceeded bool
	// Truncated reports stdout over the MaxStdout cap.
	Truncated bool
	// StderrTruncated reports captured stderr over its 64 KiB cap.
	StderrTruncated bool
	// StderrErr is the first error writing to Cmd.Stderr, or nil.
	StderrErr error
}

// Runner runs subprocesses. Tests substitute fakes through this seam.
type Runner interface {
	// Run captures the child's output. The error is non-nil only when the
	// child could not start or the context was canceled; a non-zero exit,
	// a timeout, a WaitDelay overrun and truncation are reported in Result.
	Run(ctx context.Context, c Cmd) (Result, error)
	// Stream passes the child's stdout to consume without a size cap and
	// waits for the child. It returns nil, or the join of consume's error and
	// errors wrapping ErrTimedOut, ErrWaitDelay, ErrExit or the context's
	// error. Stdout that consume leaves unread is discarded; when consume
	// fails the child is killed. Cmd.MaxStdout is ignored.
	Stream(ctx context.Context, c Cmd, consume func(io.Reader) error) error
}

// Exec is the os/exec Runner.
type Exec struct {
	// WaitDelay overrides the 2 s default when positive.
	WaitDelay time.Duration
}

var _ Runner = Exec{}

// Run implements Runner.
func (e Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	limit := c.MaxStdout
	if limit <= 0 {
		limit = defaultMaxStdout
	}
	stdout := &capWriter{limit: limit}
	stderr, captured := stderrSink(c)
	ch, err := e.start(ctx, c, stderr)
	if err != nil {
		return Result{}, err
	}
	ch.io.Go(func() { _ = drain(stdout, ch.stdout) })
	overrun, waitErr := ch.finish(e.waitDelay())

	res := Result{
		Stdout:            stdout.bytes(),
		ExitCode:          ch.cmd.ProcessState.ExitCode(),
		WaitDelayExceeded: overrun,
		StderrErr:         ch.stderrErr,
		Truncated:         stdout.truncated(),
	}
	if captured != nil {
		res.Stderr = captured.bytes()
		res.StderrTruncated = captured.truncated()
	}
	if ch.killed.Load() {
		res.ExitCode = -1
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.TimedOut = true
			return res, nil
		}
		return res, fmt.Errorf("run %s: %w", c.Name, ctx.Err())
	}
	if _, exited := errors.AsType[*exec.ExitError](waitErr); waitErr != nil && !exited {
		return res, fmt.Errorf("run %s: %w", c.Name, waitErr)
	}
	return res, nil
}

// Stream implements Runner.
func (e Exec) Stream(parent context.Context, c Cmd, consume func(io.Reader) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	stderr, captured := stderrSink(c)
	ch, err := e.start(ctx, c, stderr)
	if err != nil {
		return err
	}
	type outcome struct {
		overrun bool
		err     error
	}
	// consume is one of the readers finish waits for.
	ch.io.Add(1)
	finished := make(chan outcome, 1)
	go func() {
		overrun, err := ch.finish(e.waitDelay())
		finished <- outcome{overrun: overrun, err: err}
	}()

	consumeErr := consume(ch.stdout)
	if consumeErr != nil {
		// The rest of stdout is not wanted; stop the child.
		cancel()
	}
	_ = drain(io.Discard, ch.stdout)
	ch.io.Done()
	out := <-finished

	errs := []error{consumeErr}
	killed := ch.killed.Load()
	switch {
	case killed && errors.Is(parent.Err(), context.DeadlineExceeded):
		errs = append(errs, fmt.Errorf("stream %s: %w", c.Name, ErrTimedOut))
	case killed && parent.Err() != nil:
		errs = append(errs, fmt.Errorf("stream %s: %w", c.Name, parent.Err()))
	case killed:
		// Killed because consume failed; its error is already reported.
	case out.err != nil:
		if exitErr, ok := errors.AsType[*exec.ExitError](out.err); ok {
			msg := ""
			if captured != nil {
				msg = string(bytes.TrimSpace(captured.bytes()))
			}
			errs = append(errs, fmt.Errorf("stream %s: %w (status %d): %s",
				c.Name, ErrExit, exitErr.ExitCode(), msg))
		} else {
			errs = append(errs, fmt.Errorf("stream %s: %w", c.Name, out.err))
		}
	}
	if out.overrun {
		errs = append(errs, fmt.Errorf("stream %s: %w", c.Name, ErrWaitDelay))
	}
	if ch.stderrErr != nil {
		errs = append(errs, fmt.Errorf("stream %s: forward stderr: %w", c.Name, ch.stderrErr))
	}
	return errors.Join(errs...)
}

func (e Exec) waitDelay() time.Duration {
	if e.WaitDelay > 0 {
		return e.WaitDelay
	}
	return DefaultWaitDelay
}

// Kill seams, swapped only by non-parallel tests.
var (
	killProcess   = (*os.Process).Kill
	processExited = exited
)

// child is a started subprocess whose stdio pipes stack owns.
type child struct {
	cmd *exec.Cmd
	// killed is set when the runner killed the child because the context
	// ended.
	killed atomic.Bool
	// exitConfirmed is set when the kill failed because the child had
	// already exited.
	exitConfirmed atomic.Bool
	// stdout is stack's read end of the child's stdout.
	stdout *os.File
	// ends are stack's ends of all stdio pipes, closed on a WaitDelay overrun.
	ends []*os.File
	// io counts the goroutines still reading or writing the pipes.
	io sync.WaitGroup
	// stderrErr is the stderr forwarding error; read after io is done.
	stderrErr error
}

// start starts the child with stdout on a pipe the caller reads from
// child.stdout, stderr drained into stderr and Cmd.Stdin written to stdin.
func (e Exec) start(ctx context.Context, c Cmd, stderr io.Writer) (*child, error) {
	//nolint:gosec // G204: no shell; executables are gh/git or user-owned paths (kit board, merge-check, stack-<name> on PATH) that the user already controls, argv is passed as a list
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	// WaitDelay after the kill, os/exec retries it once; a child that
	// survives both still blocks Wait. Pipe handling is done below because
	// os/exec does not own these pipes.
	cmd.WaitDelay = e.waitDelay()
	ch := &child{cmd: cmd}
	cmd.Cancel = func() error {
		err := killProcess(cmd.Process)
		if err == nil {
			ch.killed.Store(true)
			return nil
		}
		if !errors.Is(err, os.ErrProcessDone) && processExited(cmd.Process) {
			// The child exited on its own before the kill (on Windows,
			// TerminateProcess then fails with access denied). Wrapping
			// os.ErrProcessDone makes os/exec inject no error, so the run
			// reports the child's own exit.
			ch.exitConfirmed.Store(true)
			return fmt.Errorf("kill: %w: %w", os.ErrProcessDone, err)
		}
		return fmt.Errorf("kill: %w", err) // os/exec checks errors.Is(os.ErrProcessDone).
	}

	var theirs []*os.File // the child's ends, closed in stack once started
	fail := func(err error) (*child, error) {
		closeFiles(theirs)
		closeFiles(ch.ends)
		return nil, fmt.Errorf("start %s: %w: %w", c.Name, ErrStart, err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	ch.stdout = outR
	ch.ends, theirs = append(ch.ends, outR), append(theirs, outW)
	errR, errW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	ch.ends, theirs = append(ch.ends, errR), append(theirs, errW)
	cmd.Stdout, cmd.Stderr = outW, errW
	var inW *os.File
	if c.Stdin != nil {
		inR, w, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		inW = w
		ch.ends, theirs = append(ch.ends, inW), append(theirs, inR)
		cmd.Stdin = inR
	}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	closeFiles(theirs)

	ch.io.Go(func() { ch.stderrErr = drain(stderr, errR) })
	if inW != nil {
		ch.io.Go(func() {
			// A child that exits without reading makes Write fail; that is
			// not an error of the run.
			_, _ = inW.Write(c.Stdin)
			_ = inW.Close()
		})
	}
	return ch, nil
}

// finish waits for the child to exit, then at most delay for the pipe
// goroutines. On overrun it closes stack's pipe ends, which ends those
// goroutines, and reports true. err is the error of exec.Cmd.Wait.
func (ch *child) finish(delay time.Duration) (bool, error) {
	err := ch.cmd.Wait()
	if err != nil && ch.exitConfirmed.Load() &&
		ch.cmd.ProcessState != nil && ch.cmd.ProcessState.Success() {
		// The child exited 0 on its own. Its stdio are *os.File, so os/exec runs
		// no copying goroutines, and the only error left is from its retry of the
		// kill after WaitDelay, which fails for the same reason the first did.
		err = nil
	}
	done := make(chan struct{})
	go func() {
		ch.io.Wait()
		close(done)
	}()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	overrun := false
	select {
	case <-done:
	case <-timer.C:
		overrun = true
	}
	// On overrun, closing ends the pipe goroutines; then wait for them.
	closeFiles(ch.ends)
	<-done
	if err != nil {
		return overrun, fmt.Errorf("wait: %w", err)
	}
	return overrun, nil
}

func stderrSink(c Cmd) (io.Writer, *capWriter) {
	if c.Stderr != nil {
		return c.Stderr, nil
	}
	captured := &capWriter{limit: maxStderr}
	return captured, captured
}

// drain copies src to dst until EOF or a read error and returns the first
// write error. After dst fails it keeps reading, so the child never blocks on
// a full pipe.
func drain(dst io.Writer, src io.Reader) error {
	buf := make([]byte, copyBuffer)
	var writeErr error
	for {
		n, err := src.Read(buf)
		if n > 0 && writeErr == nil {
			_, writeErr = dst.Write(buf[:n])
		}
		if err != nil {
			if writeErr != nil {
				return fmt.Errorf("write: %w", writeErr)
			}
			return nil
		}
	}
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// capWriter keeps the first limit bytes and discards the rest, so the child
// never blocks on a full pipe. Each capWriter has one writing goroutine and is
// read only after finish has joined it.
type capWriter struct {
	buf   bytes.Buffer
	limit int64
	over  bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.limit - int64(w.buf.Len())
	if int64(len(p)) > room {
		w.over = true
		_, _ = w.buf.Write(p[:max(room, 0)])
	} else {
		_, _ = w.buf.Write(p)
	}
	return len(p), nil
}

func (w *capWriter) bytes() []byte {
	return bytes.Clone(w.buf.Bytes())
}

func (w *capWriter) truncated() bool {
	return w.over
}

// Package proc runs subprocesses for stack: gh, git, kit boards,
// .claude/merge-check and extensions.
//
// Every run is bounded by its context. The runner also sets Cmd.WaitDelay, so
// a child whose grandchild still holds stdout or stderr cannot hang stack.
// WaitDelay only closes stack's ends of the pipes and kills the direct child:
// it neither kills grandchildren nor stops their I/O, and on Windows killing
// the child leaves grandchildren running.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const (
	// DefaultWaitDelay bounds how long Run and Stream wait for pipes after the
	// context ends or the child exits.
	DefaultWaitDelay = 2 * time.Second
	// DefaultMaxStdout caps captured stdout when Cmd.MaxStdout is zero.
	DefaultMaxStdout = 1 << 20
	// maxStderr caps captured stderr. Stderr is diagnostic only, so its
	// truncation is reported but never changes a decision.
	maxStderr = 64 << 10
)

var (
	// ErrTimedOut reports that the context deadline ended a Stream.
	ErrTimedOut = errors.New("process timed out")
	// ErrWaitDelay reports that output pipes were still open WaitDelay after
	// the child exited, usually because a grandchild inherited them.
	ErrWaitDelay = errors.New("process output pipes outlived the child")
	// ErrExit reports a non-zero exit status from a Stream.
	ErrExit = errors.New("process exited with non-zero status")
)

// Cmd describes one subprocess. Args exclude the program name.
type Cmd struct {
	Name string
	Args []string
	// Dir is the working directory; empty means the current one.
	Dir string
	// Env replaces the environment when non-nil; nil inherits it.
	Env []string
	// Stdin is the child's standard input; nil means the null device.
	Stdin io.Reader
	// MaxStdout caps the stdout Run captures; zero means DefaultMaxStdout.
	MaxStdout int64
}

// Result is the outcome of a run that started. Callers check TimedOut,
// WaitDelayExceeded and Truncated before using Stdout.
type Result struct {
	Stdout []byte
	Stderr []byte
	// ExitCode is the exit status, or -1 when the child was killed.
	ExitCode int
	// TimedOut reports that the context deadline ended the child.
	TimedOut bool
	// WaitDelayExceeded reports that the child exited on its own but its
	// output pipes stayed open past WaitDelay. A timed-out run reports only
	// TimedOut.
	WaitDelayExceeded bool
	// Truncated reports stdout over the MaxStdout cap.
	Truncated bool
	// StderrTruncated reports stderr over its internal cap.
	StderrTruncated bool
}

// Runner runs subprocesses. Tests substitute fakes through this seam.
type Runner interface {
	// Run captures the child's output. The error is non-nil only when the
	// child could not start or the context was canceled; a non-zero exit,
	// a timeout, a WaitDelay overrun and truncation are reported in Result.
	Run(ctx context.Context, c Cmd) (Result, error)
	// Stream passes the child's stdout to consume without a size cap and
	// waits for the child. It returns consume's error, or an error wrapping
	// ErrTimedOut, ErrWaitDelay or ErrExit. Stdout that consume leaves unread
	// is discarded. Cmd.MaxStdout is ignored.
	Stream(ctx context.Context, c Cmd, consume func(io.Reader) error) error
}

// Exec is the os/exec Runner.
type Exec struct {
	// WaitDelay overrides DefaultWaitDelay when positive.
	WaitDelay time.Duration
}

var _ Runner = Exec{}

// Run implements Runner.
func (e Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	limit := c.MaxStdout
	if limit <= 0 {
		limit = DefaultMaxStdout
	}
	stdout := &capWriter{limit: limit}
	stderr := &capWriter{limit: maxStderr}
	cmd := e.command(ctx, c)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start %s: %w", c.Name, err)
	}
	waitErr := cmd.Wait()
	res := Result{
		Stdout:          stdout.bytes(),
		Stderr:          stderr.bytes(),
		ExitCode:        cmd.ProcessState.ExitCode(),
		Truncated:       stdout.truncated(),
		StderrTruncated: stderr.truncated(),
	}
	if waitErr == nil {
		return res, nil
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
	case ctx.Err() != nil:
		return res, fmt.Errorf("run %s: %w", c.Name, ctx.Err())
	case errors.Is(waitErr, exec.ErrWaitDelay):
		res.WaitDelayExceeded = true
	}
	return res, nil
}

// Stream implements Runner.
func (e Exec) Stream(parent context.Context, c Cmd, consume func(io.Reader) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	pr, pw := io.Pipe()
	stderr := &capWriter{limit: maxStderr}
	cmd := e.command(ctx, c)
	cmd.Stdout = pw
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", c.Name, err)
	}
	waitErr := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		// Wait has returned, so no more stdout arrives; end the reader.
		_ = pw.Close()
		waitErr <- err
	}()

	consumeErr := consume(pr)
	if consumeErr != nil {
		// The rest of stdout is not wanted; stop the child.
		cancel()
	} else {
		// Discard what consume left unread so the child does not die of a
		// closed pipe; the context and WaitDelay still bound this.
		_, _ = io.Copy(io.Discard, pr)
	}
	_ = pr.CloseWithError(errStreamDone)
	err := <-waitErr

	timedOut := err != nil && errors.Is(parent.Err(), context.DeadlineExceeded)
	if consumeErr != nil {
		if timedOut {
			return errors.Join(consumeErr, fmt.Errorf("stream %s: %w", c.Name, ErrTimedOut))
		}
		return consumeErr
	}
	switch {
	case err == nil:
		return nil
	case timedOut:
		return fmt.Errorf("stream %s: %w", c.Name, ErrTimedOut)
	case parent.Err() != nil:
		return fmt.Errorf("stream %s: %w", c.Name, parent.Err())
	case errors.Is(err, exec.ErrWaitDelay):
		return fmt.Errorf("stream %s: %w", c.Name, ErrWaitDelay)
	case !cmd.ProcessState.Success():
		return fmt.Errorf("stream %s: %w (status %d): %s",
			c.Name, ErrExit, cmd.ProcessState.ExitCode(), bytes.TrimSpace(stderr.bytes()))
	default:
		return fmt.Errorf("stream %s: %w", c.Name, err)
	}
}

func (e Exec) command(ctx context.Context, c Cmd) *exec.Cmd {
	//nolint:gosec // G204: no shell; executables are gh/git or user-owned paths (kit board, merge-check, stack-<name> on PATH) that the user already controls, argv is passed as a list
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	cmd.WaitDelay = DefaultWaitDelay
	if e.WaitDelay > 0 {
		cmd.WaitDelay = e.WaitDelay
	}
	return cmd
}

var errStreamDone = errors.New("stream consumer returned")

// capWriter keeps the first limit bytes and discards the rest, so the child
// never blocks on a full pipe.
type capWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int64
	over  bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
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
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buf.Bytes())
}

func (w *capWriter) truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.over
}

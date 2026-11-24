package proc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// TestSyscallFailed covers the match that limits the exit grace to a denied
// TerminateProcess: the call's name and errno must both match, through
// wrapping.
func TestSyscallFailed(t *testing.T) {
	t.Parallel()
	const denied = syscall.Errno(5) // ERROR_ACCESS_DENIED on Windows.
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"terminate denied", os.NewSyscallError("TerminateProcess", denied), true},
		{"wrapped", fmt.Errorf("kill: %w", os.NewSyscallError("TerminateProcess", denied)), true},
		{"duplicate handle denied", os.NewSyscallError("DuplicateHandle", denied), false},
		{"other errno", os.NewSyscallError("TerminateProcess", syscall.Errno(6)), false},
		{"bare errno", denied, false},
		{"other error", errors.New("denied"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := syscallFailed(tc.err, "TerminateProcess", denied); got != tc.want {
				t.Fatalf("syscallFailed(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

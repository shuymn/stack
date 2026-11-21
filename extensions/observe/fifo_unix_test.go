//go:build !windows

package observe

import "syscall"

func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0o600) //nolint:wrapcheck // test helper
}

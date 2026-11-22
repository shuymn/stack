//go:build unix

package jev

import (
	"io/fs"
	"os"
	"syscall"
)

// currentUID is the user the key file must belong to; a variable only for tests.
var currentUID = os.Getuid

func openKey(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// keyAccess returns why the key file's owner or mode is unsafe, or "".
func keyAccess(fi fs.FileInfo) string {
	if fi.Mode().Perm()&0o077 != 0 {
		return KeyShared
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int64(st.Uid) != int64(currentUID()) {
		return KeyNotOwned
	}
	return ""
}

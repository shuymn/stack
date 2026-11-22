//go:build !unix

package jev

import (
	"io/fs"
	"os"
)

func openKey(p string) (*os.File, error) { return os.Open(p) }

// keyAccess checks nothing: on Windows the profile's ACLs guard the key file, and its mode bits
// do not describe them.
func keyAccess(fs.FileInfo) string { return "" }

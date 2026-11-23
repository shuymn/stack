//go:build !unix

package jev

import (
	"fmt"
	"io/fs"
	"os"
)

func openKey(p string) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open key file: %w", err)
	}
	return f, nil
}

// keyAccess checks nothing: on Windows the profile's ACLs guard the key file, and its mode bits
// do not describe them.
func keyAccess(fs.FileInfo) string { return "" }

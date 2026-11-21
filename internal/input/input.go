// Package input reads the text files a command takes as arguments (`--title-file`, `--body-file`,
// `--prompt-file`, `--replay`): the path is resolved as the shell that passed it means it, and the
// contents must be UTF-8.
package input

import (
	"errors"
	"fmt"
	"os"
	"unicode/utf8"

	"github.com/shuymn/stack/internal/paths"
)

// ErrInput reports an input file that cannot be used.
var ErrInput = errors.New("cannot read")

// ReadFile reads the file named by the argument arg, resolving `~` against home and, on Windows,
// the Git Bash `/c/…` forms; a relative arg is resolved against the current directory.
func ReadFile(home, arg string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("current directory: %w", err)
	}
	p, ok := paths.Resolve(home, cwd, arg, true)
	if !ok {
		return "", fmt.Errorf("%w %s: cannot resolve the path", ErrInput, arg)
	}
	return ReadUTF8(p)
}

// ReadUTF8 reads a whole file, which must be UTF-8.
func ReadUTF8(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w %s: %w", ErrInput, path, err)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%w %s: not UTF-8", ErrInput, path)
	}
	return string(data), nil
}

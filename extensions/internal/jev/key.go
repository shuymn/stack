package jev

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// KeyDisplay is the key file as diagnostics name it, independent of the home directory.
var KeyDisplay = path.Join("~", ".config", "stack", "jev", "key")

// KeyPath is the key file under home: .config/stack/jev/key.
func KeyPath(home string) string {
	return filepath.Join(home, ".config", "stack", "jev", "key")
}

// Causes of an unusable key file (KeyError.Cause).
const (
	KeyUnreadable = "cannot be read"
	KeyNotRegular = "is not a regular file"
	KeyBlank      = "is blank"
	KeyShared     = "is accessible to group or others"
	KeyNotOwned   = "is not owned by you"
)

// keyFixes is the action each cause asks of the owner.
var keyFixes = map[string]string{
	KeyUnreadable: "make it readable by you: chmod 600 " + KeyDisplay,
	KeyNotRegular: "replace it with a regular file holding only the key, or remove it to opt out",
	KeyBlank:      "write the key into it, or remove it to opt out",
	KeyShared:     "chmod 600 " + KeyDisplay,
	KeyNotOwned:   "recreate it as yourself with chmod 600 " + KeyDisplay,
}

// KeyError is a key file that is present but unusable. Its message is fixed: the cause and the
// fix, never the file's content.
type KeyError struct {
	Cause string
}

func (e *KeyError) Error() string {
	return "key file " + KeyDisplay + " " + e.Cause + ": " + keyFixes[e.Cause]
}

// Detail is the finding detail of the error: the cause alone.
func (e *KeyError) Detail() string { return "key file " + e.Cause }

// Key resolves the key (§3.1): a non-empty TYPESAFE_API_KEY, else the content of KeyPath(home),
// both trimmed of surrounding whitespace. No key ("" and nil) is the opt-out: the variable is
// unset or blank and the file is absent (or home is unknown). A present file that cannot be used
// is a *KeyError. On Unix the file must be owned by the current user and closed to group and
// others; on Windows its profile ACLs guard it and no mode is checked.
func Key(getenv func(string) string, home string) (string, error) {
	if getenv != nil {
		if k := strings.TrimSpace(getenv(KeyEnv)); k != "" {
			return k, nil
		}
	}
	if home == "" {
		return "", nil
	}
	// The file is opened without blocking (a FIFO would wait for a writer) and checked through
	// its descriptor, so what is read is what was checked.
	f, err := openKey(KeyPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", &KeyError{Cause: KeyUnreadable}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", &KeyError{Cause: KeyUnreadable}
	}
	if !fi.Mode().IsRegular() {
		return "", &KeyError{Cause: KeyNotRegular}
	}
	if cause := keyAccess(fi); cause != "" {
		return "", &KeyError{Cause: cause}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", &KeyError{Cause: KeyUnreadable}
	}
	k := strings.TrimSpace(string(data))
	if k == "" {
		return "", &KeyError{Cause: KeyBlank}
	}
	return k, nil
}

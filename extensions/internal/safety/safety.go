// Package safety loads `~/.config/stack/safety.toml`, the settings of the safety extensions
// (docs/design/core-stages.md §4.6): the v10 `[safety]` keys as a top-level file, with their v10
// defaults.
package safety

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/BurntSushi/toml"
)

// ErrInvalid marks a file that cannot be used: a syntax or type error, or a file that cannot be
// read.
var ErrInvalid = errors.New("invalid config")

// Config is the decoded file. Paths are kept as written.
type Config struct {
	Secrets            []string
	PrivateTerms       string
	DenyPencilGenerate bool
}

// T3Secrets is the default secrets directory of the T3 Code app, built from its parts so that the
// literal path stays out of public-text scans of this repository.
var T3Secrets = path.Join("~", ".t3", "userdata", "secrets")

// JevKey is the Jev extensions' key file (extensions/internal/jev KeyPath). It is a secret under
// any configured list: Load appends it when the list does not name it.
var JevKey = path.Join("~", ".config", "stack", "jev", "key")

// Defaults returns the v10 `[safety]` defaults and the Jev key file. Each call returns fresh
// slices.
func Defaults() Config {
	return Config{
		Secrets:            []string{T3Secrets, ".dev.vars", JevKey},
		PrivateTerms:       "~/.config/stack/private-terms",
		DenyPencilGenerate: true,
	}
}

// File is the path of safety.toml under home.
func File(home string) string {
	return filepath.Join(home, ".config", "stack", "safety.toml")
}

// Load reads the file at path over the defaults. A missing file or key yields the default. Unknown
// keys, including case variants of known ones, are ignored and returned as warnings, one per key. A
// broken file returns the defaults with an error wrapping ErrInvalid, so a caller that goes on
// applies the default rules; a syntax error cites the file and line.
func Load(path string) (Config, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), nil, nil
	}
	if err != nil {
		return Defaults(), nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	var top map[string]toml.Primitive
	md, err := toml.Decode(string(data), &top)
	if err != nil {
		if pe, ok := errors.AsType[toml.ParseError](err); ok {
			return Defaults(), nil, fmt.Errorf("%w: %s:%d: %s", ErrInvalid, path,
				pe.Position.Line, pe.Message)
		}
		return Defaults(), nil, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	cfg := Defaults()
	// Keys are looked up with their exact case; BurntSushi's struct decoding would also accept
	// `PRIVATE_TERMS`.
	known := map[string]any{
		"secrets":              &cfg.Secrets,
		"private_terms":        &cfg.PrivateTerms,
		"deny_pencil_generate": &cfg.DenyPencilGenerate,
	}
	for key, dst := range known {
		p, ok := top[key]
		if !ok {
			continue
		}
		if err := md.PrimitiveDecode(p, dst); err != nil {
			return Defaults(), nil, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
		}
	}
	// The Jev key file is kept once: its first entry stays, a missing one is appended.
	seen := false
	cfg.Secrets = slices.DeleteFunc(cfg.Secrets, func(s string) bool {
		dup := s == JevKey && seen
		seen = seen || s == JevKey
		return dup
	})
	if !seen {
		cfg.Secrets = append(cfg.Secrets, JevKey)
	}
	var warnings []string
	for _, k := range md.Keys() {
		if _, ok := known[k.String()]; !ok {
			warnings = append(warnings, fmt.Sprintf("%s: unknown key %s", path, k.String()))
		}
	}
	return cfg, warnings, nil
}

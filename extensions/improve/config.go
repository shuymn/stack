package improve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

var (
	errConfig = errors.New("invalid config")
	reGitHub  = regexp.MustCompile(`github\.com[:/]([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)
	// rePublic is a public evidence URL (legacy improve.py:64).
	rePublic = regexp.MustCompile(`^https://github\.com/[\w.-]+/[\w.-]+/(pull|actions/runs)/\d+$`)
)

// credentialVars are the endpoint credentials of Claude Code (legacy improve.py:517-531).
var credentialVars = []string{
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// minCredential is the shortest value treated as a credential.
const minCredential = 8

// enabled reads ~/.config/stack/improve.toml. Only `enabled = true` turns improve on; an absent
// file or any other value is off. Invalid TOML is an error.
func enabled(home string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(home, ".config", "stack", "improve.toml"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: cannot read ~/.config/stack/improve.toml", errConfig)
	}
	var cfg map[string]any
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return false, fmt.Errorf("%w: ~/.config/stack/improve.toml is not valid TOML", errConfig)
	}
	on, ok := cfg["enabled"].(bool)
	return ok && on, nil
}

// repoConfig decodes the target's .stack/improve.toml: `paths` is a required non-empty list of
// valid patterns.
func repoConfig(data []byte) ([]string, error) {
	var cfg struct {
		Paths []string `toml:"paths"`
	}
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: .stack/improve.toml is not valid TOML", errConfig)
	}
	if !md.IsDefined("paths") || len(cfg.Paths) == 0 {
		return nil, fmt.Errorf("%w: .stack/improve.toml needs a non-empty paths list", errConfig)
	}
	for _, p := range cfg.Paths {
		if !validPattern(p) {
			return nil, fmt.Errorf(
				"%w: .stack/improve.toml path %q is not a valid pattern",
				errConfig,
				p,
			)
		}
	}
	return cfg.Paths, nil
}

func validPattern(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" {
			return false
		}
		if _, err := path.Match(seg, ""); err != nil {
			return false
		}
	}
	return true
}

// matchAny reports whether the slash-separated repository path p matches one of patterns.
func matchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if match(strings.Split(pat, "/"), strings.Split(p, "/")) {
			return true
		}
	}
	return false
}

// match matches path segments against pattern segments: `**` is any number of whole directories
// (at least one segment when it ends the pattern, so `skills/**` never matches `skills` itself);
// every other segment is a path.Match pattern within one segment, so `skills/**` never matches
// `skills2/x`.
func match(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if len(pat) == 1 {
			return len(segs) > 0
		}
		for i := 0; i <= len(segs); i++ {
			if match(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	return err == nil && ok && match(pat[1:], segs[1:])
}

// userSettings is what improve reads from ~/.claude/settings.json.
type userSettings struct {
	// additional is true when permissions.additionalDirectories lists anything.
	additional bool
	// creds are the credential values of its env.
	creds []string
}

var errSettings = errors.New("cannot read ~/.claude/settings.json")

// readSettings reads ~/.claude/settings.json; an absent file is empty settings.
func readSettings(home string) (userSettings, error) {
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return userSettings{}, nil
	}
	if err != nil {
		return userSettings{}, errSettings
	}
	var s struct {
		Env         map[string]any `json:"env"`
		Permissions struct {
			AdditionalDirectories json.RawMessage `json:"additionalDirectories"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return userSettings{}, errSettings
	}
	var out userSettings
	switch dirs := strings.TrimSpace(string(s.Permissions.AdditionalDirectories)); dirs {
	case "", "null", "[]":
	default:
		out.additional = true
	}
	for _, k := range credentialVars {
		if v, ok := s.Env[k].(string); ok && len(v) >= minCredential {
			out.creds = append(out.creds, v)
		}
	}
	return out, nil
}

// envCreds returns the credential values set in env.
func envCreds(env []string) []string {
	var out []string
	for _, kv := range env {
		name, v, _ := strings.Cut(kv, "=")
		for _, k := range credentialVars {
			if name == k && len(v) >= minCredential {
				out = append(out, v)
			}
		}
	}
	return out
}

// slugOf parses owner/name from a GitHub remote URL (legacy observe.py:41), or "".
func slugOf(url string) string {
	m := reGitHub.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

// Package config loads `~/.config/stack/config.toml` over the documented defaults
// (docs/architecture.md §5).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/BurntSushi/toml"
)

// SupportedVersion is the only accepted value of the top-level `version` key.
const SupportedVersion = 1

// ErrInvalid marks a config file that cannot be used: a syntax or type error, an unsupported
// version, or a file that cannot be read.
var ErrInvalid = errors.New("invalid config")

// Config is the decoded configuration. Paths are kept as written; callers expand them with
// paths.Expand.
type Config struct {
	Version int
	Kit     Kit
	Worker  Worker
	// Handover lists the `[handover]` entries in file order; the first active one wins.
	Handover []HandoverEntry
	Safety   Safety
	Stages   Stages
}

// Kit is the `[kit]` table.
type Kit struct {
	Root string `toml:"root"`
}

// Worker is the `[worker]` table.
type Worker struct {
	AgentTypes []string `toml:"agent_types"`
	Marker     string   `toml:"marker"`
	Body       string   `toml:"body"`
}

// HandoverEntry is one `ENV_VAR = "path"` line of `[handover]`.
type HandoverEntry struct {
	Env  string
	Path string
}

// Safety is the `[safety]` table.
type Safety struct {
	Secrets            []string `toml:"secrets"`
	PrivateTerms       string   `toml:"private_terms"`
	DenyPencilGenerate bool     `toml:"deny_pencil_generate"`
}

// Stages is the `[stages]` table: extension names per stage.
type Stages struct {
	Entry    []string `toml:"entry"`
	Outbound []string `toml:"outbound"`
	Cycle    []string `toml:"cycle"`
}

// Defaults returns the §5 defaults. Each call returns fresh slices.
func Defaults() Config {
	return Config{
		Version: SupportedVersion,
		Kit:     Kit{Root: "~/.local/state/stack/kits"},
		Worker: Worker{
			AgentTypes: []string{"opus-worker"},
			Marker:     "Follow the body of ~/.claude/agents/opus-worker.md",
			Body:       "~/.claude/agents/opus-worker.md",
		},
		Handover: []HandoverEntry{
			{Env: "T3CODE_THREAD_ID", Path: "~/.config/stack/handover/t3.md"},
			{Env: "HERDR_ENV", Path: "~/.config/stack/handover/herdr.md"},
		},
		Safety: Safety{
			Secrets:            []string{"~/.t3/userdata/secrets", ".dev.vars"},
			PrivateTerms:       "~/.config/stack/private-terms",
			DenyPencilGenerate: true,
		},
		Stages: Stages{
			Entry:    []string{"jev-router"},
			Outbound: []string{"jev-semantic"},
			Cycle:    []string{"observe", "jev-classify", "improve"},
		},
	}
}

// file mirrors the TOML layout; `[handover]` is a map whose order comes from MetaData.Keys.
type file struct {
	Version  int               `toml:"version"`
	Kit      Kit               `toml:"kit"`
	Worker   Worker            `toml:"worker"`
	Handover map[string]string `toml:"handover"`
	Safety   Safety            `toml:"safety"`
	Stages   Stages            `toml:"stages"`
}

// Load reads the config at path over the defaults. A missing file yields the defaults. Unknown keys
// are returned as warnings, one per key. A syntax error cites the file and line, an unsupported
// version cites the file and the key `version`; both wrap ErrInvalid. Arrays replace defaults, and
// a `[handover]` table replaces the default entries.
func Load(path string) (Config, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), nil, nil
	}
	if err != nil {
		return Config{}, nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	def := Defaults()
	f := file{
		Version: def.Version,
		Kit:     def.Kit,
		Worker:  def.Worker,
		Safety:  def.Safety,
		Stages:  def.Stages,
	}
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		if pe, ok := errors.AsType[toml.ParseError](err); ok {
			return Config{}, nil, fmt.Errorf(
				"%w: %s:%d: %s",
				ErrInvalid,
				path,
				pe.Position.Line,
				pe.Message,
			)
		}
		return Config{}, nil, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	if f.Version != SupportedVersion {
		return Config{}, nil, fmt.Errorf("%w: %s: key version: unsupported value %d (want %d)",
			ErrInvalid, path, f.Version, SupportedVersion)
	}

	cfg := Config{
		Version:  f.Version,
		Kit:      f.Kit,
		Worker:   f.Worker,
		Handover: def.Handover,
		Safety:   f.Safety,
		Stages:   f.Stages,
	}
	if md.IsDefined("handover") {
		cfg.Handover = []HandoverEntry{}
		for _, k := range md.Keys() {
			if len(k) == 2 && k[0] == "handover" {
				cfg.Handover = append(
					cfg.Handover,
					HandoverEntry{Env: k[1], Path: f.Handover[k[1]]},
				)
			}
		}
	}

	var warnings []string
	for _, k := range md.Undecoded() {
		warnings = append(warnings, fmt.Sprintf("%s: unknown key %s", path, k.String()))
	}
	return cfg, warnings, nil
}

// LoadForHook is Load for hook commands, which never fail: any error yields the defaults, so a
// typo cannot turn a safety rule off. Warnings are dropped.
func LoadForHook(path string) Config {
	cfg, _, err := Load(path)
	if err != nil {
		return Defaults()
	}
	return cfg
}

// Package config loads `~/.config/stack/config.toml` over the documented defaults
// (docs/architecture.md §5).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/BurntSushi/toml"
)

// supportedVersion is the only accepted value of the top-level `version` key.
const supportedVersion = 1

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
	Stages   Stages
	Merge    Merge
}

// Kit is the `[kit]` table.
type Kit struct {
	Root string
}

// Worker is the `[worker]` table.
type Worker struct {
	AgentTypes []string
	Marker     string
	Body       string
}

// HandoverEntry is one `ENV_VAR = "path"` line of `[handover]`.
type HandoverEntry struct {
	Env  string
	Path string
}

// Stages is the `[stages]` table: extension names per stage.
type Stages struct {
	Entry    []string
	Outbound []string
	Merged   []string
	Cycle    []string
}

// Merge is the `[merge]` table.
type Merge struct {
	// MessageChecks lists the outbound extensions `stack pr merge` requires for --subject/--body.
	MessageChecks []string
}

// Defaults returns the §5 defaults. Each call returns fresh slices.
func Defaults() Config {
	return Config{
		Version: supportedVersion,
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
		Stages: Stages{
			Entry:    []string{"jev-router"},
			Outbound: []string{"public", "jev-semantic"},
			Merged:   []string{"audit-due"},
			Cycle:    []string{"observe", "jev-classify", "improve"},
		},
		Merge: Merge{MessageChecks: []string{}},
	}
}

// section is one TOML table with its values left undecoded, at path. Map keys keep their exact
// case, so each schema key is looked up exactly; BurntSushi's struct decoding would also accept
// `ROOT` for `root`.
type section struct {
	path toml.Key
	m    map[string]toml.Primitive
}

// Load reads the config at path over the defaults. A missing file yields the defaults. Unknown keys,
// including case variants of known ones, are ignored and returned as warnings, one per key. A syntax
// or type error cites the file and line, an unsupported version cites the file and the key
// `version`; both wrap ErrInvalid. Arrays replace defaults, and a `[handover]` table replaces the
// default entries.
func Load(path string) (Config, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), nil, nil
	}
	if err != nil {
		return Config{}, nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	var top section
	md, err := toml.Decode(string(data), &top.m)
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

	cfg := Defaults()
	d := decoder{md: &md, known: map[string]bool{}}
	d.field(top, "version", &cfg.Version)
	if d.err == nil && cfg.Version != supportedVersion {
		return Config{}, nil, fmt.Errorf("%w: %s: key version: unsupported value %d (want %d)",
			ErrInvalid, path, cfg.Version, supportedVersion)
	}

	kit := d.section(top, "kit")
	d.field(kit, "root", &cfg.Kit.Root)

	worker := d.section(top, "worker")
	d.field(worker, "agent_types", &cfg.Worker.AgentTypes)
	d.field(worker, "marker", &cfg.Worker.Marker)
	d.field(worker, "body", &cfg.Worker.Body)

	// Every `[handover]` value must be a string, so decode the table whole; its keys are free-form
	// env names and take their order from MetaData.Keys.
	if _, ok := top.m["handover"]; ok {
		var handover map[string]string
		d.field(top, "handover", &handover)
		cfg.Handover = []HandoverEntry{}
		for _, k := range md.Keys() {
			if len(k) == 2 && k[0] == "handover" {
				d.known[k.String()] = true
				cfg.Handover = append(cfg.Handover, HandoverEntry{Env: k[1], Path: handover[k[1]]})
			}
		}
	}

	stages := d.section(top, "stages")
	d.field(stages, "entry", &cfg.Stages.Entry)
	d.field(stages, "outbound", &cfg.Stages.Outbound)
	d.field(stages, "merged", &cfg.Stages.Merged)
	d.field(stages, "cycle", &cfg.Stages.Cycle)

	merge := d.section(top, "merge")
	d.field(merge, "message_checks", &cfg.Merge.MessageChecks)

	if d.err != nil {
		return Config{}, nil, fmt.Errorf("%w: %s: %w", ErrInvalid, path, d.err)
	}

	var warnings []string
	for _, k := range md.Keys() {
		if !d.known[k.String()] {
			warnings = append(warnings, fmt.Sprintf("%s: unknown key %s", path, k.String()))
		}
	}
	return cfg, warnings, nil
}

// decoder decodes exact schema keys, records them as known, and keeps the first error.
type decoder struct {
	md    *toml.MetaData
	known map[string]bool
	err   error
}

// field decodes s[key] into dst when the key exists with exactly that case.
func (d *decoder) field(s section, key string, dst any) {
	p, ok := s.m[key]
	if !ok || d.err != nil {
		return
	}
	d.known[append(slices.Clip(s.path), key).String()] = true
	if err := d.md.PrimitiveDecode(p, dst); err != nil {
		d.err = err
	}
}

// section returns the table under key; its map is nil when the key is absent.
func (d *decoder) section(s section, key string) section {
	sub := section{path: append(slices.Clip(s.path), key)}
	d.field(s, key, &sub.m)
	return sub
}

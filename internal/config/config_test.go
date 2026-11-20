package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// want5 is docs/architecture.md §5, written out rather than taken from Defaults.
func want5() Config {
	return Config{
		Version: 1,
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

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFile(t *testing.T) {
	cfg, warnings, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load(missing) error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("Load(missing) warnings = %q", warnings)
	}
	if !reflect.DeepEqual(cfg, want5()) {
		t.Fatalf("Load(missing) =\n%+v\nwant\n%+v", cfg, want5())
	}
}

func TestLoadFullDefaultFile(t *testing.T) {
	p := writeConfig(t, `version = 1

[kit]
root = "~/.local/state/stack/kits"

[worker]
agent_types = ["opus-worker"]
marker = "Follow the body of ~/.claude/agents/opus-worker.md"
body = "~/.claude/agents/opus-worker.md"

[handover]
T3CODE_THREAD_ID = "~/.config/stack/handover/t3.md"
HERDR_ENV = "~/.config/stack/handover/herdr.md"

[safety]
secrets = ["~/.t3/userdata/secrets", ".dev.vars"]
private_terms = "~/.config/stack/private-terms"
deny_pencil_generate = true

[stages]
entry = ["jev-router"]
outbound = ["jev-semantic"]
cycle = ["observe", "jev-classify", "improve"]
`)
	cfg, warnings, err := Load(p)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Load(§5 file) = %v, %q", err, warnings)
	}
	if !reflect.DeepEqual(cfg, want5()) {
		t.Fatalf("Load(§5 file) =\n%+v\nwant\n%+v", cfg, want5())
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		wants []string
	}{
		{"syntax error", "version = 1\n[kit]\nroot = = \"x\"\n", []string{":3:"}},
		{"unsupported version", "version = 2\n", []string{"key version", "2"}},
		{"version zero", "version = 0\n", []string{"key version"}},
		{
			"type error",
			"version = 1\n[safety]\ndeny_pencil_generate = \"no\"\n",
			[]string{"line 3"},
		},
		{
			"handover table value",
			"[handover]\nT3CODE_THREAD_ID.path = \"t.md\"\n",
			[]string{"handover"},
		},
		{"handover non-string value", "[handover]\nHERDR_ENV = 1\n", []string{"line 2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeConfig(t, tt.body)
			_, _, err := Load(p)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
			for _, w := range append([]string{p}, tt.wants...) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if got := LoadForHook(p); !reflect.DeepEqual(got, want5()) {
				t.Errorf("LoadForHook = %+v, want §5 defaults", got)
			}
		})
	}

	t.Run("unreadable", func(t *testing.T) {
		dir := t.TempDir()
		if _, _, err := Load(dir); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Load(dir) error = %v, want ErrInvalid", err)
		}
		if got := LoadForHook(dir); !reflect.DeepEqual(got, want5()) {
			t.Errorf("LoadForHook(dir) = %+v, want §5 defaults", got)
		}
	})
}

func TestLoadOverrides(t *testing.T) {
	p := writeConfig(t, `version = 1
mystery = 1

[kit]
root = "/k"
extra = true

[safety]
deny_pencil_generate = false

[stages]
entry = []
`)
	cfg, warnings, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := want5()
	want.Kit.Root = "/k"
	want.Safety.DenyPencilGenerate = false
	want.Stages.Entry = []string{}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load =\n%+v\nwant\n%+v", cfg, want)
	}
	slices.Sort(warnings)
	wantWarnings := []string{p + ": unknown key kit.extra", p + ": unknown key mystery"}
	if !slices.Equal(warnings, wantWarnings) {
		t.Fatalf("warnings = %q, want %q", warnings, wantWarnings)
	}
}

func TestLoadExactKeys(t *testing.T) {
	p := writeConfig(t, `VERSION = 2

[kit]
ROOT = "scratch"

[safety]
deny_pencil_generate = true
Deny_Pencil_Generate = false

[Stages]
entry = []
`)
	cfg, warnings, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, want5()) {
		t.Fatalf("Load =\n%+v\nwant §5 defaults", cfg)
	}
	slices.Sort(warnings)
	want := []string{
		p + ": unknown key Stages",
		p + ": unknown key Stages.entry",
		p + ": unknown key VERSION",
		p + ": unknown key kit.ROOT",
		p + ": unknown key safety.Deny_Pencil_Generate",
	}
	if !slices.Equal(warnings, want) {
		t.Fatalf("warnings = %q, want %q", warnings, want)
	}
}

func TestLoadHandoverOrder(t *testing.T) {
	p := writeConfig(t, `[handover]
HERDR_ENV = "h.md"
ZED = "z.md"
T3CODE_THREAD_ID = "t.md"
`)
	cfg, warnings, err := Load(p)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Load = %v, %q", err, warnings)
	}
	want := []HandoverEntry{{"HERDR_ENV", "h.md"}, {"ZED", "z.md"}, {"T3CODE_THREAD_ID", "t.md"}}
	if !slices.Equal(cfg.Handover, want) {
		t.Fatalf("Handover = %+v, want %+v", cfg.Handover, want)
	}

	cfg, _, err = Load(writeConfig(t, "[handover]\n"))
	if err != nil || len(cfg.Handover) != 0 {
		t.Fatalf("empty [handover] = %+v, %v; want no entries", cfg.Handover, err)
	}
}

func TestDefaultsAreFresh(t *testing.T) {
	a := Defaults()
	a.Safety.Secrets[0] = "changed"
	a.Handover[0].Env = "changed"
	if !reflect.DeepEqual(Defaults(), want5()) {
		t.Fatal("mutating one Defaults() result changed the next")
	}
}

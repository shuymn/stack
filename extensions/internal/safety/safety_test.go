package safety

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "safety.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	// The default T3 secrets directory, built at run time like the package's own value.
	t3 := "~/" + filepath.ToSlash(filepath.Join(".t3", "userdata", "secrets"))
	jevKey := "~/" + filepath.ToSlash(filepath.Join(".config", "stack", "jev", "key"))
	want := Config{
		Secrets:            []string{t3, ".dev.vars", jevKey},
		PrivateTerms:       "~/.config/stack/private-terms",
		DenyPencilGenerate: true,
	}
	for name, path := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "absent.toml"),
		"empty":   write(t, ""),
	} {
		cfg, warnings, err := Load(path)
		if err != nil || len(warnings) != 0 || !reflect.DeepEqual(cfg, want) {
			t.Errorf("%s: Load = %+v, %q, %v; want the defaults", name, cfg, warnings, err)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	p := write(t, `secrets = ["/s"]
private_terms = "~/terms"
deny_pencil_generate = false
PRIVATE_TERMS = "ignored"
mystery = 1

[safety]
private_terms = "v10 table"
`)
	cfg, warnings, err := Load(p)
	want := Config{Secrets: []string{"/s", JevKey}, PrivateTerms: "~/terms"}
	if err != nil || !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load = %+v, %v; want %+v", cfg, err, want)
	}
	slices.Sort(warnings)
	wantWarnings := []string{
		p + ": unknown key PRIVATE_TERMS", p + ": unknown key mystery",
		p + ": unknown key safety", p + ": unknown key safety.private_terms",
	}
	if !slices.Equal(warnings, wantWarnings) {
		t.Errorf("warnings = %q, want %q", warnings, wantWarnings)
	}
}

// TestLoadJevKeyKept: a configured secrets list replaces the defaults but always keeps the Jev
// key file, once.
func TestLoadJevKeyKept(t *testing.T) {
	for body, want := range map[string][]string{
		`secrets = []`:                       {JevKey},
		`secrets = ["/s"]`:                   {"/s", JevKey},
		`secrets = ["` + JevKey + `", "/s"]`: {JevKey, "/s"},
	} {
		cfg, _, err := Load(write(t, body))
		if err != nil || !slices.Equal(cfg.Secrets, want) {
			t.Errorf("%s: Secrets = %q, %v; want %q", body, cfg.Secrets, err, want)
		}
	}
}

// TestLoadBroken checks that a broken file yields the defaults with an ErrInvalid error citing the
// file, so a custom entry is not kept.
func TestLoadBroken(t *testing.T) {
	for name, tt := range map[string]struct{ body, want string }{
		"syntax":        {"private_terms = \"/x\"\nsecrets = = 1\n", ":2:"},
		"type":          {"private_terms = \"/x\"\ndeny_pencil_generate = \"no\"\n", "line 2"},
		"secrets type":  {"secrets = \"/x\"\n", "secrets"},
		"terms not str": {"private_terms = 1\n", "line 1"},
	} {
		p := write(t, tt.body)
		cfg, _, err := Load(p)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), p) ||
			!strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want ErrInvalid with %q", name, err, tt.want)
		}
		if !reflect.DeepEqual(cfg, Defaults()) {
			t.Errorf("%s: cfg = %+v, want the defaults", name, cfg)
		}
	}
	if _, _, err := Load(t.TempDir()); !errors.Is(err, ErrInvalid) {
		t.Errorf("directory: err = %v, want ErrInvalid", err)
	}
}

func TestFile(t *testing.T) {
	home := t.TempDir()
	if got, want := File(
		home,
	), filepath.Join(
		home,
		".config",
		"stack",
		"safety.toml",
	); got != want {
		t.Errorf("File = %q, want %q", got, want)
	}
}

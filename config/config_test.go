package config_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/internal/fake"
)

// These tests use the stand-in kinds of package fake. The fact and action
// kinds whilst registers are tested in package systemd.

func parse(src string) (*config.Config, error) { return config.Parse(src, fake.Registry()) }

func load(path string) (*config.Config, error) { return config.Load(path, fake.Registry()) }

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		`bogus = 1`,
		"[fact.x]\n", // no source
		"[fact.x]\ncommand = \"a\"\nexpr = \"b\"\n",                                            // two sources
		"[fact.x]\ncommand = \"a\"\nevry = \"1m\"\n",                                           // typo
		"[fact.now]\ncommand = \"a\"\n",                                                        // reserved
		"[fact.x]\nexpr = \"a\"\nevery = \"1m\"\n",                                             // every on expr
		"[fact.x]\nexpr = \"a\"\ntransform = \"!value\"\n",                                     // transform on expr
		"[fact.x]\nstate = true\n",                                                             // free state without initial
		"[fact.x]\nstate = [\"a\"]\ninitial = \"b\"\n",                                         // initial not allowed
		"[[rule]]\ninhibit = [\"sleep\"]\n",                                                    // no while
		"[[rule]]\nname = \"a\"\nwhile = \"true\"\n[[rule]]\nname = \"a\"\nwhile = \"true\"\n", // dup
		"[[rule]]\nwhile = \"true\"\nenter = [{ notify = \"a\", command = \"b\" }]\n",
		"[[rule]]\nwhile = \"true\"\nenter = [{ notify = \"a\", timeout = \"1s\" }]\n", // another kind's key
		"[[rule]]\nwhile = \"true\"\nenter = [{ set = { nofact = 1 } }]\n",
		"[[rule]]\nwhile = \"true\"\nenter = [{ notify = \"\" }]\n", // the kind's parser rejects it
		"include = [\"preset:nope\"]\n",
		"include = [\"preset:../x\"]\n",
	} {
		if _, err := parse(src); err == nil {
			t.Errorf("expected an error for:\n%s", src)
		}
	}
}

// Messages list the registered kinds first, then the core's own.
func TestKindsInMessages(t *testing.T) {
	_, err := parse("[fact.x]\n")
	if err == nil || !strings.HasSuffix(err.Error(), "needs a source: one of dbus, dbus_signal, command, expr, state") {
		t.Errorf("no source: %v", err)
	}
	_, err = parse("[[rule]]\nwhile = \"true\"\nenter = [{ bogus = 1 }]\n")
	if err == nil || !strings.HasSuffix(err.Error(), "needs exactly one of notify, command or set") {
		t.Errorf("no action kind: %v", err)
	}
	_, err = parse("include = [\"preset:nope\"]\n")
	if err == nil || !strings.HasSuffix(err.Error(), `include "preset:nope": no such preset (have: upower)`) {
		t.Errorf("no preset: %v", err)
	}
	if _, err := config.Parse("[fact.x]\ncommand = \"a\"\n", nil); err == nil || !strings.HasSuffix(err.Error(), "needs a source: one of expr, state") {
		t.Errorf("without a registry: %v", err)
	}
}

// An action of a kind without a legacy slot is hashed as just its own field
// and Set.
func TestActionHashLayout(t *testing.T) {
	c, err := parse("[fact.m]\nstate = [\"a\", \"b\"]\n[[rule]]\nwhile = \"true\"\nenter = [{ notify = \"hi\" }, { set = { m = \"b\" } }]\nexit = []\n")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"While":"true","Outputs":{},"Enter":[{"kind:notify":"hi","Set":null},{"Set":{"m":"b"}}],"Exit":[]}`
	sum := sha256.Sum256([]byte(want))
	if got := c.Rules[0].Name; got != "rule-"+hex.EncodeToString(sum[:4]) {
		t.Fatalf("name %s, want the hash of %s", got, want)
	}
}

// Facts of the core's own kinds keep the source keys stored in state files.
func TestCoreSourceKeys(t *testing.T) {
	c, err := parse("[fact.e]\nexpr = \"c2 == 'x'\"\n[fact.s]\nstate = [\"off\", \"on\"]\n[fact.s2]\nstate = true\ninitial = 3\n[fact.c2]\ncommand = \"true\"\n")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"e":  `["expr",null,null,null,null,"","c2 == 'x'",null]`,
		"s":  `["state",null,null,null,null,"","",["off","on"]]`,
		"s2": `["state",null,null,null,null,"","",null]`,
		"c2": `["command","true"]`, // the fake kind's own key
	} {
		if got := c.Facts[name].SourceKey(); got != want {
			t.Errorf("%s: source key %s, want %s", name, got, want)
		}
	}
	for name, want := range map[string]bool{"e": true, "s": false, "c2": true} {
		if got := c.Facts[name].Rereadable(); got != want {
			t.Errorf("%s: rereadable %v", name, got)
		}
	}
}

func TestRegistryRejectsClashes(t *testing.T) {
	parser := func(any, map[string]any) (config.FactSpec, error) { return nil, nil }
	action := func(any, map[string]any) (any, error) { return nil, nil }
	for name, add := range map[string]func(*config.Registry){
		"fact kind twice": func(r *config.Registry) { r.AddFactKind(config.FactKind{Name: "command", Parse: parser}) },
		"core fact kind":  func(r *config.Registry) { r.AddFactKind(config.FactKind{Name: "expr", Parse: parser}) },
		"generic key": func(r *config.Registry) {
			r.AddFactKind(config.FactKind{Name: "x", Keys: []string{"type"}, Parse: parser})
		},
		"no parser":         func(r *config.Registry) { r.AddFactKind(config.FactKind{Name: "x"}) },
		"action kind twice": func(r *config.Registry) { r.AddActionKind(config.ActionKind{Name: "notify", Parse: action}) },
		"core action kind":  func(r *config.Registry) { r.AddActionKind(config.ActionKind{Name: "set", Parse: action}) },
		"action, no parser": func(r *config.Registry) { r.AddActionKind(config.ActionKind{Name: "x"}) },
		"fact key names a kind": func(r *config.Registry) {
			r.AddFactKind(config.FactKind{Name: "x", Keys: []string{"dbus"}, Parse: parser})
		},
		"fact key names a core kind": func(r *config.Registry) {
			r.AddFactKind(config.FactKind{Name: "x", Keys: []string{"state"}, Parse: parser})
		},
		"fact kind is another's key": func(r *config.Registry) {
			r.AddFactKind(config.FactKind{Name: "every", Parse: parser})
		},
		"action key names a kind": func(r *config.Registry) {
			r.AddActionKind(config.ActionKind{Name: "x", Keys: []string{"notify"}, Parse: action})
		},
		"action key names a core kind": func(r *config.Registry) {
			r.AddActionKind(config.ActionKind{Name: "x", Keys: []string{"set"}, Parse: action})
		},
		"action kind is another's key": func(r *config.Registry) {
			r.AddActionKind(config.ActionKind{Name: "timeout", Parse: action})
		},
		"hash slot twice": func(r *config.Registry) {
			r.AddActionKind(config.ActionKind{Name: "x", LegacyHashSlot: "X", Parse: action})
			r.AddActionKind(config.ActionKind{Name: "y", LegacyHashSlot: "X", Parse: action})
		},
		"hash slot Set": func(r *config.Registry) {
			r.AddActionKind(config.ActionKind{Name: "x", LegacyHashSlot: "Set", Parse: action})
		},
		"hash slot kind:": func(r *config.Registry) {
			r.AddActionKind(config.ActionKind{Name: "x", LegacyHashSlot: "kind:x", Parse: action})
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			add(fake.Registry())
		}()
	}
}

// A fact kind whose parser returns neither a source nor an error is a bug
// in the kind, reported instead of leaving the fact without a source.
func TestNilFactSpec(t *testing.T) {
	reg := config.NewRegistry()
	reg.AddFactKind(config.FactKind{Name: "x", Parse: func(any, map[string]any) (config.FactSpec, error) { return nil, nil }})
	_, err := config.Parse("[fact.f]\nx = 1\n", reg)
	if err == nil || !strings.Contains(err.Error(), "the x fact kind returned no source") {
		t.Fatalf("got %v", err)
	}
}

func TestIncludeFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "facts.toml"), []byte("[fact.x]\ncommand = \"true\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.toml"), []byte("include = [\"facts.toml\"]\n[[rule]]\nwhile = \"x == ''\"\n"), 0o644)
	c, err := load(filepath.Join(dir, "main.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Facts["x"] == nil || !strings.HasSuffix(c.Facts["x"].Origin, "facts.toml") {
		t.Fatalf("include not loaded: %+v", c.Facts)
	}
	os.WriteFile(filepath.Join(dir, "loop.toml"), []byte("include = [\"loop.toml\"]\n"), 0o644)
	if _, err := load(filepath.Join(dir, "loop.toml")); err == nil {
		t.Fatal("self-include should fail")
	}
	os.WriteFile(filepath.Join(dir, "tick.toml"), []byte("tick = \"1m\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main2.toml"), []byte("include = [\"tick.toml\"]\n"), 0o644)
	if _, err := load(filepath.Join(dir, "main2.toml")); err == nil {
		t.Fatal("tick in an included file should fail")
	}
}

func TestUnnamedRulesAreStable(t *testing.T) {
	a, err := parse("[[rule]]\nwhile = \"true\"\n[[rule]]\nwhile = \"false\"\n[[rule]]\nwhile = \"false\"\n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := parse("[[rule]]\nwhile = \"false\"\n[[rule]]\nwhile = \"true\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if a.Rules[0].Name != b.Rules[1].Name || a.Rules[1].Name != b.Rules[0].Name {
		t.Fatalf("names depend on position: %s %s / %s %s", a.Rules[0].Name, a.Rules[1].Name, b.Rules[0].Name, b.Rules[1].Name)
	}
	if a.Rules[1].Name == a.Rules[2].Name || !strings.HasPrefix(a.Rules[2].Name, a.Rules[1].Name+"-") {
		t.Fatalf("identical rules: %s %s", a.Rules[1].Name, a.Rules[2].Name)
	}
}

func TestStateDoubleLiterals(t *testing.T) {
	if _, err := parse("[fact.level]\ntype = \"double\"\nstate = true\ninitial = 1\n[[rule]]\nwhile = \"true\"\nenter = [{ set = { level = 2 } }]\n"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRecordsFiles(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "config.toml")
	inc := filepath.Join(dir, "more.toml")
	os.WriteFile(main, []byte("include = [\"preset:upower\", \"more.toml\"]\n"), 0o644)
	os.WriteFile(inc, []byte("[fact.x]\nstate = [\"a\"]\ninitial = \"a\"\n"), 0o644)
	cfg, err := load(main)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Files) != 2 || cfg.Files[0] != main || cfg.Files[1] != inc {
		t.Fatalf("files %v, want the main file and the include (not the preset)", cfg.Files)
	}
}

// A required key without a destination is the caller's bug; it is reported
// rather than dereferencing nil.
func TestFieldsRequiredWithoutDestination(t *testing.T) {
	var a string
	err := config.Fields(map[string]any{"a": "x"}, map[string]*string{"a": &a}, "a", "b")
	if err == nil || err.Error() != `required key "b" has no destination` {
		t.Fatalf("got %v", err)
	}
	var empty string
	if err := config.Fields(map[string]any{}, map[string]*string{"a": &empty}, "a"); err == nil || err.Error() != "a is required" {
		t.Fatalf("got %v", err)
	}
}

package engine

// Tests for config reload: what carries over and what is dropped or kept.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// reloadSrc parses src, compiles it and reloads the engine with it.
func reloadSrc(t *testing.T, h *harness, src string) {
	t.Helper()
	cfg, err := parseConfig(src)
	if err != nil {
		t.Fatal(err)
	}
	comp, err := Compile(cfg, h.e.outputs)
	if err != nil {
		t.Fatal(err)
	}
	h.e.Reload(comp)
	h.eval()
}

func TestReloadTypeChange(t *testing.T) {
	h := newHarness(t, "[fact.x]\ncommand = \"true\"\n[[rule]]\nname = \"r\"\nwhile = \"x == 'on'\"\ninhibit = [\"idle\"]\n", "")
	h.set("x", "on")
	cfg, err := parseConfig("[fact.x]\ntype = \"bool\"\ncommand = \"true\"\n[[rule]]\nname = \"r\"\nwhile = \"x\"\ninhibit = [\"idle\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	comp, err := Compile(cfg, h.e.outputs)
	if err != nil {
		t.Fatal(err)
	}
	h.e.Reload(comp)
	f := h.e.facts["x"]
	if f.Has && f.Value != true {
		t.Fatalf("incompatible value carried over: %#v", f.Value)
	}
}

func TestReloadRunsExitOfRemovedRules(t *testing.T) {
	src := "[fact.x]\ntype = \"bool\"\ncommand = \"true\"\n[[rule]]\nname = \"a\"\nwhile = \"x\"\nexit = [{ notify = \"bye\" }]\n"
	h := newHarness(t, src, "")
	h.set("x", true)
	cfg, err := parseConfig("[fact.x]\ntype = \"bool\"\ncommand = \"true\"\n")
	if err != nil {
		t.Fatal(err)
	}
	comp, err := Compile(cfg, h.e.outputs)
	if err != nil {
		t.Fatal(err)
	}
	h.e.Reload(comp)
	if strings.Join(h.actions.ran, ",") != "a:exit:notify" {
		t.Fatalf("actions %v", h.actions.ran)
	}
}

func TestReloadKeepsUncompilableTemp(t *testing.T) {
	h := newHarness(t, "[fact.docked]\ntype = \"bool\"\ncommand = \"true\"\n", "")
	h.set("docked", true)
	if _, err := h.e.Ensure(TempSpec{While: "docked", Outputs: map[string]any{"inhibit": []any{"sleep"}}}, ""); err != nil {
		t.Fatal(err)
	}
	h.eval()
	cfg, _ := parseConfig("")
	comp, err := Compile(cfg, h.e.outputs)
	if err != nil {
		t.Fatal(err)
	}
	h.e.Reload(comp)
	h.eval()
	if h.heldLock() != "sleep" {
		t.Fatal("temporary rule should fail closed and keep its lock")
	}
}

// Extending a state fact's allowed list on reload keeps its current value.
func TestReloadKeepsStateFactWhenAllowedListExtended(t *testing.T) {
	src := `
[fact.carry]
state = ["off", "protecting"]
[fact.x]
type = "bool"
command = "true"
[[rule]]
name = "go"
while = "x"
enter = [{ set = { carry = "protecting" } }]
[[rule]]
name = "hold"
while = "carry == 'protecting'"
inhibit = ["handle-lid-switch"]
`
	h := newHarness(t, src, "")
	h.set("x", true)
	if h.e.facts["carry"].Value != "protecting" || h.heldLock() == "" {
		t.Fatalf("setup: %v %v", h.e.facts["carry"].Value, h.heldLock())
	}
	reloadSrc(t, h, `
[fact.carry]
state = ["off", "protecting", "paused"]
[fact.x]
type = "bool"
command = "true"
[[rule]]
name = "go"
while = "x"
enter = [{ set = { carry = "protecting" } }]
[[rule]]
name = "hold"
while = "carry == 'protecting'"
inhibit = ["handle-lid-switch"]
`)
	t.Logf("after reload carry=%v lock=%q", h.e.facts["carry"].Value, h.heldLock())
	if h.e.facts["carry"].Value != "protecting" {
		t.Errorf("state fact reset to %v by adding an allowed value", h.e.facts["carry"].Value)
	}
}

// Exit actions of a rule removed on reload must not leave a state fact
// holding a value the new definition forbids.
func TestReloadExitSetOutsideNewStateDefinition(t *testing.T) {
	src := `
[fact.mode]
state = ["a", "b"]
[fact.x]
type = "bool"
command = "true"
[[rule]]
name = "r"
while = "x"
exit = [{ set = { mode = "b" } }]
`
	h := newHarness(t, src, "")
	h.set("x", true)
	reloadSrc(t, h, `
[fact.mode]
state = ["a", "c"]
[fact.x]
type = "bool"
command = "true"
`)
	t.Logf("mode=%v", h.e.facts["mode"].Value)
	if h.e.facts["mode"].Value == "b" {
		t.Errorf("state fact holds %q, not an allowed value", "b")
	}
}

// Editing an expr fact's definition on reload does not reset since().
func TestReloadKeepsSinceWhenFactDefinitionEdited(t *testing.T) {
	base := `
[fact.ssid]
type = "string"
command = "true"
[fact.home]
expr = "%s"
[[rule]]
name = "away-long"
while = "!home && since('home') > duration('2h')"
enter = [{ notify = "away for 2h" }]
`
	h := newHarness(t, sprintf(base, "ssid == 'A'"), "")
	h.set("ssid", "A")
	h.advance(time.Hour)
	h.set("ssid", "elsewhere")
	h.advance(10 * time.Minute)
	if h.matched("away-long") {
		t.Fatal("setup")
	}
	reloadSrc(t, h, sprintf(base, "ssid == 'A' || ssid == 'B'"))
	h.advance(time.Second)
	t.Logf("after reload: home=%v changed=%v matched=%v actions=%v", h.e.facts["home"].Value, h.e.facts["home"].Changed, h.matched("away-long"), h.actions.ran)
	if h.matched("away-long") || len(h.actions.ran) != 0 {
		t.Fatal("editing a fact's definition must not reset since()")
	}
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

func TestStaleSourceUpdatesDropped(t *testing.T) {
	h := newHarness(t, "[fact.x]\ncommand = \"true\"\n", "")
	oldGen := h.e.gen
	h.e.startSources()
	if h.e.gen == oldGen {
		t.Fatal("generation should advance when sources restart")
	}
}

package engine

// Tests for odd values: NaN, doubles and type coercion.

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/PeterSR/whilst/plugin"
	"github.com/PeterSR/whilst/value"
)

func TestNaNIsNoValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	h := newHarness(t, "[fact.x]\ncommand = \"true\"\n[[rule]]\nname = \"r\"\nwhile = \"true\"\ninhibit = [\"idle\"]\n", path)
	h.e.onUpdate(sourceUpdate("x", nanValue()))
	h.eval()
	h.e.save(true)
	h2 := newHarness(t, "[fact.x]\ncommand = \"true\"\n[[rule]]\nname = \"r\"\nwhile = \"true\"\ninhibit = [\"idle\"]\n", path)
	if !h2.e.rules[0].Matched {
		t.Fatal("state should still be saved when a fact read NaN")
	}
}

func nanValue() any { return value.Normalize(math.NaN()) }

func sourceUpdate(f string, v any) plugin.Update { return plugin.Update{Fact: f, Value: v} }

// A non-finite value computed by a CEL expression is not kept as the fact's
// value.
func TestExprNonFiniteValueIsNoValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.x]
type = "double"
command = "true"
[fact.r]
expr = "x / 0.0"
`
	h := newHarness(t, src, path)
	h.set("x", 1.0)
	v := h.e.facts["r"].Value
	t.Logf("r=%v", v)
	if f, ok := v.(float64); ok && (math.IsInf(f, 0) || math.IsNaN(f)) {
		b, _ := os.ReadFile(path)
		var st map[string]any
		_ = json.Unmarshal(b, &st)
		t.Errorf("non-finite value from CEL stored (%v); state file contents: %d bytes", v, len(b))
	}
}

// A double-typed dbus_signal fact restored in the same boot is coerced to a double.
func TestRestoredSignalDoubleStaysDouble(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.level]
type = "double"
dbus_signal = { sender = "org.example.X", interface = "org.example.X", member = "Level" }
[[rule]]
name = "high"
while = "level * 2.0 > 3.0"
inhibit = ["sleep"]
`
	h := newHarness(t, src, path)
	h.e.onUpdate(plugin.Update{Fact: "level", Value: int64(2)})
	h.eval()
	t.Logf("before: %v (%T) matched=%v err=%q", h.e.facts["level"].Value, h.e.facts["level"].Value, h.matched("high"), h.e.rules[0].Err)
	h.e.save(true)
	h2 := newHarness(t, src, path)
	h2.e.rules[0].Matched = false // pretend it had not matched, to see whether it can evaluate
	h2.eval()
	t.Logf("after restore: %v (%T) matched=%v err=%q", h2.e.facts["level"].Value, h2.e.facts["level"].Value, h2.matched("high"), h2.e.rules[0].Err)
	if _, ok := h2.e.facts["level"].Value.(float64); !ok || h2.e.rules[0].Err != "" || !h2.matched("high") {
		t.Fatal("restored signal double should stay a double and evaluate")
	}
}

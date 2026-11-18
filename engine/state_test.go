package engine

// Tests for persistence across restarts and reboots: state file round trips
// and fact definitions. Tests that only log document accepted behaviour.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/PeterSR/whilst/plugin"
)

// Within a boot, rule match state and output state are restored; after a
// reboot both are dropped, so enter actions run again and outputs start from
// nothing. How whilst's units output relies on this is tested in package
// systemd.
func TestRebootResetsRulesAndOutputState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.home]
type = "bool"
command = "true"
[[rule]]
name    = "sync"
while   = "home"
inhibit = ["idle"]
enter   = [{ notify = "hi" }]
`
	h := newHarness(t, src, path)
	h.set("home", true)
	if h.heldLock() != "idle" || len(h.actions.ran) != 1 {
		t.Fatal("setup")
	}
	h.e.save(true)

	// Same boot: both come back, and the enter action does not run again.
	h1 := newHarness(t, src, path)
	var loaded []string
	if len(h1.locker.Loaded) != 1 || json.Unmarshal(h1.locker.Loaded[0], &loaded) != nil || strings.Join(loaded, ",") != "idle" {
		t.Fatalf("output state not restored in the same boot: %q", h1.locker.Loaded)
	}
	if !h1.matched("sync") {
		t.Fatal("rule state not restored in the same boot")
	}
	h1.set("home", true)
	if len(h1.actions.ran) != 0 {
		t.Fatalf("enter re-ran within a boot: %v", h1.actions.ran)
	}

	// After a reboot neither comes back: the output is asked for the lock
	// again and the enter action runs again.
	h2 := newHarness(t, src, "")
	h2.e.deps.StatePath, h2.e.deps.BootID = path, "boot-2"
	if err := h2.e.Restore(); err != nil {
		t.Fatal(err)
	}
	if len(h2.locker.Loaded) != 0 || h2.matched("sync") {
		t.Fatalf("state restored across a reboot: outputs %q, matched %v", h2.locker.Loaded, h2.matched("sync"))
	}
	h2.set("home", true)
	if h2.heldLock() != "idle" || strings.Join(h2.locker.Taken, ",") != "idle" {
		t.Fatalf("lock not taken after reboot: held %q, taken %v", h2.heldLock(), h2.locker.Taken)
	}
	if len(h2.actions.ran) != 1 {
		t.Fatalf("enter should run again after a reboot: %v", h2.actions.ran)
	}
}

func TestRestartKeepsDoubles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.pct]
command = "true"
[[rule]]
name    = "recent"
while   = "since('pct') < duration('1h')"
inhibit = ["idle"]
`
	h := newHarness(t, src, path)
	h.set("pct", 80.0)
	h.e.save(true)
	h2 := newHarness(t, src, path)
	h2.set("pct", 80.0)
	if !h2.e.facts["pct"].Changed.IsZero() || h2.matched("recent") {
		t.Fatal("80.0 read back from state counted as a change")
	}
}

// A double state fact holding a whole number survives a restart as a double.
func TestStateDoubleRoundTripKeepsWholeNumber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.level]
type = "double"
state = true
initial = 0.5
[fact.x]
type = "bool"
command = "true"
[[rule]]
name = "r"
while = "x"
enter = [{ set = { level = 2.0 } }]
`
	h := newHarness(t, src, path)
	h.set("x", true)
	if h.e.facts["level"].Value != 2.0 {
		t.Fatalf("setup %v", h.e.facts["level"].Value)
	}
	h.e.save(true)
	h2 := newHarness(t, src, path)
	t.Logf("restored level=%v (%T)", h2.e.facts["level"].Value, h2.e.facts["level"].Value)
	if h2.e.facts["level"].Value != 2.0 {
		t.Errorf("state double 2.0 restored as %v", h2.e.facts["level"].Value)
	}
}

// Restarting without a rule that was active runs no exit action for it.
// Logs only: documents accepted behaviour.
func TestRestartWithoutRuleSkipsItsExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := "[fact.x]\ntype = \"bool\"\ncommand = \"true\"\n[[rule]]\nname = \"a\"\nwhile = \"x\"\nexit = [{ notify = \"bye\" }]\n"
	h := newHarness(t, src, path)
	h.set("x", true)
	h.e.save(true)
	h2 := newHarness(t, "[fact.x]\ntype = \"bool\"\ncommand = \"true\"\n", path)
	h2.eval()
	t.Logf("actions after restart without rule a: %v", h2.actions.ran)
}

// A definition edited while whilst was stopped (same boot) does not make
// the first read a change.
func TestRestartWithEditedFactIsNotAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	base := `
[fact.ac]
type = "bool"
dbus = { service = "org.freedesktop.UPower", path = "/org/freedesktop/UPower", interface = "org.freedesktop.UPower", property = "OnBattery" }
%s
[[rule]]
name = "just-unplugged"
while = "!ac && since('ac') < duration('30m')"
enter = [{ notify = "lid protected" }]
`
	h := newHarness(t, sprintf(base, ""), path) // forgot the transform: ac holds OnBattery
	h.set("ac", false)                          // on mains, OnBattery=false
	h.e.save(true)
	h2 := newHarness(t, sprintf(base, `transform = "!value"`), path)
	h2.set("ac", false) // raw OnBattery=false -> ac=true
	h2.eval()
	t.Logf("ac=%v changed=%v prov=%v actions=%v", h2.e.facts["ac"].Value, h2.e.facts["ac"].Changed, h2.e.facts["ac"].Provisional, h2.actions.ran)
	if !h2.e.facts["ac"].Changed.IsZero() || len(h2.actions.ran) != 0 {
		t.Fatal("a definition edited while stopped must not make the first read a change")
	}
}

// A state file without definition keys restores signal facts as before.
func TestLegacyStateWithoutDefKeysRestoresSignalFact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.level]
type = "int"
initial = 0
dbus_signal = { sender = "org.example.X", interface = "org.example.X", member = "Level" }
[[rule]]
name = "high"
while = "level > 3"
exit = [{ notify = "bye" }]
inhibit = ["sleep"]
`
	h := newHarness(t, src, path)
	h.e.onUpdate(plugin.Update{Fact: "level", Value: int64(5)})
	h.eval()
	h.e.save(true)
	b, _ := os.ReadFile(path)
	b = regexp.MustCompile(`,\s*"def": "(?:[^"\\]|\\.)*"`).ReplaceAll(b, nil)
	os.WriteFile(path, b, 0o600)
	h2 := newHarness(t, src, path)
	h2.eval()
	t.Logf("after upgrade restart: level=%v matched=%v actions=%v", h2.e.facts["level"].Value, h2.matched("high"), h2.actions.ran)
	if !h2.matched("high") || len(h2.actions.ran) != 0 {
		t.Fatal("a state file without definition keys should restore signal facts as before")
	}
}

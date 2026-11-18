package engine

// Tests for guessed (provisional) fact values, expr facts computed from them
// and debounce around boot.

import (
	"path/filepath"
	"testing"
	"time"
)

func TestExprFactDebounce(t *testing.T) {
	src := `
[fact.ssid]
type = "string"
command = "true"
[fact.home]
expr = "ssid == 'home'"
debounce = "1m"
[[rule]]
name    = "home"
while   = "home"
inhibit = ["idle"]
`
	h := newHarness(t, src, "")
	h.set("ssid", "home")
	if h.heldLock() != "idle" {
		t.Fatal("setup")
	}
	h.advance(time.Hour)
	h.set("ssid", "")
	if h.heldLock() != "idle" {
		t.Fatal("expr fact debounce ignored: lock dropped at once")
	}
	h.advance(10 * time.Second)
	h.set("ssid", "home")
	h.advance(2 * time.Minute)
	if h.heldLock() != "idle" {
		t.Fatal("a recovered blip should not drop the lock")
	}
	h.set("ssid", "")
	h.now = h.e.nextWake
	h.eval()
	if h.heldLock() != "" {
		t.Fatal("a lasting change should apply after the debounce")
	}
}

func TestNoPhantomChangeFromGuesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.ssid]
type = "string"
command = "true"
[fact.home]
expr = "ssid == 'HomeWifi'"
[[rule]]
name  = "left-home"
while = "!home && since('home') < duration('10m')"
enter = [{ notify = "left" }]
`
	h := newHarness(t, src, path)
	h.set("ssid", "HomeWifi")
	h.e.save(true)

	// Reboot: ssid is remembered as a guess, and home is computed from it.
	h2 := newHarness(t, src, "")
	h2.e.deps.StatePath, h2.e.deps.BootID = path, "boot-2"
	if err := h2.e.Restore(); err != nil {
		t.Fatal(err)
	}
	h2.eval()
	if !h2.e.facts["home"].Provisional {
		t.Fatal("an expr fact computed from a guess should be provisional")
	}
	if h2.e.rules[0].Err == "" {
		t.Fatal("a rule reading a guess should wait")
	}
	h2.set("ssid", "Elsewhere")
	if !h2.e.facts["home"].Changed.IsZero() || len(h2.actions.ran) != 0 {
		t.Fatalf("phantom change: changed=%v actions=%v", h2.e.facts["home"].Changed, h2.actions.ran)
	}
	// A real change after that is a change.
	h2.advance(time.Minute)
	h2.set("ssid", "HomeWifi")
	h2.advance(time.Minute)
	h2.set("ssid", "Elsewhere")
	if len(h2.actions.ran) != 1 {
		t.Fatalf("real change should fire: %v", h2.actions.ran)
	}
}

// A rule on a dbus_signal fact with an initial value matches before the
// first signal arrives.
func TestSignalFactWithInitialMatchesBeforeFirstSignal(t *testing.T) {
	src := `
[fact.sleeping]
type = "bool"
initial = false
dbus_signal = { sender = "org.freedesktop.login1", interface = "org.freedesktop.login1.Manager", member = "PrepareForSleep" }
[[rule]]
name = "awake"
while = "!sleeping"
inhibit = ["idle"]
`
	h := newHarness(t, src, "")
	h.eval()
	h.advance(time.Hour)
	t.Logf("matched=%v err=%q", h.matched("awake"), h.e.rules[0].Err)
	if !h.matched("awake") {
		t.Errorf("rule on a dbus_signal fact with initial never matches until the first signal: %s", h.e.rules[0].Err)
	}
}

// A real change of one input while another input is still a guess. Logs
// only: documents that since() may not record that change.
func TestMixedProvisionalInputsChange(t *testing.T) {
	src := `
[fact.ac]
type = "bool"
command = "true"
[fact.ssid]
type = "string"
initial = "HomeWifi"
command = "true"
[fact.home_ac]
expr = "ac && ssid == 'HomeWifi'"
`
	h := newHarness(t, src, "")
	h.set("ac", true)
	h.advance(time.Minute)
	h.set("ac", false) // real change, ssid still a guess
	h.advance(time.Minute)
	h.set("ssid", "HomeWifi") // ssid confirmed
	t.Logf("home_ac=%v changed=%v prov=%v", h.e.facts["home_ac"].Value, h.e.facts["home_ac"].Changed, h.e.facts["home_ac"].Provisional)
	if h.e.facts["home_ac"].Changed.IsZero() {
		t.Logf("NOTE: since('home_ac') lost the real change caused by ac")
	}
}

// A short blip right after boot respects the debounce of an expr fact.
func TestBootBlipRespectsDebounce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.ssid]
type = "string"
command = "true"
[fact.home]
expr = "ssid == 'HomeWifi'"
debounce = "10s"
[[rule]]
name = "away"
while = "!home"
enter = [{ notify = "left home" }]
`
	h := newHarness(t, src, path)
	h.set("ssid", "HomeWifi")
	h.e.save(true)
	h2 := newHarness(t, src, "")
	h2.e.deps.StatePath, h2.e.deps.BootID = path, "boot-2"
	if err := h2.e.Restore(); err != nil {
		t.Fatal(err)
	}
	h2.eval()
	h2.set("ssid", "") // Wi-Fi not up yet at boot
	h2.advance(3 * time.Second)
	h2.set("ssid", "HomeWifi")
	h2.advance(20 * time.Second)
	t.Logf("actions=%v home.Changed=%v", h2.actions.ran, h2.e.facts["home"].Changed)
	if len(h2.actions.ran) > 0 {
		t.Errorf("a 3s blip at boot fired %v despite debounce 10s on home", h2.actions.ran)
	}
}

// A noisy debounced fact that starts provisional still gets confirmed.
func TestNoisyDebouncedFactGetsConfirmed(t *testing.T) {
	src := `
[fact.load]
type = "double"
command = "true"
initial = 0.0
debounce = "30s"
[[rule]]
name = "busy"
while = "load > 1.0"
inhibit = ["sleep"]
`
	h := newHarness(t, src, "")
	for i := 0; i < 60; i++ {
		h.set("load", 2.0+float64(i)/100)
		h.advance(10 * time.Second)
	}
	f := h.e.facts["load"]
	t.Logf("after 10 min: value=%v prov=%v pending=%v rule matched=%v err=%q", f.Value, f.Provisional, f.pending != nil, h.matched("busy"), h.e.rules[0].Err)
	if f.Provisional || h.e.rules[0].Err != "" {
		t.Fatal("a noisy debounced fact should get confirmed")
	}
}

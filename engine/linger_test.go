package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const lingerConfig = `
[fact.ac]
type = "bool"
command = "true"

[[rule]]
name    = "on-mains"
while   = "ac"
inhibit = ["sleep"]

[[rule]]
name    = "just-unplugged"
while   = "!ac && since('ac') < duration('30m')"
linger  = "1m"
inhibit = ["handle-lid-switch"]
enter   = [{ notify = "unplugged" }]
exit    = [{ notify = "protection over" }]
`

func hasLock(h *harness, what string) bool {
	return strings.Contains(h.heldLock(), what)
}

// Plugging back in keeps the lid lock for the linger time, alongside the
// lock of the rule that now matches, and only then runs the exit actions.
func TestLingerKeepsOutputsAfterConditionEnds(t *testing.T) {
	h := newHarness(t, lingerConfig, "")
	h.set("ac", true)
	h.advance(time.Hour)
	h.set("ac", false)
	if !hasLock(h, "handle-lid-switch") {
		t.Fatalf("unplugged: lock %q", h.heldLock())
	}
	h.actions.ran = nil

	h.advance(5 * time.Minute)
	h.set("ac", true)
	if !h.matched("just-unplugged") {
		t.Fatal("rule stopped matching at once despite linger")
	}
	if !hasLock(h, "handle-lid-switch") || !hasLock(h, "sleep") {
		t.Fatalf("while lingering: lock %q, want sleep and handle-lid-switch", h.heldLock())
	}
	if len(h.actions.ran) != 0 {
		t.Fatalf("exit actions ran before the linger ended: %v", h.actions.ran)
	}
	want := h.now.Add(time.Minute)
	if !h.e.nextWake.Equal(want) {
		t.Fatalf("next wake %v, want the end of the linger %v", h.e.nextWake, want)
	}

	var st *RuleStatus
	snap := h.e.Snapshot()
	for i := range snap.Rules {
		if snap.Rules[i].Name == "just-unplugged" {
			st = &snap.Rules[i]
		}
	}
	if st == nil || !st.Matched || !st.LingerUntil.Equal(want) {
		t.Fatalf("status while lingering: %+v", st)
	}

	h.advance(59 * time.Second)
	if !hasLock(h, "handle-lid-switch") {
		t.Fatal("linger ended early")
	}
	h.now = h.e.nextWake
	h.eval()
	if h.matched("just-unplugged") {
		t.Fatal("still matching after the linger")
	}
	if got := h.heldLock(); got != "sleep" {
		t.Fatalf("after the linger: lock %q, want sleep", got)
	}
	if len(h.actions.ran) != 1 || h.actions.ran[0] != "just-unplugged:exit:notify" {
		t.Fatalf("actions %v", h.actions.ran)
	}
}

// A condition that turns true again while lingering continues the rule
// without exit or enter actions.
func TestLingerConditionTrueAgainContinues(t *testing.T) {
	h := newHarness(t, lingerConfig, "")
	h.set("ac", true)
	h.advance(time.Hour)
	h.set("ac", false)
	h.actions.ran = nil
	h.advance(time.Minute)
	h.set("ac", true)
	h.advance(20 * time.Second)
	h.set("ac", false)
	if !h.matched("just-unplugged") {
		t.Fatal("rule ended")
	}
	if len(h.actions.ran) != 0 {
		t.Fatalf("actions ran for a blip inside the linger: %v", h.actions.ran)
	}
	for _, r := range h.e.rules {
		if r.Def.Name == "just-unplugged" && !r.FalseSince.IsZero() {
			t.Fatal("still lingering although the condition is true again")
		}
	}
}

// The linger also applies when the condition ends by time, not by a change.
func TestLingerAfterTimeThreshold(t *testing.T) {
	h := newHarness(t, lingerConfig, "")
	h.set("ac", true)
	h.advance(time.Hour)
	h.set("ac", false)
	h.now = h.e.nextWake // the 30 minute threshold
	h.eval()
	if !h.matched("just-unplugged") || !hasLock(h, "handle-lid-switch") {
		t.Fatal("rule ended at the threshold despite linger")
	}
	h.now = h.e.nextWake
	h.eval()
	if h.matched("just-unplugged") || h.heldLock() != "" {
		t.Fatalf("after threshold and linger: matched=%v lock=%q", h.matched("just-unplugged"), h.heldLock())
	}
}

// A restart in the middle of a linger ends it on time, not later.
func TestLingerSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	h := newHarness(t, lingerConfig, path)
	h.set("ac", true)
	h.advance(time.Hour)
	h.set("ac", false)
	h.advance(5 * time.Minute)
	h.set("ac", true)
	end := h.now.Add(time.Minute)
	h.e.save(true)

	h2 := newHarness(t, lingerConfig, path)
	h2.now = h.now.Add(10 * time.Second)
	h2.set("ac", true)
	if !h2.matched("just-unplugged") {
		t.Fatal("lingering rule ended on restart")
	}
	if !h2.e.nextWake.Equal(end) {
		t.Fatalf("next wake %v, want the original end %v", h2.e.nextWake, end)
	}
}

// A reload that removes linger from a lingering rule ends it.
func TestReloadWithoutLingerEndsIt(t *testing.T) {
	h := newHarness(t, lingerConfig, "")
	h.set("ac", true)
	h.advance(time.Hour)
	h.set("ac", false)
	h.advance(time.Minute)
	h.set("ac", true)
	h.actions.ran = nil
	reloadSrc(t, h, strings.Replace(lingerConfig, `linger  = "1m"`, "", 1))
	if h.matched("just-unplugged") {
		t.Fatal("rule still matches after linger was removed")
	}
	if len(h.actions.ran) != 1 || h.actions.ran[0] != "just-unplugged:exit:notify" {
		t.Fatalf("actions %v, want the exit action once", h.actions.ran)
	}
}

func TestLingerConfigValidation(t *testing.T) {
	for _, bad := range []string{`"0s"`, `"-1m"`, `"5"`, `5`, `true`} {
		src := "[[rule]]\nwhile = \"true\"\nlinger = " + bad + "\ninhibit = [\"sleep\"]\n"
		if _, err := parseConfig(src); err == nil {
			t.Errorf("linger = %s accepted", bad)
		}
	}
}

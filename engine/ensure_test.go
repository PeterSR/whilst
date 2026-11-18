package engine

// Tests for EnsureNow and temporary rules.

import (
	"errors"
	"strings"
	"testing"
)

func TestEnsureNowReportsProblems(t *testing.T) {
	h := newHarness(t, sleepConfig, "")
	h.set("ssid", "x")
	h.set("ac", false)
	h.locker.Fail = true
	_, problems, _, err := h.e.EnsureNow(TempSpec{Outputs: map[string]any{"inhibit": []any{"sleep"}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "sleep is not held") {
		t.Fatalf("problems %v", problems)
	}
	h.locker.Fail = false
	_, problems, _, err = h.e.EnsureNow(TempSpec{Name: "b", Outputs: map[string]any{"inhibit": []any{"idle"}}}, "")
	if err != nil || len(problems) != 0 || h.heldLock() != "idle:sleep" {
		t.Fatalf("problems %v err %v lock %q", problems, err, h.heldLock())
	}
}

// Attaching needs a caller the engine can track; the application maps the
// sentinel to its own message.
func TestAttachWithoutSender(t *testing.T) {
	h := newHarness(t, "", "")
	_, err := h.e.Ensure(TempSpec{Attach: true, Outputs: map[string]any{"inhibit": []any{"sleep"}}}, "")
	if !errors.Is(err, ErrNoLifetimes) {
		t.Fatalf("got %v, want ErrNoLifetimes", err)
	}
}

func TestGoneTempFreesName(t *testing.T) {
	h := newHarness(t, "", "")
	spec := TempSpec{Name: "build", Outputs: map[string]any{"inhibit": []any{"sleep"}}}
	if _, err := h.e.Ensure(spec, ""); err != nil {
		t.Fatal(err)
	}
	h.e.Release("build")
	if _, err := h.e.Ensure(spec, ""); err != nil {
		t.Fatalf("name of a released rule should be free at once: %v", err)
	}
}

func TestEnsureNowPermanentConditionErrorIsAProblem(t *testing.T) {
	h := newHarness(t, "[fact.x]\ncommand = \"true\"\n", "")
	h.set("x", "text")
	_, problems, waiting, err := h.e.EnsureNow(TempSpec{While: "x > 5", Outputs: map[string]any{"inhibit": []any{"sleep"}}}, "")
	if err != nil || waiting != "" || len(problems) == 0 {
		t.Fatalf("problems=%v waiting=%q err=%v", problems, waiting, err)
	}
	h2 := newHarness(t, "[fact.y]\ntype = \"bool\"\ncommand = \"true\"\n", "")
	_, problems, waiting, err = h2.e.EnsureNow(TempSpec{While: "y", Outputs: map[string]any{"inhibit": []any{"sleep"}}}, "")
	if err != nil || waiting == "" || len(problems) != 0 {
		t.Fatalf("unread fact: problems=%v waiting=%q err=%v", problems, waiting, err)
	}
}

// ensure --while on a not-yet-read fact reports no problems, so attached
// ensure can still run its command.
func TestEnsureWhileOnUnreadFactReportsNoProblem(t *testing.T) {
	src := `
[fact.ac]
type = "bool"
initial = true
command = "true"
`
	h := newHarness(t, src, "")
	h.e.settled = true
	_, problems, _, err := h.e.EnsureNow(TempSpec{While: "ac", Outputs: map[string]any{"inhibit": []any{"sleep"}}}, "")
	t.Logf("problems=%v err=%v", problems, err)
	if len(problems) > 0 {
		t.Errorf("ensure --while on a not-yet-read fact reports problems %v; attached ensure refuses to run", problems)
	}
}

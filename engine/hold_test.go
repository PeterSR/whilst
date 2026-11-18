package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/internal/fake"
	"github.com/PeterSR/whilst/plugin"
)

const holdConfig = `
[fact.battery]
type = "int"
command = "true"

[[rule]]
name    = "save-power"
while   = "battery < 20"
hold    = "battery < 30"
inhibit = ["idle"]
enter   = [{ notify = "saving power" }]
exit    = [{ notify = "done saving" }]
`

func ruleByName(h *harness, name string) *rule {
	for _, r := range h.e.allRules() {
		if r.Def.Name == name {
			return r
		}
	}
	h.t.Fatalf("no rule %s", name)
	return nil
}

// hold only keeps a rule on; it never starts one.
func TestHoldDoesNotStartRule(t *testing.T) {
	h := newHarness(t, holdConfig, "")
	h.set("battery", int64(25))
	if h.matched("save-power") {
		t.Fatal("hold started the rule")
	}
}

// Hysteresis: start below 20, keep going until 30, then stop.
func TestHoldKeepsRuleUntilFalse(t *testing.T) {
	h := newHarness(t, holdConfig, "")
	h.set("battery", int64(50))
	h.set("battery", int64(15))
	if !h.matched("save-power") || ruleByName(h, "save-power").Holding {
		t.Fatal("rule should match through while")
	}
	h.actions.ran = nil
	h.set("battery", int64(25))
	r := ruleByName(h, "save-power")
	if !r.Matched || !r.Holding {
		t.Fatalf("at 25: matched=%v holding=%v, want both", r.Matched, r.Holding)
	}
	if h.heldLock() != "idle" {
		t.Fatalf("lock %q while holding", h.heldLock())
	}
	h.set("battery", int64(19))
	if r.Holding {
		t.Fatal("holding although while is true again")
	}
	h.set("battery", int64(30))
	if h.matched("save-power") || h.heldLock() != "" {
		t.Fatal("rule still on after hold turned false")
	}
	if len(h.actions.ran) != 1 || h.actions.ran[0] != "save-power:exit:notify" {
		t.Fatalf("actions %v, want only the exit", h.actions.ran)
	}
}

// hold and linger combine: the linger starts when hold turns false.
func TestHoldThenLinger(t *testing.T) {
	src := strings.Replace(holdConfig, `hold    = "battery < 30"`, "hold    = \"battery < 30\"\nlinger  = \"1m\"", 1)
	h := newHarness(t, src, "")
	h.set("battery", int64(15))
	h.set("battery", int64(35))
	r := ruleByName(h, "save-power")
	if !r.Matched || r.Holding || r.lingerUntil().IsZero() {
		t.Fatalf("matched=%v holding=%v lingerUntil=%v", r.Matched, r.Holding, r.lingerUntil())
	}
	h.advance(time.Minute)
	if h.matched("save-power") {
		t.Fatal("still matching after the linger")
	}
}

// A since() threshold in hold is scheduled like one in while.
func TestHoldThresholdScheduled(t *testing.T) {
	src := `
[fact.ac]
type = "bool"
command = "true"

[[rule]]
name    = "r"
while   = "!ac"
hold    = "since('ac') < duration('10m')"
inhibit = ["sleep"]
`
	h := newHarness(t, src, "")
	h.set("ac", true)
	h.advance(time.Hour)
	h.set("ac", false)
	h.advance(time.Minute)
	h.set("ac", true)
	if !h.matched("r") {
		t.Fatal("hold should keep the rule for 10 minutes after ac changed")
	}
	want := h.now.Add(10*time.Minute + time.Millisecond)
	if !h.e.nextWake.Equal(want) {
		t.Fatalf("next wake %v, want %v", h.e.nextWake, want)
	}
	h.now = h.e.nextWake
	h.eval()
	if h.matched("r") {
		t.Fatal("rule still on after the hold threshold")
	}
}

func TestHoldConfigValidation(t *testing.T) {
	for _, bad := range []string{`""`, `5`, `"battery +"`, `"battery"`} {
		src := "[fact.battery]\ntype = \"int\"\ncommand = \"true\"\n[[rule]]\nwhile = \"true\"\nhold = " + bad + "\ninhibit = [\"sleep\"]\n"
		cfg, err := parseConfig(src)
		if err == nil {
			_, err = Compile(cfg, []plugin.Output{&fake.Locks{}})
		}
		if err == nil {
			t.Errorf("hold = %s accepted", bad)
		}
	}
}

// Adding hold to an unnamed rule gives it a new name, but rules without hold
// keep the names they had before hold existed.
func TestHoldNamingStable(t *testing.T) {
	src := "[[rule]]\nwhile = \"true\"\ninhibit = [\"sleep\"]\n"
	a, err := parseConfig(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseConfig(strings.Replace(src, "inhibit", "hold = \"true\"\ninhibit", 1))
	if err != nil {
		t.Fatal(err)
	}
	if a.Rules[0].Name == b.Rules[0].Name {
		t.Fatal("hold does not change the generated name")
	}
	if a.Rules[0].Name != "rule-"+contentHashWithoutHold(a.Rules[0]) {
		t.Fatalf("name %s changed for a rule without hold", a.Rules[0].Name)
	}
}

// contentHashWithoutHold is how unnamed rules were named before hold.
func contentHashWithoutHold(r *config.Rule) string {
	b, _ := json.Marshal(struct {
		While   string
		Outputs map[string]any
		Enter   []config.Action
		Exit    []config.Action
	}{r.While, r.Outputs, r.Enter, r.Exit})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

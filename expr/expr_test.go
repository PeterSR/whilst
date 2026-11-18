package expr

import (
	"testing"
	"time"
)

func env(t *testing.T) *Env {
	t.Helper()
	e, err := NewEnv(map[string]string{"ac": "bool", "ssid": "string", "battery": "double", "info": "dyn"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestThresholds(t *testing.T) {
	e := env(t)
	p, err := e.Compile("!ac && since('ac') < duration('30m')", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ticks {
		t.Error("recognised since() comparison should not need ticks")
	}
	if len(p.Thresholds) != 1 || p.Thresholds[0] != (Threshold{"ac", 30 * time.Minute}) {
		t.Errorf("thresholds = %v", p.Thresholds)
	}
	if len(p.Facts) != 1 || p.Facts[0] != "ac" {
		t.Errorf("facts = %v", p.Facts)
	}

	p, err = e.Compile("duration('1h') >= since('ssid')", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ticks || len(p.Thresholds) != 1 || p.Thresholds[0].After != time.Hour {
		t.Errorf("reversed comparison: ticks=%v thresholds=%v", p.Ticks, p.Thresholds)
	}
}

func TestTicks(t *testing.T) {
	e := env(t)
	for _, src := range []string{
		"hour >= 21",
		"since('ac') + duration('1m') < duration('30m')",
		"now > timestamp('2026-01-01T00:00:00Z')",
	} {
		p, err := e.Compile(src, true)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if !p.Ticks {
			t.Errorf("%s: expected ticks", src)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	e := env(t)
	for _, src := range []string{
		"ac &&",      // syntax
		"nosuchfact", // unknown ident
		"since('nosuch') < duration('1m')",
		"since(ssid) < duration('1m')", // not a literal
		"ssid",                         // not a bool
		"battery + 1",                  // not a bool
	} {
		if _, err := e.Compile(src, true); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}

func TestEval(t *testing.T) {
	e := env(t)
	now := time.Date(2026, 11, 18, 22, 30, 0, 0, time.Local)
	ctx := &Context{Now: now, Since: func(name string) time.Duration {
		if name == "ac" {
			return 10 * time.Minute
		}
		return Never
	}}
	p, err := e.Compile("!ac && since('ac') < duration('30m') && hour == 22 && weekday == 3", true)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := p.EvalBool(map[string]any{"ac": false}, ctx)
	if err != nil || !ok {
		t.Errorf("got %v, %v", ok, err)
	}
	if _, err := p.EvalBool(map[string]any{}, ctx); err == nil {
		t.Error("missing fact value should be an error")
	}

	p, err = e.Compile("info.levels.map(x, x * 2)", false)
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Eval(map[string]any{"info": map[string]any{"levels": []any{int64(1), int64(2)}}}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := v.([]any)
	if !ok || len(l) != 2 || l[1] != int64(4) {
		t.Errorf("got %#v", v)
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"ac", "home_wifi", "_x1"} {
		if err := ValidName(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	for _, n := range []string{"1x", "a-b", "now", "in", "value", ""} {
		if err := ValidName(n); err == nil {
			t.Errorf("%s: expected an error", n)
		}
	}
}

func TestClockVarsReserved(t *testing.T) {
	for _, n := range ClockVars() {
		if !IsReserved(n) {
			t.Errorf("clock variable %s is not reserved", n)
		}
		if ValidName(n) == nil {
			t.Errorf("clock variable %s is accepted as a fact name", n)
		}
	}
	if IsReserved("battery") {
		t.Error("battery is reserved")
	}
}

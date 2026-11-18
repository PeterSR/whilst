package value

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		{uint32(4), int64(4)},
		{[]string{"a", "b"}, []any{"a", "b"}},
		{map[string]int32{"k": 1}, map[string]any{"k": int64(1)}},
		{uint64(1 << 63), uint64(1 << 63)},
	}
	for _, c := range cases {
		if got := Normalize(c.in); !Equal(got, c.want) {
			t.Errorf("Normalize(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// A conversion hook is offered every level of the value.
func TestNormalizeWith(t *testing.T) {
	type wrapped struct{ v any }
	unwrap := func(v any) (any, bool) {
		if w, ok := v.(wrapped); ok {
			return w.v, true
		}
		return nil, false
	}
	in := map[string]wrapped{"k": {[]wrapped{{int32(1)}, {wrapped{"x"}}}}}
	want := map[string]any{"k": []any{int64(1), "x"}}
	if got := NormalizeWith(in, unwrap); !Equal(got, want) {
		t.Errorf("NormalizeWith = %#v, want %#v", got, want)
	}
}

func TestCoerce(t *testing.T) {
	ok := []struct {
		in   any
		kind string
		want any
	}{
		{"1", "bool", true},
		{"0", "bool", false},
		{"42", "int", int64(42)},
		{"96.5", "double", 96.5},
		{int64(3), "double", 3.0},
		{true, "string", "true"},
		{int64(5), "dyn", int64(5)},
	}
	for _, c := range ok {
		got, err := Coerce(c.in, c.kind)
		if err != nil || !Equal(got, c.want) {
			t.Errorf("Coerce(%#v, %s) = %#v, %v", c.in, c.kind, got, err)
		}
	}
	if _, err := Coerce("maybe", "bool"); err == nil {
		t.Error("expected an error")
	}
	if _, err := Coerce("x", "list"); err == nil {
		t.Error("expected an error")
	}
}

func TestFromJSON(t *testing.T) {
	if got := FromJSON(float64(3)); got != int64(3) {
		t.Errorf("%#v", got)
	}
	if got := FromJSON(2.5); got != 2.5 {
		t.Errorf("%#v", got)
	}
}

func TestEqualNumbers(t *testing.T) {
	if !Equal(int64(80), 80.0) || !Equal([]any{80.0}, []any{int64(80)}) || !Equal(map[string]any{"a": 1.0}, map[string]any{"a": int64(1)}) {
		t.Error("whole doubles should equal ints")
	}
	if Equal(int64(80), 80.5) || Equal(int64(1), "1") || Equal(int64(1), true) {
		t.Error("unequal values compared equal")
	}
	if Equal(int64(1<<62), int64(1<<62+1)) {
		t.Error("large ints must compare exactly")
	}
}

// Named scalar types take the normal form of their underlying kind instead
// of becoming strings.
func TestNormalizeNamedScalars(t *testing.T) {
	type level int
	type small uint8
	type huge uint64
	type ratio float32
	type flag bool
	type state string
	cases := []struct {
		in   any
		want any
	}{
		{level(3), int64(3)},
		{small(7), int64(7)},
		{huge(1 << 63), uint64(1 << 63)},
		{ratio(0.5), float64(0.5)},
		{flag(true), true},
		{state("on"), "on"},
		{[]level{1, 2}, []any{int64(1), int64(2)}},
		{map[state]level{"a": 1}, map[string]any{"a": int64(1)}},
		{time.Minute, "1m0s"},
	}
	for _, c := range cases {
		got := Normalize(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Normalize(%#v) = %#v (%T), want %#v (%T)", c.in, got, got, c.want, c.want)
		}
	}
	if got := Normalize(ratio(float32(math.Inf(1)))); got != nil {
		t.Errorf("Normalize(+Inf ratio) = %#v, want nil", got)
	}
}

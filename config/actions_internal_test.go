package config

import (
	"encoding/json"
	"math"
	"testing"
)

// An action of a kind the registry does not know, built with NewAction, is
// still hashed, as its own field.
func TestHashUnregisteredAction(t *testing.T) {
	for _, reg := range []*Registry{nil, NewRegistry()} {
		got, err := json.Marshal(hashActions([]Action{NewAction("beep", "hi")}, reg))
		if err != nil {
			t.Fatal(err)
		}
		if want := `[{"kind:beep":"hi","Set":null}]`; string(got) != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

// A spec that JSON cannot encode still tells rules apart: it is hashed as
// its Go syntax rather than all such specs hashing alike.
func TestHashUnencodableSpec(t *testing.T) {
	a := hashActions([]Action{NewAction("beep", map[string]any{"x": math.NaN()})}, nil)
	b := hashActions([]Action{NewAction("beep", map[string]any{"x": math.Inf(1)})}, nil)
	for _, h := range [][]json.RawMessage{a, b} {
		if !json.Valid(h[0]) {
			t.Fatalf("invalid JSON %s", h[0])
		}
	}
	if string(a[0]) == string(b[0]) {
		t.Fatalf("different specs hash alike: %s", a[0])
	}
	r1 := &Rule{While: "true", Enter: []Action{NewAction("beep", math.NaN())}}
	r2 := &Rule{While: "true", Enter: []Action{NewAction("beep", math.Inf(1))}}
	if contentHash(r1, nil) == contentHash(r2, nil) {
		t.Fatal("rules with different unencodable specs get the same name")
	}
}

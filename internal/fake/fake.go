// Package fake provides stand-ins for what an application registers with the
// core: fact kinds, action kinds, presets and an output. The core's tests
// use them so they need nothing from an application.
//
// The fact kinds borrow whilst's names so test configs read like real ones:
// command and dbus stand for sources that read on their own (rereadable),
// dbus_signal for one that may never report. None of them reads anything;
// tests feed values to the engine directly.
package fake

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing/fstest"

	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/plugin"
)

// Spec is the parsed source of a fake fact kind.
type Spec struct {
	Kind  string
	Value any
}

func (s *Spec) SourceKey() string {
	b, _ := json.Marshal([]any{s.Kind, s.Value})
	return string(b)
}

// Presets has one preset, upower, declaring the facts ac and battery.
var Presets = fstest.MapFS{
	"upower.toml": {Data: []byte("[fact.ac]\ntype = \"bool\"\ncommand = \"true\"\n[fact.battery]\ntype = \"int\"\ncommand = \"true\"\n")},
}

// Registry returns a registry with the fake kinds and presets.
func Registry() *config.Registry {
	reg := config.NewRegistry()
	reg.Presets = Presets
	str := func(kind string) func(any, map[string]any) (config.FactSpec, error) {
		return func(v any, _ map[string]any) (config.FactSpec, error) {
			s, ok := v.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("%s must be a non-empty string", kind)
			}
			return &Spec{Kind: kind, Value: s}, nil
		}
	}
	table := func(kind string) func(any, map[string]any) (config.FactSpec, error) {
		return func(v any, _ map[string]any) (config.FactSpec, error) {
			t, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s must be a table", kind)
			}
			return &Spec{Kind: kind, Value: t}, nil
		}
	}
	reg.AddFactKind(config.FactKind{Name: "dbus", Rereadable: true, Parse: table("dbus")})
	reg.AddFactKind(config.FactKind{Name: "dbus_signal", Parse: table("dbus_signal")})
	reg.AddFactKind(config.FactKind{Name: "command", Keys: []string{"every"}, Rereadable: true, Parse: str("command")})

	reg.AddActionKind(config.ActionKind{Name: "notify", Parse: func(v any, _ map[string]any) (any, error) {
		if s, ok := v.(string); !ok || s == "" {
			return nil, fmt.Errorf("notify needs a summary")
		}
		return v, nil
	}})
	reg.AddActionKind(config.ActionKind{Name: "command", Keys: []string{"timeout"}, Parse: func(v any, t map[string]any) (any, error) {
		if s, ok := v.(string); !ok || s == "" {
			return nil, fmt.Errorf("command must be a non-empty string")
		}
		if d, ok := t["timeout"]; ok {
			if _, err := config.Duration(d); err != nil {
				return nil, fmt.Errorf("timeout must be a positive duration")
			}
		}
		return v, nil
	}})
	return reg
}

// LockTypes are the values the Locks output accepts, in the order its
// status lists them.
var LockTypes = []string{"shutdown", "sleep", "idle", "handle-lid-switch"}

// Locks is an output named "inhibit" that holds one combined lock for the
// union of every matching rule's lock types, the way whilst's inhibitor
// output does. A failed change keeps the previous lock.
type Locks struct {
	// Fail makes taking a new lock fail.
	Fail bool
	// Taken lists every lock taken, as its sorted types joined with ":".
	Taken []string
	// Loaded records state passed to Load.
	Loaded []json.RawMessage

	held   []string
	wanted map[string][]string
	err    error
}

func (*Locks) Name() string { return "inhibit" }

func (*Locks) Parse(raw any) (any, error) {
	list, err := config.StringList(raw)
	if err != nil {
		return nil, fmt.Errorf("inhibit: %w", err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("inhibit: list at least one lock type")
	}
	for _, w := range list {
		if !contains(LockTypes, w) {
			return nil, fmt.Errorf("inhibit: unknown lock type %q", w)
		}
	}
	return list, nil
}

func (l *Locks) Reconcile(wants []plugin.Want) error {
	l.wanted = map[string][]string{}
	for _, w := range wants {
		for _, s := range w.Claim.([]string) {
			if !contains(l.wanted[s], w.Rule) {
				l.wanted[s] = append(l.wanted[s], w.Rule)
			}
		}
	}
	var whats []string
	for w, rules := range l.wanted {
		sort.Strings(rules)
		whats = append(whats, w)
	}
	sort.Strings(whats)
	if strings.Join(whats, ":") == strings.Join(l.held, ":") {
		l.err = nil
		return nil
	}
	if len(whats) > 0 && l.Fail {
		l.err = fmt.Errorf("take %s lock: failed", strings.Join(whats, ":"))
		return l.err
	}
	if len(whats) > 0 {
		l.Taken = append(l.Taken, strings.Join(whats, ":"))
	}
	l.held, l.err = whats, nil
	return nil
}

// Held returns the lock held now, as its types joined with ":", or "".
func (l *Locks) Held() string { return strings.Join(l.held, ":") }

func (l *Locks) Status() []plugin.Item {
	var items []plugin.Item
	for _, w := range LockTypes {
		if !contains(l.held, w) && l.wanted[w] == nil {
			continue
		}
		it := plugin.Item{Key: w, Held: contains(l.held, w), WantedBy: l.wanted[w]}
		if l.err != nil {
			it.Err = l.err.Error()
		}
		items = append(items, it)
	}
	return items
}

// Save persists the held lock types, so tests can see when the engine
// restores output state.
func (l *Locks) Save() json.RawMessage {
	if len(l.held) == 0 {
		return nil
	}
	b, _ := json.Marshal(l.held)
	return b
}

func (l *Locks) Load(raw json.RawMessage) error {
	l.Loaded = append(l.Loaded, raw)
	return nil
}

func (l *Locks) Close() { l.held = nil }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

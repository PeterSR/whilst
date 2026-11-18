package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/PeterSR/whilst/value"
)

// Action is one entry in a rule's enter or exit list: either an action of a
// registered kind, or a set action.
type Action struct {
	// Spec is the parsed action of a registered kind, as returned by its
	// ActionKind.Parse; nil for a set action.
	Spec any
	// Set writes state facts.
	Set map[string]any

	kind string
}

// NewAction returns an action of a registered kind, as the config loader
// builds it.
func NewAction(kind string, spec any) Action { return Action{Spec: spec, kind: kind} }

// Kind names the action for logs.
func (a Action) Kind() string {
	switch {
	case a.Set != nil:
		return "set"
	case a.kind != "":
		return a.kind
	}
	return "?"
}

func parseActions(v any, reg *Registry) ([]Action, error) {
	var list []map[string]any
	switch x := v.(type) {
	case []map[string]any:
		list = x
	case map[string]any:
		list = []map[string]any{x}
	case []any:
		for _, e := range x {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("each action must be a table, like %s", reg.actionExample())
			}
			list = append(list, m)
		}
	default:
		return nil, fmt.Errorf("must be a list of action tables")
	}
	out := make([]Action, 0, len(list))
	for i, m := range list {
		a, err := parseAction(m, reg)
		if err != nil {
			return nil, fmt.Errorf("action %d: %w", i+1, err)
		}
		out = append(out, a)
	}
	return out, nil
}

func parseAction(m map[string]any, reg *Registry) (Action, error) {
	var a Action
	all := reg.ActionKindNames()
	var kinds []string
	for _, k := range all {
		if _, ok := m[k]; ok {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) != 1 {
		return a, fmt.Errorf("needs exactly one of %s", orList(all))
	}
	kind := kinds[0]
	ak := reg.actionKind(kind)
	for k := range m {
		if k != kind && (ak == nil || !contains(ak.Keys, k)) {
			return a, fmt.Errorf("unknown key %q in a %s action", k, kind)
		}
	}
	if ak != nil {
		spec, err := ak.Parse(m[kind], m)
		if err != nil {
			return a, err
		}
		return NewAction(kind, spec), nil
	}
	// set
	t, ok := m[kind].(map[string]any)
	if !ok || len(t) == 0 {
		return a, fmt.Errorf("set must be a table of fact = value")
	}
	a.Set = map[string]any{}
	for k, v := range t {
		a.Set[k] = value.Normalize(v)
	}
	return a, nil
}

// hashActions encodes actions for contentHash. Each action is an object
// with, in order:
//
//   - one field per registered kind with a LegacyHashSlot (in registration
//     order), holding the action's spec if it is of that kind and null
//     otherwise: the layout whilst has always hashed, so existing unnamed
//     rules keep their names;
//   - for an action of a kind without a slot, registered or not (one built
//     with NewAction), a field "kind:NAME" with its spec, so a newly
//     registered kind changes only the names of rules that use it;
//   - Set.
func hashActions(actions []Action, reg *Registry) []json.RawMessage {
	if actions == nil {
		return nil
	}
	out := make([]json.RawMessage, 0, len(actions))
	field := func(b *bytes.Buffer, name string, v any) {
		n, _ := json.Marshal(name)
		b.Write(n)
		b.WriteByte(':')
		b.Write(hashJSON(v))
		b.WriteByte(',')
	}
	for _, a := range actions {
		var b bytes.Buffer
		b.WriteByte('{')
		if reg != nil {
			for _, k := range reg.actions {
				if k.LegacyHashSlot == "" {
					continue
				}
				var spec any
				if a.Set == nil && a.kind == k.Name {
					spec = a.Spec
				}
				field(&b, k.LegacyHashSlot, spec)
			}
		}
		if a.Set == nil && a.kind != "" {
			if k := reg.actionKind(a.kind); k == nil || k.LegacyHashSlot == "" {
				field(&b, "kind:"+a.kind, a.Spec)
			}
		}
		b.WriteString(`"Set":`)
		b.Write(hashJSON(a.Set))
		b.WriteByte('}')
		out = append(out, b.Bytes())
	}
	return out
}

// hashJSON encodes v for hashActions. A spec that JSON cannot encode (a NaN,
// a channel) is hashed as its Go syntax instead, so that such actions still
// tell rules apart rather than all hashing alike.
func hashJSON(v any) []byte {
	if j, err := json.Marshal(v); err == nil {
		return j
	}
	j, _ := json.Marshal(fmt.Sprintf("%#v", v))
	return j
}

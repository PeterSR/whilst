package config

import (
	"fmt"
	"io/fs"
	"strings"
)

// Registry holds what an application adds to the config language: the fact
// source kinds and action kinds it implements, and where presets come from.
// The core itself only knows expr and state facts and the set action.
//
// Register everything before loading a config; a Registry is not safe for
// concurrent changes.
type Registry struct {
	// Presets holds the files that `include = ["preset:NAME"]` reads, as
	// NAME.toml. Nil means there are no presets.
	Presets fs.FS
	// Messages words a few config errors in the application's own terms.
	Messages Messages

	facts   []*FactKind
	actions []*ActionKind
}

// Messages lets an application word a few config errors in its own terms.
// An empty field keeps the neutral wording.
type Messages struct {
	// ActionExample is an action table shown when an action is not a
	// table, such as `{ notify = "..." }`. The neutral wording shows
	// `{ set = { NAME = VALUE } }`.
	ActionExample string
	// TempRules says who adds temporary rules, in the error about rule
	// names starting with "ensure:", which are reserved for them; for
	// example "`app ensure`". The neutral wording is "temporary rules".
	TempRules string
}

func (r *Registry) actionExample() string {
	if r != nil && r.Messages.ActionExample != "" {
		return r.Messages.ActionExample
	}
	return "{ set = { NAME = VALUE } }"
}

func (r *Registry) tempRules() string {
	if r != nil && r.Messages.TempRules != "" {
		return r.Messages.TempRules
	}
	return "temporary rules"
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// FactKind is a kind of fact source, such as a D-Bus property or a command.
// A fact table names exactly one kind, by its key.
type FactKind struct {
	// Name is the key that selects the kind in a fact table, such as
	// "command".
	Name string
	// Keys lists the other keys a fact of this kind accepts, besides the
	// generic type, initial, transform and debounce.
	Keys []string
	// Rereadable is set when the source gets a real reading on its own soon
	// after it starts. Only such facts can hold guesses (an initial value,
	// or one remembered from before a reboot), and the engine waits for
	// them to report before the first evaluation. A fact whose source may
	// never report (a signal) is not rereadable: its initial value is taken
	// as real, and a value remembered from before a reboot is dropped.
	Rereadable bool
	// Parse checks the kind's value v and the fact's table t (for the keys
	// in Keys) and returns the parsed source. Its errors are prefixed with
	// the fact's file and name.
	Parse func(v any, t map[string]any) (FactSpec, error)
}

// FactSpec is a parsed fact source of a registered kind. The application
// builds its source from it (see engine.Deps.NewSource).
type FactSpec interface {
	// SourceKey identifies where the value comes from, including the kind:
	// two facts with the same key read the same value. It tells whether a
	// reload or an edit made while whilst was stopped changed a fact, and it
	// is stored in the state file, so it must stay the same across versions
	// for an unchanged definition. Leave out settings that do not change
	// the value, such as timeouts and re-read triggers.
	SourceKey() string
}

// ActionKind is a kind of enter or exit action, such as a notification.
type ActionKind struct {
	// Name is the key that selects the kind in an action table, such as
	// "notify".
	Name string
	// Keys lists the other keys an action of this kind accepts.
	Keys []string
	// Parse checks the kind's value v and the action's table t (for the
	// keys in Keys) and returns the parsed action, which the application's
	// runner receives in Action.Spec.
	//
	// The returned spec also names unnamed rules: they are named after a
	// hash of their content, which includes the JSON encoding of each
	// action's spec. So that encoding must be deterministic and complete:
	// every setting that makes two actions differ must appear in it (in an
	// exported field, not behind a pointer to shared state), and an
	// unchanged action must encode the same way across versions of the
	// application, or the rules that use it are renamed and lose their
	// persisted state. A spec JSON cannot encode is hashed as its Go syntax
	// (fmt's %#v), which is a last resort: pointers in it change between
	// runs.
	Parse func(v any, t map[string]any) (any, error)
	// LegacyHashSlot keeps the names of unnamed rules from before action
	// kinds were pluggable: such a kind has a fixed field in the hash of
	// every action. Leave it empty for new kinds; see Registry.AddActionKind.
	LegacyHashSlot string
}

// coreFactKinds and coreActionKinds are the kinds the core implements itself.
var (
	coreFactKinds   = []string{SrcExpr, SrcState}
	coreActionKinds = []string{"set"}
)

// AddFactKind registers a fact source kind. Kinds are listed in messages in
// the order they were added, before expr and state. It panics on a missing
// parser, a name that is already taken, or a key that is another kind's name
// (in either direction), since a table with that key would name two kinds.
func (r *Registry) AddFactKind(k FactKind) {
	if k.Name == "" || k.Parse == nil {
		panic("config: a fact kind needs a name and a parser")
	}
	if contains(coreFactKinds, k.Name) || r.factKind(k.Name) != nil {
		panic(fmt.Sprintf("config: fact kind %q is registered twice", k.Name))
	}
	for _, key := range k.Keys {
		if contains(genericFactKeys, key) {
			panic(fmt.Sprintf("config: fact kind %q claims the generic key %q", k.Name, key))
		}
		if contains(r.FactKindNames(), key) || key == k.Name {
			panic(fmt.Sprintf("config: fact kind %q claims the key %q, which names a fact kind", k.Name, key))
		}
	}
	for _, other := range r.facts {
		if contains(other.Keys, k.Name) {
			panic(fmt.Sprintf("config: fact kind %q is a key of fact kind %q", k.Name, other.Name))
		}
	}
	r.facts = append(r.facts, &k)
}

// AddActionKind registers an action kind. Kinds are listed in messages in the
// order they were added, before set.
//
// Unnamed rules are named after a hash of their content. An action of a kind
// without a LegacyHashSlot adds only itself to that hash, so registering a new
// kind does not rename rules that do not use it. Kinds with a LegacyHashSlot
// are hashed in every action, in registration order, to keep the names
// whilst gave before kinds were pluggable; changing them or their order
// renames unnamed rules that have actions. A LegacyHashSlot must be unique
// and must not be "Set" or start with "kind:", which the hash uses itself.
//
// It panics on a missing parser, a name that is already taken, a key that is
// another kind's name (in either direction), or a bad LegacyHashSlot.
func (r *Registry) AddActionKind(k ActionKind) {
	if k.Name == "" || k.Parse == nil {
		panic("config: an action kind needs a name and a parser")
	}
	if contains(coreActionKinds, k.Name) || r.actionKind(k.Name) != nil {
		panic(fmt.Sprintf("config: action kind %q is registered twice", k.Name))
	}
	for _, key := range k.Keys {
		if contains(r.ActionKindNames(), key) || key == k.Name {
			panic(fmt.Sprintf("config: action kind %q claims the key %q, which names an action kind", k.Name, key))
		}
	}
	for _, other := range r.actions {
		if contains(other.Keys, k.Name) {
			panic(fmt.Sprintf("config: action kind %q is a key of action kind %q", k.Name, other.Name))
		}
	}
	if slot := k.LegacyHashSlot; slot != "" {
		if slot == "Set" || strings.HasPrefix(slot, "kind:") {
			panic(fmt.Sprintf("config: action kind %q has the reserved hash slot %q", k.Name, slot))
		}
		for _, other := range r.actions {
			if other.LegacyHashSlot == slot {
				panic(fmt.Sprintf("config: action kinds %q and %q share the hash slot %q", other.Name, k.Name, slot))
			}
		}
	}
	r.actions = append(r.actions, &k)
}

// FactKindNames lists every fact kind: registered ones, then expr and state.
func (r *Registry) FactKindNames() []string {
	var out []string
	if r != nil {
		for _, k := range r.facts {
			out = append(out, k.Name)
		}
	}
	return append(out, coreFactKinds...)
}

// ActionKindNames lists every action kind: registered ones, then set.
func (r *Registry) ActionKindNames() []string {
	var out []string
	if r != nil {
		for _, k := range r.actions {
			out = append(out, k.Name)
		}
	}
	return append(out, coreActionKinds...)
}

func (r *Registry) factKind(name string) *FactKind {
	if r == nil {
		return nil
	}
	for _, k := range r.facts {
		if k.Name == name {
			return k
		}
	}
	return nil
}

func (r *Registry) actionKind(name string) *ActionKind {
	if r == nil {
		return nil
	}
	for _, k := range r.actions {
		if k.Name == name {
			return k
		}
	}
	return nil
}

// orList joins names as "a, b or c".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

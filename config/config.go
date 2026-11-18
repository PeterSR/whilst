// Package config loads and validates whilst's TOML configuration.
//
// Config files are decoded into generic maps and checked by hand, so that rule
// tables can carry output keys known only to the engine, and so that every
// unknown key is reported instead of silently ignored.
//
// The core parses expr and state facts, rules and set actions. Every other
// fact source kind and action kind comes from the application, through a
// Registry.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/PeterSR/whilst/expr"
	"github.com/PeterSR/whilst/value"
)

// Config is a loaded configuration, with includes merged in.
type Config struct {
	Path string
	// Files lists the files on disk the config was read from (the main
	// file and its includes, not presets), as absolute paths.
	Files []string
	Facts map[string]*Fact
	// FactOrder lists fact names in the order they were declared.
	FactOrder []string
	Rules     []*Rule
	// Tick is how often expressions that depend on the clock are re-evaluated.
	Tick time.Duration
	// Settle is how long to wait at start for every source to report before
	// the first evaluation.
	Settle time.Duration
}

// The fact kinds the core implements itself. Every other kind is registered
// by the application.
const (
	SrcExpr  = "expr"
	SrcState = "state"
)

// Fact is one declared fact. Facts are built by Load and Parse, which set
// unexported fields from the fact's kind (such as whether it is
// rereadable); a Fact built by hand is not rereadable.
type Fact struct {
	Name   string
	Origin string // file that declared it
	Kind   string // a registered kind, SrcExpr or SrcState
	Type   string // one of value.Kinds(), "" meaning dyn

	Initial    any
	HasInitial bool
	Transform  string
	Debounce   time.Duration

	// Spec is the parsed source of a registered kind; nil for expr and
	// state facts.
	Spec FactSpec
	Expr string
	// States lists the allowed values of a state fact; nil allows any value.
	States []string

	rereadable bool
}

// Rereadable reports whether the fact gets a real reading on its own soon
// after start (see FactKind.Rereadable). expr facts do; state facts do not.
func (f *Fact) Rereadable() bool { return f.rereadable }

// SourceKey identifies where the fact's value comes from (see
// FactSpec.SourceKey).
func (f *Fact) SourceKey() string {
	if f.Spec != nil {
		return f.Spec.SourceKey()
	}
	// expr and state facts keep the key whilst stored before kinds were
	// registered: the kind and then one slot each for dbus, dbus_signal,
	// unit, command, file, expr and state. This seven-slot layout is a
	// frozen historical format, kept because the key is stored in state
	// files; it has nothing to do with which kinds are registered now, and
	// it must not change when kinds are added or removed.
	b, _ := json.Marshal([]any{f.Kind, nil, nil, nil, nil, "", f.Expr, f.States})
	return string(b)
}

// Rule is one [[rule]] table.
type Rule struct {
	Name   string
	Origin string
	While  string
	// Hold, if set, keeps a matching rule matching while it is true.
	Hold     string
	Priority int
	// HasPriority is false when the rule relies on file order.
	HasPriority bool
	Enter       []Action
	Exit        []Action
	// Linger keeps the rule matched this long after its condition turns
	// false. Zero means it ends at once.
	Linger time.Duration
	// Outputs holds the remaining keys, validated by the engine.
	Outputs map[string]any
}

// Reserved rule keys; every other key in a rule is an output.
var ruleKeys = map[string]bool{"name": true, "while": true, "hold": true, "priority": true, "linger": true, "enter": true, "exit": true}

// IsRuleKey reports whether key is one that rules use themselves (name,
// while, hold, priority, linger, enter, exit), so it cannot name an output.
func IsRuleKey(key string) bool { return ruleKeys[key] }

// Empty returns a configuration with no facts and no rules.
func Empty(path string) *Config {
	return &Config{Path: path, Facts: map[string]*Fact{}, Tick: 30 * time.Second, Settle: 3 * time.Second}
}

// Load reads the config at path and everything it includes, with the fact
// and action kinds in reg (which may be nil).
func Load(path string, reg *Registry) (*Config, error) {
	c := Empty(path)
	l := &loader{cfg: c, reg: reg, seen: map[string]bool{}}
	if err := l.file(path, true, 0); err != nil {
		return nil, err
	}
	if err := c.validate(reg); err != nil {
		return nil, err
	}
	return c, nil
}

// Parse loads a config from a string, for tests.
func Parse(src string, reg *Registry) (*Config, error) {
	c := Empty("(inline)")
	l := &loader{cfg: c, reg: reg, seen: map[string]bool{}}
	var raw map[string]any
	if _, err := toml.Decode(src, &raw); err != nil {
		return nil, err
	}
	if err := l.doc(raw, "(inline)", ".", true, 0); err != nil {
		return nil, err
	}
	if err := c.validate(reg); err != nil {
		return nil, err
	}
	return c, nil
}

type loader struct {
	cfg  *Config
	reg  *Registry
	seen map[string]bool
}

func (l *loader) file(path string, top bool, depth int) error {
	var data []byte
	var err error
	dir := "."
	origin := path
	if name, ok := strings.CutPrefix(path, "preset:"); ok {
		if data, err = l.preset(name); err != nil {
			return fmt.Errorf("include %q: no such preset (have: %s)", path, strings.Join(PresetNames(l.reg), ", "))
		}
	} else {
		if abs, aerr := filepath.Abs(path); aerr == nil {
			origin = abs
		}
		if data, err = os.ReadFile(origin); err != nil {
			return err
		}
		dir = filepath.Dir(origin)
		l.cfg.Files = append(l.cfg.Files, origin)
	}
	if l.seen[origin] {
		return fmt.Errorf("%s is included more than once", origin)
	}
	l.seen[origin] = true
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return fmt.Errorf("%s: %w", origin, err)
	}
	return l.doc(raw, origin, dir, top, depth)
}

func (l *loader) preset(name string) ([]byte, error) {
	if l.reg == nil || l.reg.Presets == nil || !fs.ValidPath(name) {
		return nil, fs.ErrNotExist
	}
	return fs.ReadFile(l.reg.Presets, name+".toml")
}

// PresetNames lists the presets in reg, sorted.
func PresetNames(reg *Registry) []string {
	if reg == nil || reg.Presets == nil {
		return nil
	}
	entries, _ := fs.ReadDir(reg.Presets, ".")
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".toml"); ok && !e.IsDir() {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (l *loader) doc(raw map[string]any, origin, dir string, top bool, depth int) error {
	for _, k := range sortedKeys(raw) {
		v := raw[k]
		switch k {
		case "include":
			list, err := stringList(v)
			if err != nil {
				return fmt.Errorf("%s: include: %w", origin, err)
			}
			if depth >= 8 {
				return fmt.Errorf("%s: includes nested too deeply", origin)
			}
			for _, inc := range list {
				p := inc
				if !strings.HasPrefix(p, "preset:") {
					p = expandHome(p)
					if !filepath.IsAbs(p) {
						p = filepath.Join(dir, p)
					}
				}
				if err := l.file(p, false, depth+1); err != nil {
					return fmt.Errorf("%s: %w", origin, err)
				}
			}
		case "fact":
			facts, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: fact must be a table of tables, like [fact.ac]", origin)
			}
			for _, name := range sortedKeys(facts) {
				ft, ok := facts[name].(map[string]any)
				if !ok {
					return fmt.Errorf("%s: fact.%s must be a table", origin, name)
				}
				if prev, dup := l.cfg.Facts[name]; dup {
					return fmt.Errorf("%s: fact %s is already declared in %s", origin, name, prev.Origin)
				}
				f, err := l.parseFact(name, ft)
				if err != nil {
					return fmt.Errorf("%s: fact %s: %w", origin, name, err)
				}
				f.Origin = origin
				l.cfg.Facts[name] = f
				l.cfg.FactOrder = append(l.cfg.FactOrder, name)
			}
		case "rule":
			rules, ok := v.([]map[string]any)
			if !ok {
				return fmt.Errorf("%s: rule must be an array of tables, like [[rule]]", origin)
			}
			for i, rt := range rules {
				r, err := l.parseRule(rt)
				if err != nil {
					label := fmt.Sprintf("rule %d", i+1)
					if n, ok := rt["name"].(string); ok {
						label = "rule " + n
					}
					return fmt.Errorf("%s: %s: %w", origin, label, err)
				}
				r.Origin = origin
				l.cfg.Rules = append(l.cfg.Rules, r)
			}
		case "tick", "settle":
			if !top {
				return fmt.Errorf("%s: %s can only be set in the main config", origin, k)
			}
			d, err := durationValue(v)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", origin, k, err)
			}
			if d <= 0 {
				return fmt.Errorf("%s: %s must be positive", origin, k)
			}
			if k == "tick" {
				l.cfg.Tick = d
			} else {
				l.cfg.Settle = d
			}
		default:
			return fmt.Errorf("%s: unknown key %q (expected include, fact, rule, tick or settle)", origin, k)
		}
	}
	return nil
}

// genericFactKeys are accepted by facts of every registered kind.
var genericFactKeys = []string{"type", "initial", "transform", "debounce"}

func (l *loader) parseFact(name string, t map[string]any) (*Fact, error) {
	if err := expr.ValidName(name); err != nil {
		return nil, err
	}
	f := &Fact{Name: name}
	all := l.reg.FactKindNames()
	var kinds []string
	for _, k := range all {
		if _, ok := t[k]; ok {
			kinds = append(kinds, k)
		}
	}
	switch len(kinds) {
	case 0:
		return nil, fmt.Errorf("needs a source: one of %s", strings.Join(all, ", "))
	case 1:
		f.Kind = kinds[0]
	default:
		return nil, fmt.Errorf("has more than one source (%s)", strings.Join(kinds, ", "))
	}
	kind := l.reg.factKind(f.Kind)
	allowed := map[string]bool{f.Kind: true, "type": true, "initial": true, "debounce": true}
	if kind != nil {
		allowed["transform"] = true
		for _, k := range kind.Keys {
			allowed[k] = true
		}
		f.rereadable = kind.Rereadable
	} else {
		f.rereadable = f.Kind == SrcExpr
	}
	for _, k := range sortedKeys(t) {
		if !allowed[k] {
			return nil, fmt.Errorf("unknown key %q for a %s fact", k, f.Kind)
		}
	}

	var err error
	if v, ok := t["type"]; ok {
		s, ok := v.(string)
		if !ok || !contains(value.Kinds(), s) {
			return nil, fmt.Errorf("type must be one of %s", strings.Join(value.Kinds(), ", "))
		}
		f.Type = s
	}
	if v, ok := t["initial"]; ok {
		f.Initial = value.Normalize(v)
		f.HasInitial = true
	}
	if v, ok := t["transform"]; ok {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("transform must be a CEL expression string")
		}
		f.Transform = s
	}
	if v, ok := t["debounce"]; ok {
		if f.Debounce, err = durationValue(v); err != nil {
			return nil, fmt.Errorf("debounce: %w", err)
		}
	}

	src := t[f.Kind]
	if kind != nil {
		if f.Spec, err = kind.Parse(src, t); err != nil {
			return nil, err
		}
		if f.Spec == nil {
			return nil, fmt.Errorf("the %s fact kind returned no source (a bug in its parser)", f.Kind)
		}
	}
	switch f.Kind {
	case SrcExpr:
		s, ok := src.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("expr must be a CEL expression string")
		}
		f.Expr = s
	case SrcState:
		switch x := src.(type) {
		case bool:
			if !x {
				return nil, fmt.Errorf("state = false makes no sense; use true or a list of allowed values")
			}
		default:
			list, err := stringList(x)
			if err != nil || len(list) == 0 {
				return nil, fmt.Errorf("state must be true or a non-empty list of allowed values")
			}
			f.States = list
			if f.Type == "" {
				f.Type = "string"
			}
		}
		if !f.HasInitial {
			if f.States == nil {
				return nil, fmt.Errorf("a state fact needs an initial value")
			}
			f.Initial = f.States[0]
			f.HasInitial = true
		}
		if v, err := value.Coerce(f.Initial, f.Type); err == nil {
			f.Initial = v
		}
		if err := f.CheckState(f.Initial); err != nil {
			return nil, fmt.Errorf("initial: %w", err)
		}
	}
	if f.HasInitial && f.Kind != SrcState {
		if f.Initial, err = value.Coerce(f.Initial, f.Type); err != nil {
			return nil, fmt.Errorf("initial: %w", err)
		}
	}
	return f, nil
}

// CheckState validates a value for a state fact.
func (f *Fact) CheckState(v any) error {
	if f.Kind != SrcState {
		return fmt.Errorf("%s is not a state fact", f.Name)
	}
	if f.States != nil {
		s, ok := v.(string)
		if !ok || !contains(f.States, s) {
			return fmt.Errorf("%s must be one of %s, not %s", f.Name, strings.Join(f.States, ", "), value.Format(v))
		}
	}
	if f.Type != "" && f.Type != "dyn" && value.Kind(v) != f.Type {
		return fmt.Errorf("%s must be a %s, not %s", f.Name, f.Type, value.Format(v))
	}
	return nil
}

func (l *loader) parseRule(t map[string]any) (*Rule, error) {
	r := &Rule{Outputs: map[string]any{}}
	if v, ok := t["name"]; ok {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("name must be a non-empty string")
		}
		r.Name = s
	}
	w, ok := t["while"]
	if !ok {
		return nil, fmt.Errorf("while is required")
	}
	if r.While, ok = w.(string); !ok || strings.TrimSpace(r.While) == "" {
		return nil, fmt.Errorf("while must be a CEL condition string")
	}
	if v, ok := t["priority"]; ok {
		i, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("priority must be an integer")
		}
		r.Priority = int(i)
		r.HasPriority = true
	}
	if v, ok := t["hold"]; ok {
		if r.Hold, ok = v.(string); !ok || strings.TrimSpace(r.Hold) == "" {
			return nil, fmt.Errorf("hold must be a CEL condition string")
		}
	}
	if v, ok := t["linger"]; ok {
		d, err := durationValue(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("linger must be a positive duration like \"1m\"")
		}
		r.Linger = d
	}
	var err error
	if v, ok := t["enter"]; ok {
		if r.Enter, err = parseActions(v, l.reg); err != nil {
			return nil, fmt.Errorf("enter: %w", err)
		}
	}
	if v, ok := t["exit"]; ok {
		if r.Exit, err = parseActions(v, l.reg); err != nil {
			return nil, fmt.Errorf("exit: %w", err)
		}
	}
	for k, v := range t {
		if !ruleKeys[k] {
			r.Outputs[k] = v
		}
	}
	return r, nil
}

func (c *Config) validate(reg *Registry) error {
	names := map[string]bool{}
	for _, r := range c.Rules {
		if strings.HasPrefix(r.Name, "ensure:") {
			return fmt.Errorf("rule %s: names starting with ensure: are reserved for %s", r.Name, reg.tempRules())
		}
		if r.Name != "" {
			if names[r.Name] {
				return fmt.Errorf("two rules are named %q; rule names must be unique", r.Name)
			}
			names[r.Name] = true
		}
	}
	for _, r := range c.Rules {
		if r.Name == "" {
			// Rule state is persisted and carried over by name, so an
			// unnamed rule is named after its content rather than its
			// position: reordering rules must not swap their state.
			base := "rule-" + contentHash(r, reg)
			r.Name = base
			for i := 2; names[r.Name]; i++ {
				r.Name = fmt.Sprintf("%s-%d", base, i)
			}
			names[r.Name] = true
		}
		for _, a := range append(append([]Action{}, r.Enter...), r.Exit...) {
			for fact, v := range a.Set {
				f, ok := c.Facts[fact]
				if !ok {
					return fmt.Errorf("rule %s: set: no fact named %s", r.Name, fact)
				}
				if cv, err := value.Coerce(v, f.Type); err == nil {
					v = cv
				}
				if err := f.CheckState(v); err != nil {
					return fmt.Errorf("rule %s: set: %w", r.Name, err)
				}
			}
		}
	}
	return nil
}

func contentHash(r *Rule, reg *Registry) string {
	// Outputs go through hashJSON so a value JSON cannot encode (a NaN)
	// still tells rules apart.
	b, _ := json.Marshal(struct {
		While   string
		Hold    string `json:",omitempty"`
		Outputs json.RawMessage
		Enter   []json.RawMessage
		Exit    []json.RawMessage
	}{r.While, r.Hold, hashJSON(r.Outputs), hashActions(r.Enter, reg), hashActions(r.Exit, reg)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// helpers

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func stringList(v any) ([]string, error) {
	switch x := v.(type) {
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("expected a list of strings")
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return x, nil
	}
	return nil, fmt.Errorf("expected a string or a list of strings")
}

// StringList accepts a string or a list of strings, for parsers of
// registered kinds and outputs.
func StringList(v any) ([]string, error) { return stringList(v) }

// Fields copies string fields out of a table, rejecting unknown keys and
// requiring the keys in required, for parsers of registered kinds.
func Fields(t map[string]any, dst map[string]*string, required ...string) error {
	return fields(t, dst, required...)
}

// Duration parses a config duration, which always needs a unit ("30s").
func Duration(v any) (time.Duration, error) { return durationValue(v) }

// ExpandHome replaces a leading ~ in a path with the home directory.
func ExpandHome(p string) string { return expandHome(p) }

// fields copies string fields out of a table, rejecting unknown keys.
func fields(t map[string]any, dst map[string]*string, required ...string) error {
	for _, k := range sortedKeys(t) {
		p, ok := dst[k]
		if !ok {
			return fmt.Errorf("unknown key %q", k)
		}
		s, ok := t[k].(string)
		if !ok {
			return fmt.Errorf("%s must be a string", k)
		}
		*p = s
	}
	for _, k := range required {
		p, ok := dst[k]
		if !ok {
			// A bug in the caller, not in the config.
			return fmt.Errorf("required key %q has no destination", k)
		}
		if *p == "" {
			return fmt.Errorf("%s is required", k)
		}
	}
	return nil
}

func durationValue(v any) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("expected a duration string like \"30s\" or \"5m\"")
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use a unit, like \"30s\", \"5m\" or \"1h\")", s)
	}
	return d, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

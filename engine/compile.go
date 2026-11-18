package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/expr"
	"github.com/PeterSR/whilst/plugin"
)

// Compiled is a config turned into programs, ready to run.
type Compiled struct {
	Config *config.Config
	Env    *expr.Env
	Facts  map[string]*FactDef
	// ExprOrder lists expr facts so that every one comes after the expr
	// facts it reads.
	ExprOrder []string
	Rules     []*RuleDef
	Warnings  []string
}

// FactDef is a fact's config plus its compiled expressions.
type FactDef struct {
	*config.Fact
	Prog      *expr.Program // expr facts
	TransProg *expr.Program // transform
}

// RuleDef is a rule's config plus its compiled condition and parsed outputs.
type RuleDef struct {
	Name     string
	Origin   string
	Priority int
	Linger   time.Duration
	While    *expr.Program
	Hold     *expr.Program
	Outputs  map[string]any
	Enter    []config.Action
	Exit     []config.Action
}

// Compile type-checks every expression in cfg and parses rule outputs. Each
// output must have a unique Name that is not a key rules use themselves
// (see config.IsRuleKey). Compile reads only its arguments, so it may run on
// any goroutine; it calls Output.Parse.
func Compile(cfg *config.Config, outputs []plugin.Output) (*Compiled, error) {
	types := map[string]string{}
	for name, f := range cfg.Facts {
		types[name] = f.Type
	}
	env, err := expr.NewEnv(types)
	if err != nil {
		return nil, err
	}
	c := &Compiled{Config: cfg, Env: env, Facts: map[string]*FactDef{}}
	var tenv *expr.Env
	for _, name := range cfg.FactOrder {
		f := cfg.Facts[name]
		d := &FactDef{Fact: f}
		if f.Kind == config.SrcExpr {
			if d.Prog, err = env.Compile(f.Expr, f.Type == "bool"); err != nil {
				return nil, fmt.Errorf("fact %s: expr: %w", name, err)
			}
			if d.Prog.Ticks {
				c.Warnings = append(c.Warnings, fmt.Sprintf("fact %s depends on the clock in a way whilst cannot schedule exactly; it is re-evaluated every %s", name, cfg.Tick))
			}
		}
		if f.Transform != "" {
			if tenv == nil {
				if tenv, err = expr.NewTransformEnv(); err != nil {
					return nil, err
				}
			}
			if d.TransProg, err = tenv.Compile(f.Transform, false); err != nil {
				return nil, fmt.Errorf("fact %s: transform: %w", name, err)
			}
		}
		c.Facts[name] = d
	}
	if c.ExprOrder, err = exprOrder(c.Facts, cfg.FactOrder); err != nil {
		return nil, err
	}

	byName := map[string]plugin.Output{}
	var outNames []string
	for _, o := range outputs {
		n := o.Name()
		switch {
		case n == "":
			return nil, fmt.Errorf("an output has no name")
		case config.IsRuleKey(n):
			return nil, fmt.Errorf("output %q has the name of a rule key", n)
		case byName[n] != nil:
			return nil, fmt.Errorf("two outputs are named %q", n)
		}
		byName[n] = o
		outNames = append(outNames, n)
	}
	for _, r := range cfg.Rules {
		d, err := compileRule(env, r.Name, r.While, r.Outputs, byName, outNames)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", r.Name, err)
		}
		if r.Hold != "" {
			if d.Hold, err = env.Compile(r.Hold, true); err != nil {
				return nil, fmt.Errorf("rule %s: hold: %w", r.Name, err)
			}
		}
		d.Origin = r.Origin
		d.Enter, d.Exit, d.Linger = r.Enter, r.Exit, r.Linger
		if r.HasPriority {
			d.Priority = r.Priority
		}
		if d.While.Ticks || (d.Hold != nil && d.Hold.Ticks) {
			c.Warnings = append(c.Warnings, fmt.Sprintf("rule %s depends on the clock in a way whilst cannot schedule exactly; it is re-evaluated every %s", r.Name, cfg.Tick))
		}
		if len(d.Outputs) == 0 && len(d.Enter) == 0 && len(d.Exit) == 0 {
			c.Warnings = append(c.Warnings, fmt.Sprintf("rule %s has no outputs and no actions", r.Name))
		}
		c.Rules = append(c.Rules, d)
	}
	// Higher priority first; file order breaks ties.
	sort.SliceStable(c.Rules, func(i, j int) bool { return c.Rules[i].Priority > c.Rules[j].Priority })
	return c, nil
}

func compileRule(env *expr.Env, name, while string, raw map[string]any, byName map[string]plugin.Output, outNames []string) (*RuleDef, error) {
	d := &RuleDef{Name: name, Outputs: map[string]any{}}
	var err error
	if d.While, err = env.Compile(while, true); err != nil {
		return nil, fmt.Errorf("while: %w", err)
	}
	for k, v := range raw {
		o, ok := byName[k]
		if !ok {
			return nil, fmt.Errorf("unknown key %q (outputs are: %s; other keys are name, while, hold, priority, linger, enter, exit)", k, strings.Join(outNames, ", "))
		}
		claim, err := o.Parse(v)
		if err != nil {
			return nil, err
		}
		d.Outputs[k] = claim
	}
	return d, nil
}

func exprOrder(facts map[string]*FactDef, declared []string) ([]string, error) {
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var order []string
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("expr facts depend on each other in a cycle: %s", strings.Join(append(path, name), " -> "))
		}
		state[name] = visiting
		for _, dep := range facts[name].Prog.Facts {
			if d, ok := facts[dep]; ok && d.Kind == config.SrcExpr {
				if err := visit(dep, append(path, name)); err != nil {
					return err
				}
			}
		}
		state[name] = done
		order = append(order, name)
		return nil
	}
	for _, name := range declared {
		if facts[name].Kind == config.SrcExpr {
			if err := visit(name, nil); err != nil {
				return nil, err
			}
		}
	}
	return order, nil
}

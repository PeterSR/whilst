// Package expr compiles and evaluates the CEL expressions used in rule
// conditions and expression facts, and analyses them for the facts they read
// and the moments at which their result may change because time passes.
package expr

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"

	"github.com/PeterSR/whilst/value"
)

// ClockVars lists the clock variables available in every expression. They
// are evaluated in local time; weekday is 1 for Monday through 7 for Sunday.
// It returns a new slice on every call.
func ClockVars() []string { return []string{"now", "hour", "minute", "weekday"} }

// IsReserved reports whether name is reserved, so it cannot be used as a
// fact name: the clock variables, value (the input of a transform), the
// functions since, duration and timestamp, and CEL's keywords and literals.
func IsReserved(name string) bool { return reserved[name] }

var reserved = map[string]bool{
	"now": true, "hour": true, "minute": true, "weekday": true, "value": true,
	"since": true, "duration": true, "timestamp": true,
	// CEL keywords and literals.
	"true": true, "false": true, "null": true, "in": true, "as": true, "break": true,
	"const": true, "continue": true, "else": true, "for": true, "function": true,
	"if": true, "import": true, "let": true, "loop": true, "package": true,
	"namespace": true, "return": true, "var": true, "void": true, "while": true,
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidName reports whether name can be used as a fact name.
func ValidName(name string) error {
	if !identRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid name (letters, digits and _, not starting with a digit)", name)
	}
	if reserved[name] {
		return fmt.Errorf("%q is a reserved name", name)
	}
	return nil
}

// Never is what since() returns for a fact that has not changed while whilst
// was watching it: long enough ago that every "within the last X" test fails.
const Never = 100 * 365 * 24 * time.Hour

// Context supplies what an evaluation needs besides fact values.
type Context struct {
	Now   time.Time
	Since func(name string) time.Duration
}

// Env is a CEL environment declaring one variable per fact.
type Env struct {
	env   *cel.Env
	facts map[string]bool
	ctx   *Context // set for the duration of one Eval call
}

// NewEnv declares the given facts (name to type name, see value.Kinds).
func NewEnv(facts map[string]string) (*Env, error) {
	e := &Env{facts: map[string]bool{}}
	opts := []cel.EnvOption{
		cel.Variable("now", cel.TimestampType),
		cel.Variable("hour", cel.IntType),
		cel.Variable("minute", cel.IntType),
		cel.Variable("weekday", cel.IntType),
		cel.Function("since",
			cel.Overload("since_string", []*cel.Type{cel.StringType}, cel.DurationType,
				cel.UnaryBinding(func(arg ref.Val) ref.Val {
					name, ok := arg.Value().(string)
					if !ok {
						return types.NewErr("since() needs a fact name")
					}
					if e.ctx == nil || e.ctx.Since == nil {
						return types.Duration{Duration: Never}
					}
					return types.Duration{Duration: e.ctx.Since(name)}
				}))),
	}
	names := make([]string, 0, len(facts))
	for n := range facts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t, err := celType(facts[n])
		if err != nil {
			return nil, fmt.Errorf("fact %s: %w", n, err)
		}
		opts = append(opts, cel.Variable(n, t))
		e.facts[n] = true
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, err
	}
	e.env = env
	return e, nil
}

// NewTransformEnv is the environment for a fact's `transform`, which sees the
// raw source value as `value`.
func NewTransformEnv() (*Env, error) {
	env, err := cel.NewEnv(cel.Variable("value", cel.DynType))
	if err != nil {
		return nil, err
	}
	return &Env{env: env, facts: map[string]bool{"value": true}}, nil
}

func celType(kind string) (*cel.Type, error) {
	switch kind {
	case "", "dyn":
		return cel.DynType, nil
	case "bool":
		return cel.BoolType, nil
	case "int":
		return cel.IntType, nil
	case "uint":
		return cel.UintType, nil
	case "double":
		return cel.DoubleType, nil
	case "string":
		return cel.StringType, nil
	case "list":
		return types.NewListType(cel.DynType), nil
	case "map":
		return types.NewMapType(cel.StringType, cel.DynType), nil
	}
	return nil, fmt.Errorf("unknown type %q", kind)
}

// Threshold says the expression may change value when the named fact has gone
// unchanged for After.
type Threshold struct {
	Fact  string
	After time.Duration
}

// Program is a compiled expression with what analysis learned about it.
type Program struct {
	Source string
	// Facts lists the facts the expression reads, directly or through since().
	Facts []string
	// Thresholds lists recognised `since('x') < duration('...')` comparisons.
	Thresholds []Threshold
	// Ticks is true when the result can change with time in a way that is not
	// covered by Thresholds, so the engine must re-evaluate periodically.
	Ticks bool

	env  *Env
	prog cel.Program
	bool bool
}

// Compile parses and type-checks src. When wantBool is set the result must be
// a bool.
func (e *Env) Compile(src string, wantBool bool) (*Program, error) {
	ast, iss := e.env.Compile(src)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("%s", iss.String())
	}
	if wantBool {
		ot := ast.OutputType()
		if !ot.IsExactType(cel.BoolType) && !ot.IsExactType(cel.DynType) {
			return nil, fmt.Errorf("%q must be a condition (bool), but it is %s", src, ot)
		}
	}
	p := &Program{Source: src, env: e, bool: wantBool}
	if err := p.analyse(ast.NativeRep().Expr()); err != nil {
		return nil, err
	}
	prog, err := e.env.Program(ast, cel.CostLimit(1_000_000), cel.InterruptCheckFrequency(100))
	if err != nil {
		return nil, err
	}
	p.prog = prog
	return p, nil
}

func (p *Program) analyse(root celast.Expr) error {
	facts := map[string]bool{}
	var sinceCalls, matched int
	var problem error
	thresholds := map[Threshold]bool{}

	sinceArg := func(x celast.Expr) (string, bool) {
		if x.Kind() != celast.CallKind {
			return "", false
		}
		c := x.AsCall()
		if c.FunctionName() != "since" || len(c.Args()) != 1 || c.Args()[0].Kind() != celast.LiteralKind {
			return "", false
		}
		s, ok := c.Args()[0].AsLiteral().Value().(string)
		return s, ok
	}
	durArg := func(x celast.Expr) (time.Duration, bool) {
		if x.Kind() != celast.CallKind {
			return 0, false
		}
		c := x.AsCall()
		if c.FunctionName() != "duration" || len(c.Args()) != 1 || c.Args()[0].Kind() != celast.LiteralKind {
			return 0, false
		}
		s, ok := c.Args()[0].AsLiteral().Value().(string)
		if !ok {
			return 0, false
		}
		d, err := time.ParseDuration(s)
		return d, err == nil
	}

	celast.PostOrderVisit(root, celast.NewExprVisitor(func(x celast.Expr) {
		switch x.Kind() {
		case celast.IdentKind:
			n := x.AsIdent()
			switch n {
			case "now", "hour", "minute", "weekday":
				p.Ticks = true
			default:
				if p.env.facts[n] {
					facts[n] = true
				}
			}
		case celast.CallKind:
			c := x.AsCall()
			switch c.FunctionName() {
			case "since":
				sinceCalls++
				name, ok := sinceArg(x)
				if !ok {
					problem = fmt.Errorf("since() takes a fact name as a literal string, like since('ac')")
					return
				}
				if !p.env.facts[name] {
					problem = fmt.Errorf("since(%q): no such fact", name)
					return
				}
				facts[name] = true
			case operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals:
				if len(c.Args()) != 2 {
					return
				}
				a, b := c.Args()[0], c.Args()[1]
				name, ok := sinceArg(a)
				d, ok2 := durArg(b)
				if !ok || !ok2 {
					name, ok = sinceArg(b)
					d, ok2 = durArg(a)
				}
				if ok && ok2 {
					matched++
					thresholds[Threshold{Fact: name, After: d}] = true
				}
			}
		}
	}))
	if problem != nil {
		return problem
	}
	if sinceCalls > matched {
		p.Ticks = true
	}
	for n := range facts {
		p.Facts = append(p.Facts, n)
	}
	sort.Strings(p.Facts)
	for t := range thresholds {
		p.Thresholds = append(p.Thresholds, t)
	}
	sort.Slice(p.Thresholds, func(i, j int) bool {
		if p.Thresholds[i].Fact != p.Thresholds[j].Fact {
			return p.Thresholds[i].Fact < p.Thresholds[j].Fact
		}
		return p.Thresholds[i].After < p.Thresholds[j].After
	})
	return nil
}

// Eval evaluates the program. vars holds fact values in normal form (see the
// value package); a fact without a value is simply absent.
func (p *Program) Eval(vars map[string]any, ctx *Context) (any, error) {
	act := make(map[string]any, len(vars)+4)
	for k, v := range vars {
		act[k] = v
	}
	if ctx != nil {
		lt := ctx.Now.Local()
		act["now"] = ctx.Now
		act["hour"] = int64(lt.Hour())
		act["minute"] = int64(lt.Minute())
		wd := int64(lt.Weekday())
		if wd == 0 {
			wd = 7
		}
		act["weekday"] = wd
	}
	p.env.ctx = ctx
	out, _, err := p.prog.Eval(act)
	p.env.ctx = nil
	if err != nil {
		return nil, err
	}
	v, err := native(out)
	if err != nil {
		return nil, err
	}
	if p.bool {
		if _, ok := v.(bool); !ok {
			return nil, fmt.Errorf("condition evaluated to %v, not a bool", v)
		}
	}
	return v, nil
}

// EvalBool evaluates a condition.
func (p *Program) EvalBool(vars map[string]any, ctx *Context) (bool, error) {
	v, err := p.Eval(vars, ctx)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("condition evaluated to %v, not a bool", v)
	}
	return b, nil
}

// native converts a CEL result into the value package's normal form.
func native(v ref.Val) (any, error) {
	switch x := v.(type) {
	case types.Bool:
		return bool(x), nil
	case types.Int:
		return int64(x), nil
	case types.Uint:
		return uint64(x), nil
	case types.Double:
		return value.Normalize(float64(x)), nil
	case types.String:
		return string(x), nil
	case types.Null:
		return nil, nil
	case types.Duration:
		return x.Duration.String(), nil
	case types.Timestamp:
		return x.Time.UTC().Format(time.RFC3339Nano), nil
	}
	if types.IsError(v) {
		return nil, fmt.Errorf("%v", v)
	}
	if l, ok := v.(traits.Lister); ok {
		n, ok := l.Size().(types.Int)
		if !ok {
			return nil, fmt.Errorf("list without a size")
		}
		out := make([]any, int(n))
		for i := range out {
			e, err := native(l.Get(types.Int(i)))
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	}
	if m, ok := v.(traits.Mapper); ok {
		out := map[string]any{}
		it := m.Iterator()
		for it.HasNext() == types.True {
			k := it.Next()
			kv, err := native(k)
			if err != nil {
				return nil, err
			}
			e, err := native(m.Get(k))
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(kv)] = e
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported result type %s", v.Type())
}

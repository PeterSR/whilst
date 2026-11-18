package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// TempSpec describes a temporary rule, added at run time with Ensure. Its
// JSON form is stored in the state file.
type TempSpec struct {
	Name    string         `json:"name,omitempty"`
	While   string         `json:"while,omitempty"`
	Until   time.Time      `json:"until,omitzero"`
	Outputs map[string]any `json:"outputs"`
	// Attach ties the rule to the caller (see plugin.Lifetimes):
	// it is dropped when the caller exits.
	Attach bool `json:"attach,omitempty"`
}

// temp is a temporary rule's extra state.
type temp struct {
	ID      string
	Spec    TempSpec
	Until   time.Time
	Owner   string // holder id of an attached caller; see plugin.Lifetimes
	Created time.Time
	gone    bool // released, or its owner exited
	unwatch func()
}

func (t *temp) finished(at time.Time) bool {
	return t.gone || (!t.Until.IsZero() && !at.Before(t.Until))
}

// ErrNoOutputs is returned by Ensure when the spec names no outputs.
var ErrNoOutputs = errors.New("name at least one output")

// ErrNoLifetimes is returned by Ensure when the spec asks to attach but the
// caller cannot be tracked: there is no sender, or no Deps.Lifetimes.
var ErrNoLifetimes = errors.New("attach needs a way to track the caller")

// tempPriority puts temporary rules ahead of every config rule.
const tempPriority = 1 << 30

func (e *Engine) compileTemp(id string, spec TempSpec) (*rule, error) {
	while := strings.TrimSpace(spec.While)
	if while == "" {
		while = "true"
	}
	if len(spec.Outputs) == 0 {
		return nil, ErrNoOutputs
	}
	name := spec.Name
	if name == "" {
		name = id
	}
	name = "ensure:" + name
	for _, r := range e.allRules() {
		if r.Temp != nil && r.Temp.gone {
			continue // ended; dropped at the next evaluation
		}
		if r.Def.Name == name {
			return nil, fmt.Errorf("a rule named %s already exists", name)
		}
	}
	byName, names := e.outputIndex()
	d, err := compileRule(e.comp.Env, name, while, spec.Outputs, byName, names)
	if err != nil {
		return nil, err
	}
	d.Origin = "command line"
	d.Priority = tempPriority
	return &rule{Def: d, Temp: &temp{ID: id, Spec: spec, Until: spec.Until}}, nil
}

// Ensure adds a temporary rule. sender identifies the caller for
// Deps.Lifetimes (a holder id, such as a connection name) and is used when
// the spec asks to attach. It must run on the engine goroutine.
//
// It returns ErrNoOutputs when the spec names no outputs and ErrNoLifetimes
// when it asks to attach but the caller cannot be tracked, so an
// application can word those in its own terms.
func (e *Engine) Ensure(spec TempSpec, sender string) (string, error) {
	t := now()
	if !spec.Until.IsZero() && !spec.Until.After(t) {
		return "", fmt.Errorf("that time has already passed")
	}
	e.tempSeq++
	id := fmt.Sprintf("t%d", e.tempSeq)
	r, err := e.compileTemp(id, spec)
	if err != nil {
		e.tempSeq--
		return "", err
	}
	r.Temp.Created = t
	if spec.Attach {
		if sender == "" || e.deps.Lifetimes == nil {
			return "", ErrNoLifetimes
		}
		tmp := r.Temp
		tmp.Owner = sender
		cancel, err := e.deps.Lifetimes.Watch(sender, func() {
			e.post(func() {
				if !tmp.gone {
					slog.Info("caller exited; dropping temporary rule", "rule", r.Def.Name)
					tmp.gone = true
					e.dirty = true
				}
			})
		})
		if err != nil {
			return "", err
		}
		tmp.unwatch = cancel
		if !e.deps.Lifetimes.Alive(sender) {
			cancel()
			return "", fmt.Errorf("caller already exited")
		}
	}
	e.temps = append(e.temps, r)
	slog.Info("added temporary rule", "rule", r.Def.Name, "while", r.Def.While.Source, "until", spec.Until)
	e.dirty = true
	return id, nil
}

// EnsureNow adds a temporary rule and applies it before returning, so the
// caller knows the outputs are in place. problems lists outputs the rule
// wants that could not be applied (they are retried). It must run on the
// engine goroutine.
//
// waiting is set when the rule's condition cannot be evaluated yet (a fact
// has no confirmed value); the rule then applies as soon as it can. That is
// not a problem: a false condition is not one either.
func (e *Engine) EnsureNow(spec TempSpec, sender string) (id string, problems []string, waiting string, err error) {
	id, err = e.Ensure(spec, sender)
	if err != nil {
		return "", nil, "", err
	}
	if !e.settled {
		// An explicit request should not wait for slow sources.
		e.settled = true
	}
	e.dirty = false
	e.Evaluate(now())
	var tr *rule
	for _, r := range e.temps {
		if r.Temp.ID == id {
			tr = r
		}
	}
	if tr == nil {
		return id, nil, "", nil
	}
	if tr.Waiting {
		waiting = tr.Err
	} else if tr.Err != "" {
		problems = append(problems, "condition: "+tr.Err)
	}
	for _, o := range e.outputs {
		if _, ok := tr.Def.Outputs[o.Name()]; !ok {
			continue
		}
		for _, it := range o.Status() {
			wanted := false
			for _, w := range it.WantedBy {
				wanted = wanted || w == tr.Def.Name
			}
			if wanted && !it.Held {
				msg := o.Name() + " " + it.Key + " is not held"
				if it.Err != "" {
					msg += ": " + it.Err
				}
				problems = append(problems, msg)
			}
		}
	}
	return id, problems, waiting, nil
}

// Release drops temporary rules: the one with the given id or name, or all of
// them when id is empty. It returns how many were dropped. It must run on the
// engine goroutine.
func (e *Engine) Release(id string) int {
	n := 0
	for _, r := range e.temps {
		if r.Temp.gone {
			continue
		}
		if id == "" || r.Temp.ID == id || r.Def.Name == id || r.Def.Name == "ensure:"+id {
			r.Temp.gone = true
			n++
		}
	}
	if n > 0 {
		e.dirty = true
	}
	return n
}

// dropFinishedTemps forgets temporary rules that have ended and no longer
// match (so their outputs were already released).
func (e *Engine) dropFinishedTemps(t time.Time) {
	kept := e.temps[:0]
	for _, r := range e.temps {
		if r.Temp.finished(t) && !r.Matched {
			if r.Temp.unwatch != nil {
				r.Temp.unwatch()
			}
			slog.Info("temporary rule ended", "rule", r.Def.Name)
			continue
		}
		kept = append(kept, r)
	}
	for i := len(kept); i < len(e.temps); i++ {
		e.temps[i] = nil
	}
	e.temps = kept
}

package engine

import (
	"slices"
	"sort"
	"time"

	"github.com/PeterSR/whilst/plugin"
)

// Status is a snapshot of the engine: every fact, rule and output and why it
// is in its state. Its JSON form is meant to be shown to users or sent to
// clients.
type Status struct {
	Config      string         `json:"config"`
	ConfigError string         `json:"config_error,omitempty"`
	Started     time.Time      `json:"started"`
	Now         time.Time      `json:"now"`
	NextWake    time.Time      `json:"next_wake,omitzero"`
	Settled     bool           `json:"settled"`
	Facts       []FactStatus   `json:"facts"`
	Rules       []RuleStatus   `json:"rules"`
	Outputs     []OutputStatus `json:"outputs"`
	Warnings    []string       `json:"warnings,omitempty"`
}

// FactStatus describes one fact.
type FactStatus struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Source      string    `json:"source,omitempty"`
	Value       any       `json:"value"`
	HasValue    bool      `json:"has_value"`
	Provisional bool      `json:"provisional,omitempty"`
	Changed     time.Time `json:"changed,omitzero"`
	Updated     time.Time `json:"updated,omitzero"`
	Error       string    `json:"error,omitempty"`
	Pending     any       `json:"pending,omitempty"`
	PendingDue  time.Time `json:"pending_due,omitzero"`
}

// RuleStatus describes one rule.
type RuleStatus struct {
	Name   string `json:"name"`
	Origin string `json:"origin,omitempty"`
	While  string `json:"while"`
	Hold   string `json:"hold,omitempty"`
	// Holding is set while the rule matches only through hold.
	Holding  bool           `json:"holding,omitempty"`
	Priority int            `json:"priority"`
	Matched  bool           `json:"matched"`
	Since    time.Time      `json:"since,omitzero"`
	Error    string         `json:"error,omitempty"`
	Facts    []string       `json:"facts,omitempty"`
	Outputs  map[string]any `json:"outputs,omitempty"`
	Enter    []string       `json:"enter,omitempty"`
	Exit     []string       `json:"exit,omitempty"`
	TempID   string         `json:"temp_id,omitempty"`
	Until    time.Time      `json:"until,omitzero"`
	// LingerUntil is set while the rule lingers after its condition
	// turned false.
	LingerUntil time.Time `json:"linger_until,omitzero"`
	Attached    bool      `json:"attached,omitempty"`
}

// OutputStatus describes one output.
type OutputStatus struct {
	Output string        `json:"output"`
	Items  []plugin.Item `json:"items"`
}

// Snapshot builds a Status. It must run on the engine goroutine.
func (e *Engine) Snapshot() Status {
	s := Status{
		Config:      e.comp.Config.Path,
		ConfigError: e.configError,
		Started:     e.started,
		Now:         now(),
		NextWake:    e.nextWake,
		Settled:     e.settled,
		Warnings:    e.comp.Warnings,
	}
	names := make([]string, 0, len(e.facts))
	for n := range e.facts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := e.facts[n]
		fs := FactStatus{
			Name: n, Kind: f.Def.Kind, Value: f.Value, HasValue: f.Has, Provisional: f.Provisional,
			Changed: f.Changed, Updated: f.Updated, Error: f.Err,
		}
		if f.src != nil {
			fs.Source = f.src.Describe()
		} else if f.Def.Expr != "" {
			fs.Source = f.Def.Expr
		}
		if f.pending != nil {
			fs.Pending, fs.PendingDue = f.pending.value, f.pending.due
		}
		s.Facts = append(s.Facts, fs)
	}
	for _, r := range e.allRules() {
		rs := RuleStatus{
			Name: r.Def.Name, Origin: r.Def.Origin, While: r.Def.While.Source, Priority: r.Def.Priority,
			Matched: r.Matched, Since: r.Since, Error: r.Err, Facts: r.Def.While.Facts,
		}
		if r.Def.Hold != nil {
			rs.Hold = r.Def.Hold.Source
			rs.Holding = r.Holding
			rs.Facts = slices.Clone(rs.Facts)
			for _, f := range r.Def.Hold.Facts {
				if !slices.Contains(rs.Facts, f) {
					rs.Facts = append(rs.Facts, f)
				}
			}
		}
		if r.Err == "" {
			// While the condition cannot be evaluated, the rule keeps its
			// state and is not known to be lingering.
			rs.LingerUntil = r.lingerUntil()
		}
		if len(r.Def.Outputs) > 0 {
			rs.Outputs = r.Def.Outputs
		}
		for _, a := range r.Def.Enter {
			rs.Enter = append(rs.Enter, a.Kind())
		}
		for _, a := range r.Def.Exit {
			rs.Exit = append(rs.Exit, a.Kind())
		}
		if r.Temp != nil {
			rs.TempID, rs.Until, rs.Attached = r.Temp.ID, r.Temp.Until, r.Temp.Spec.Attach
		}
		s.Rules = append(s.Rules, rs)
	}
	for _, o := range e.outputs {
		s.Outputs = append(s.Outputs, OutputStatus{Output: o.Name(), Items: o.Status()})
	}
	return s
}

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/plugin"
	"github.com/PeterSR/whilst/value"
)

// stateFile is what is persisted between runs:
//
//   - Fact values and when they last changed, so since() survives a restart
//     of whilst. After a reboot the values are only kept as provisional
//     guesses (except state facts), because whatever happened while the
//     machine was off was not observed.
//   - Whether each rule matched, so restarting whilst does not re-run enter
//     actions.
//   - Output state that outlives the process, such as which services an
//     output started.
//   - Temporary rules that are not attached to a caller.
type stateFile struct {
	Version int                        `json:"version"`
	BootID  string                     `json:"boot_id,omitempty"`
	Facts   map[string]factState       `json:"facts,omitempty"`
	Rules   map[string]ruleState       `json:"rules,omitempty"`
	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`
	Temps   []tempState                `json:"temps,omitempty"`
	TempSeq int                        `json:"temp_seq,omitempty"`
}

type factState struct {
	Value   any       `json:"value"`
	Changed time.Time `json:"changed,omitzero"`
	// Def identifies the fact's definition (source, type, transform). A
	// value remembered under another definition is only a guess.
	Def string `json:"def,omitempty"`
}

type ruleState struct {
	Matched    bool      `json:"matched"`
	Since      time.Time `json:"since,omitzero"`
	FalseSince time.Time `json:"false_since,omitzero"`
}

type tempState struct {
	ID      string    `json:"id"`
	Spec    TempSpec  `json:"spec"`
	Created time.Time `json:"created"`
	Matched bool      `json:"matched"`
}

// Restore loads persisted state. Call it after New and before Run, on the
// goroutine that then calls Run (or before starting it). With an empty
// Deps.StatePath it does nothing.
func (e *Engine) Restore() error {
	if e.deps.StatePath == "" {
		return nil
	}
	b, err := os.ReadFile(e.deps.StatePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var st stateFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&st); err != nil {
		// Keep the broken file for inspection instead of overwriting it.
		bad := e.deps.StatePath + ".corrupt"
		if rerr := os.Rename(e.deps.StatePath, bad); rerr == nil {
			return fmt.Errorf("%s: %w (moved to %s)", e.deps.StatePath, err, bad)
		}
		return fmt.Errorf("%s: %w", e.deps.StatePath, err)
	}
	sameBoot := st.BootID != "" && st.BootID == e.deps.BootID
	for name, fs := range st.Facts {
		f, ok := e.facts[name]
		if !ok {
			continue
		}
		v := value.FromJSON(fs.Value)
		if f.Def.Kind == config.SrcState {
			if cv, err := value.Coerce(v, f.Def.Type); err == nil {
				v = cv
			}
			if f.Def.CheckState(v) != nil {
				continue
			}
			f.Value, f.Has, f.Provisional, f.Changed = v, true, false, fs.Changed
			continue
		}
		if !f.Def.Rereadable() {
			// A signal fact is never read, so a remembered value would stay
			// a guess forever. Within a boot it is still right; after a
			// reboot the configured initial value applies.
			if cv, err := value.Coerce(v, f.Def.Type); sameBoot && defMatches(fs.Def, f.Def) && err == nil {
				f.Value, f.Has, f.Provisional, f.Changed = cv, true, false, fs.Changed
			}
			continue
		}
		if cv, err := value.Coerce(v, f.Def.Type); err == nil {
			v = cv
		} else {
			continue
		}
		f.Value, f.Has = v, true
		if sameBoot && !defMatches(fs.Def, f.Def) {
			// Edited while whilst was stopped: keep since() running, but
			// treat the value as a guess, like a reload would.
			f.Provisional, f.Changed = true, fs.Changed
		} else if sameBoot {
			f.Provisional, f.Changed = false, fs.Changed
		} else {
			f.Provisional, f.Changed = true, time.Time{}
		}
	}
	// Rule state and output state only make sense within one boot: a reboot
	// undoes what enter actions did and stops what outputs started, so
	// after one, rules start fresh and enter actions run again.
	if !sameBoot {
		e.tempSeq = st.TempSeq
		e.restoreTemps(st.Temps)
		return nil
	}
	for _, r := range e.rules {
		if rs, ok := st.Rules[r.Def.Name]; ok {
			r.Matched, r.Since, r.FalseSince = rs.Matched, rs.Since, rs.FalseSince
		}
	}
	for _, o := range e.outputs {
		if raw, ok := st.Outputs[o.Name()]; ok {
			if err := o.Load(raw); err != nil {
				slog.Warn("cannot restore output state", "output", o.Name(), "error", err)
			}
		}
	}
	e.tempSeq = st.TempSeq
	e.restoreTemps(st.Temps)
	return nil
}

func (e *Engine) restoreTemps(temps []tempState) {
	for _, ts := range temps {
		if !ts.Spec.Until.IsZero() && !ts.Spec.Until.After(now()) {
			continue
		}
		r, err := e.compileTemp(ts.ID, ts.Spec)
		if err != nil {
			slog.Warn("dropping temporary rule that no longer compiles", "id", ts.ID, "error", err)
			continue
		}
		r.Temp.Created = ts.Created
		r.Matched = ts.Matched
		e.temps = append(e.temps, r)
	}
}

// defMatches compares a stored definition key with a fact's definition. A
// state file from before keys were stored has none; it is taken as a match
// rather than turning every remembered value into a guess on upgrade.
func defMatches(stored string, d *FactDef) bool {
	return stored == "" || stored == defKey(d)
}

// save persists state when it changed. force writes even if unchanged.
func (e *Engine) save(force bool) {
	if e.deps.StatePath == "" {
		return
	}
	st := stateFile{
		Version: 1,
		BootID:  e.deps.BootID,
		Facts:   map[string]factState{},
		Rules:   map[string]ruleState{},
		Outputs: map[string]json.RawMessage{},
		TempSeq: e.tempSeq,
	}
	for name, f := range e.facts {
		if f.Has && (!f.Provisional || f.Def.Kind == config.SrcState) {
			st.Facts[name] = factState{Value: f.Value, Changed: f.Changed, Def: defKey(f.Def)}
		}
	}
	for _, r := range e.rules {
		st.Rules[r.Def.Name] = ruleState{Matched: r.Matched, Since: r.Since, FalseSince: r.FalseSince}
	}
	for _, o := range e.outputs {
		if raw := o.Save(); len(raw) > 0 {
			st.Outputs[o.Name()] = raw
		}
	}
	for _, r := range e.temps {
		if r.Temp.Spec.Attach || r.Temp.gone {
			continue
		}
		st.Temps = append(st.Temps, tempState{ID: r.Temp.ID, Spec: r.Temp.Spec, Created: r.Temp.Created, Matched: r.Matched})
	}
	sort.Slice(st.Temps, func(i, j int) bool { return st.Temps[i].ID < st.Temps[j].ID })
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		slog.Error("cannot encode state", "error", err)
		return
	}
	if !force && bytes.Equal(b, e.lastSaved) {
		return
	}
	if err := writeAtomic(e.deps.StatePath, b); err != nil {
		slog.Error("cannot save state", "path", e.deps.StatePath, "error", err)
		return
	}
	e.lastSaved = b
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Reload replaces the config. It must run on the engine goroutine. Fact values,
// rule match state and temporary rules carry over by name; temporary rules
// that no longer compile against the new facts are dropped.
func (e *Engine) Reload(comp *Compiled) {
	oldTemps, oldFacts, oldRules := e.temps, e.facts, e.rules
	e.stopSources()
	e.install(comp, oldFacts, oldRules)
	e.temps = nil
	for _, r := range oldTemps {
		if r.Temp.gone {
			if r.Temp.unwatch != nil {
				r.Temp.unwatch()
			}
			continue
		}
		nr, err := e.compileTemp(r.Temp.ID, r.Temp.Spec)
		if err != nil {
			// Fail closed: keep the rule as it was. Its condition now
			// refers to facts that do not exist, so it cannot be
			// evaluated and keeps its current state until released.
			slog.Warn("temporary rule does not compile against the new config; keeping its current state", "rule", r.Def.Name, "error", err)
			r.Err = err.Error()
			e.temps = append(e.temps, r)
			continue
		}
		nr.Temp = r.Temp
		nr.Matched, nr.Since = r.Matched, r.Since
		e.temps = append(e.temps, nr)
	}
	// Rules that matched and are gone from the new config get their exit
	// actions, as if they had stopped matching.
	kept := map[string]bool{}
	for _, r := range e.rules {
		kept[r.Def.Name] = true
	}
	t := now()
	for _, r := range oldRules {
		if r.Matched && !kept[r.Def.Name] {
			slog.Info("rule removed by reload while it matched; running its exit actions", "rule", r.Def.Name)
			e.runActions(r, "exit", r.Def.Exit, t)
		}
	}
	e.startSources()
	e.waiting = map[string]bool{}
	e.settled = true
	e.dirty = true
	e.configError = ""
	slog.Info("config reloaded", "facts", len(comp.Facts), "rules", len(comp.Rules))
}

func (e *Engine) outputIndex() (map[string]plugin.Output, []string) {
	byName := map[string]plugin.Output{}
	var names []string
	for _, o := range e.outputs {
		byName[o.Name()] = o
		names = append(names, o.Name())
	}
	return byName, names
}

// Outputs returns the engine's outputs, for compiling configs against them.
// It may be called from any goroutine.
func (e *Engine) Outputs() []plugin.Output { return e.outputs }

// SetConfigError records a failed reload so status can show it. It must run
// on the engine goroutine.
func (e *Engine) SetConfigError(err error) {
	if err == nil {
		e.configError = ""
	} else {
		e.configError = err.Error()
	}
}

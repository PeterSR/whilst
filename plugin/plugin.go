// Package plugin defines how an application connects the engine to the
// world: sources that read facts, outputs that rules keep true while they
// match, a runner for enter and exit actions, and lifetimes that tie
// temporary rules to whoever asked for them.
//
// The engine knows nothing about where values come from or how outputs are
// applied. The application provides these, together with the fact and action
// kinds it registers in a config.Registry.
//
// # Threading
//
// The engine owns its state on one goroutine, the one that runs
// engine.Engine.Run (the engine goroutine). Each method below says which
// goroutine calls it. A method called on the engine goroutine must not block
// for long, since nothing else is evaluated meanwhile, and must not call
// engine.Engine.Do, which would deadlock. Source.Start runs on a goroutine
// of its own. Output.Parse may run on another goroutine at the same time as
// the other Output methods (a config is compiled off the engine goroutine
// before a reload), so it should only validate its input.
package plugin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PeterSR/whilst/config"
)

// Update carries a new raw value for a fact, or the error from reading it.
type Update struct {
	Fact  string
	Value any
	Err   error
	// Seq orders updates from one source: the engine drops an update with a
	// lower Seq than one it already applied. Zero means unordered.
	Seq int64
}

// Source produces updates for one fact.
type Source interface {
	// Start begins watching. The engine calls it on a goroutine of its own
	// for each source, so it may block briefly (on a call that subscribes,
	// say), but it should return as soon as watching is set up: the first
	// read happens in the background and is reported through emit. Values
	// should already be in the normal form of package value (the engine
	// normalises them again, but knows only plain Go types).
	//
	// emit may be called from any goroutine, at any time, also after ctx
	// is done: updates from a source that was replaced, or that arrive
	// after the engine stopped, are dropped. emit can block while the
	// engine's queue is full, so a source must not call it from Refresh
	// or Describe (which run on the engine goroutine); call it from a
	// goroutine of the source's own.
	Start(ctx context.Context, emit func(Update)) error
	// Refresh re-reads the value in the background, if the source can. It
	// is called on the engine goroutine, so it must not block.
	Refresh()
	// Describe is a short human description, shown in status. It is
	// called on the engine goroutine.
	Describe() string
}

// permanent marks a Start error that retrying cannot fix.
type permanent struct {
	err    error
	reason string
}

func (p *permanent) Error() string { return p.err.Error() }
func (p *permanent) Unwrap() error { return p.err }

// Permanent wraps an error from Source.Start that retrying will not fix. The
// engine then logs reason and stops retrying that source.
func Permanent(err error, reason string) error {
	if err == nil {
		return nil
	}
	return &permanent{err: err, reason: reason}
}

// IsPermanent reports whether err was wrapped with Permanent, and why.
func IsPermanent(err error) (reason string, ok bool) {
	var p *permanent
	if errors.As(err, &p) {
		return p.reason, true
	}
	return "", false
}

// Want is one matching rule's claim on an output.
type Want struct {
	Rule  string
	Claim any // as returned by Parse
}

// Item is one thing an output manages, for status.
type Item struct {
	Key      string   `json:"key"`
	Held     bool     `json:"held"`
	WantedBy []string `json:"wanted_by,omitempty"`
	Note     string   `json:"note,omitempty"`
	Err      string   `json:"error,omitempty"`
}

// Output is one output type, named by the rule key that configures it.
type Output interface {
	// Name is the rule key, such as "lock". It must be unique among the
	// outputs and must not be a key that rules use themselves (name,
	// while, hold, priority, linger, enter, exit); engine.Compile rejects
	// such outputs. It may be called from any goroutine.
	Name() string
	// Parse validates a rule's value for this output. It is called on the
	// goroutine that runs engine.Compile, on the goroutine that calls
	// Engine.Restore when temporary rules are restored, and on the engine
	// goroutine when one is added or recompiled, possibly while other
	// methods run: it should only
	// validate and build the claim, not touch the output's state.
	Parse(raw any) (any, error)
	// Reconcile makes the held state match the union of wants. It is called
	// on the engine goroutine after every evaluation, so it must be cheap
	// when nothing changed and must not block for long. An error means some
	// part could not be applied; it is retried later.
	Reconcile(wants []Want) error
	// Status lists the managed items. It is called on the engine goroutine.
	Status() []Item
	// Save returns state to persist across restarts; it is called on the
	// engine goroutine. Load restores it; it is called by
	// engine.Engine.Restore, before Run, and only when the state was saved
	// in the same boot (see engine.Deps.BootID).
	Save() json.RawMessage
	Load(json.RawMessage) error
	// Close releases what can be released on shutdown. It is called on the
	// engine goroutine as Run returns.
	Close()
}

// Runner runs the enter and exit actions of rules (all but set actions,
// which the engine applies itself). Run is called on the engine goroutine
// and must not block it.
type Runner interface {
	// Run queues one rule's enter or exit actions. Actions run in the order
	// they are listed, and batches run in the order they were queued, so a
	// rule's exit actions finish before the next rule's enter actions start.
	Run(rule, event string, actions []config.Action)
}

// Lifetimes ties temporary rules to the life of whoever asked for them, such
// as a client process that wants something held only while it runs. A holder
// is an id the application understands and passes to engine.Engine.Ensure
// as the sender, such as a connection or process id. Watch and Alive are
// called on the engine goroutine.
type Lifetimes interface {
	// Watch calls gone, from any goroutine, once holder has gone away. The
	// returned function stops watching.
	Watch(holder string, gone func()) (stop func(), err error)
	// Alive reports whether holder is still there.
	Alive(holder string) bool
}

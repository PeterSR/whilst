# whilst

A rule engine that keeps a system in the state its rules ask for, for as long as their conditions hold.

You give it facts (named values that change over time) and rules written in [CEL](https://cel.dev). While a rule's condition holds, its outputs are held; when it starts or stops holding, its enter and exit actions run. The engine reconciles rather than reacting to transitions: after every change it works out what should be held right now and makes it so, so a missed event, a crash or a restart is corrected on the next evaluation.

whilst has no platform of its own. An application registers where facts come from, what outputs do and which actions exist. [systemd-whilst](https://github.com/PeterSR/systemd-whilst) is the application for Linux desktops: facts from D-Bus and systemd, inhibitor locks and user units as outputs, desktop notifications as actions. Its README is the full reference for the rule language from a user's point of view.

```sh
go get github.com/PeterSR/whilst
```

## What the engine provides

- **Facts** with types, transforms, debounce and an `initial` value, plus two kinds built in: `expr` (a CEL expression over other facts) and `state` (a value only rules change, through `set` actions).
- **Rules** with a CEL `while` condition, optional `hold` (hysteresis: once on, stay on while this is true), `linger` (stay on for a while after the condition ends), `priority`, `enter` and `exit` actions, and any number of outputs.
- **Time**: `since('fact')` in conditions, and exact wake-ups for comparisons like `since('ac') < duration('30m')`, so nothing polls. Other clock use (`hour >= 21`) is re-checked on a tick.
- **Guesses**: values remembered from before a reboot, and initial values, are provisional. Replacing a guess is not a change, and rules do not act on a guess.
- **Persistence**: fact values and change times, rule state, output state and temporary rules survive a restart, and the engine knows when the machine itself restarted.
- **Temporary rules** added at run time (for a duration, until a time, while a condition holds, or tied to the life of a caller), and status snapshots that explain why each rule and output is in its state.

[CONFIG.md](CONFIG.md) is the reference for everything the library defines in a config: `include`, `tick`, `settle`, the generic fact keys, `expr` and `state` facts, rule keys, the `set` action, the CEL extras, reserved names and how unnamed rules are named.

## Plugging in an application

```go
reg := config.NewRegistry()
reg.AddFactKind(config.FactKind{
	Name:       "file",            // [fact.x] file = "PATH"
	Rereadable: true,              // reports on its own soon after start
	Parse: func(v any, t map[string]any) (config.FactSpec, error) {
		p, ok := v.(string)
		if !ok || p == "" {
			return nil, fmt.Errorf("file must be a path")
		}
		return fileSpec{Path: p}, nil // FactSpec has SourceKey() string
	},
})
reg.AddActionKind(config.ActionKind{Name: "beep", Parse: parseBeep})
reg.Presets = myPresets // optional fs.FS: include = ["preset:NAME"]
reg.Messages = config.Messages{ActionExample: `{ beep = "..." }`} // optional wording

cfg, err := config.Load(path, reg)
if err != nil {
	return err
}
comp, err := engine.Compile(cfg, outputs) // outputs: []plugin.Output
if err != nil {
	return err
}

e := engine.New(comp, engine.Deps{
	NewSource: newSource,   // *config.Fact -> (plugin.Source, error)
	Outputs:   outputs,
	Actions:   runner,      // plugin.Runner: runs enter/exit actions in order
	Lifetimes: lifetimes,   // optional: ends temporary rules when their caller leaves
	StatePath: statePath,   // optional: where state is persisted
	BootID:    bootID,      // optional: changes when the machine restarts
})
if err := e.Restore(); err != nil {
	log.Printf("cannot restore state; starting fresh: %v", err)
}
return e.Run(ctx) // returns when ctx is cancelled
```

The interfaces an application implements are in package `plugin`:

| Interface | Job |
|---|---|
| `Source` | Reads one fact: `Start(ctx, emit)`, `Refresh()`, `Describe()`. |
| `Output` | Holds something while rules want it: parses a rule's value, reconciles to the union of all matching rules' wants, reports status, persists its own state. |
| `Runner` | Runs a rule's enter or exit actions, in order. `set` actions are handled by the engine. |
| `Lifetimes` | Tells the engine when the holder of a temporary rule has gone. |

[example_test.go](example_test.go) is a complete small application, run as a test: a fact kind whose values the application pushes in over a channel, an output that prints what it holds, and a status read while the engine runs.

`BootID` matters for persistence: rule state and output state are restored only when the saved boot id equals it, so with an empty `BootID` they are never restored, and rules start fresh (and run their enter actions again) after every restart.

## Talking to a running engine

The engine's state belongs to one goroutine, the one running `Run`. Anything else, such as a request handler, a signal handler or a file watcher, reaches it through `Do`, which runs a function on that goroutine and waits for it:

```go
// Read the state, for a status command.
var st engine.Status
if !e.Do(func() { st = e.Snapshot() }) {
	return errors.New("stopping")
}

// Add a temporary rule and wait until its outputs are applied.
var id string
var problems []string
var err error
e.Do(func() {
	id, problems, _, err = e.EnsureNow(engine.TempSpec{
		Name:    "build",
		Until:   time.Now().Add(2 * time.Hour),
		Outputs: map[string]any{"lock": "sleep"},
	}, "") // a sender is needed only with Attach (see plugin.Lifetimes)
})
if err != nil {
	return err
}
// problems lists outputs that could not be applied yet; they are retried.

// Drop it again.
e.Do(func() { e.Release(id) })

// Reload: load and compile anywhere, then apply on the engine goroutine.
cfg, err := config.Load(path, reg)
if err == nil {
	var comp *engine.Compiled
	if comp, err = engine.Compile(cfg, e.Outputs()); err == nil {
		e.Do(func() { e.Reload(comp) })
	}
}
if err != nil {
	e.Do(func() { e.SetConfigError(err) }) // shown in status
}
```

- **Through `Do` only**: `Ensure`, `EnsureNow`, `Release`, `Snapshot`, `Reload`, `SetConfigError`, `RefreshAll`, `Apply` and `Evaluate` must run on the engine goroutine.
- **From any goroutine**: `Do`, `Wake` (evaluate soon) and `Resumed` (the machine woke from sleep; every source re-reads).
- **Before `Run`**: `Restore`. `Do` called before `Run` starts waits for it.
- **After `Run` returns**, `Do` runs nothing and returns `false` at once, so a late request cannot hang; `Wake`, `Resumed` and the `emit` functions of sources drop what they get.
- Never call `Do` from the engine goroutine itself: not inside a `Do` function, and not from a plugin method the engine calls (`Output.Reconcile`, `Runner.Run`, `Source.Refresh` and so on), or it deadlocks.

Package `engine`'s documentation lists which goroutine calls each plugin method: `Source.Start` on its own goroutine, `Output.Parse` on the goroutine that runs `Compile` (and, for temporary rules, in `Restore` or on the engine goroutine), `Output.Load` in `Restore`, the other `Output` methods, `Runner.Run`, `Source.Refresh` and `Lifetimes.Watch` on the engine goroutine, and `emit` and the `Lifetimes` gone callback from any goroutine.

Unnamed rules are named after a hash of their content, so their state follows them when the file is reordered. Registering a new action kind never renames rules that do not use it.

## Design

The engine keeps no platform dependencies: no D-Bus, no `golang.org/x/sys`, no processes or signals. A test (`internal/boundary`) fails if any other package in the module, tests included, imports one. Its only dependencies are [cel-go](https://github.com/google/cel-go) and [BurntSushi/toml](https://github.com/BurntSushi/toml).

## License

MIT

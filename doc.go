// Package whilst is a rule engine that keeps a system in the state its
// rules ask for, for as long as their conditions hold. It has no platform of
// its own: applications add fact kinds, action kinds, outputs and presets.
//
//   - value: the normal form of fact values
//   - expr: CEL environments and programs, and time thresholds
//   - config: loading and checking configs, and the registry through which
//     an application adds fact kinds, action kinds and presets
//   - plugin: the interfaces an application implements (sources, outputs,
//     an action runner, lifetimes)
//   - engine: the reconcile loop, temporary rules, status and the state file
//
// The engine's state belongs to the goroutine that runs engine.Engine.Run;
// other goroutines reach it through engine.Engine.Do. The package
// documentation of engine says which methods need that and which goroutine
// calls each plugin method.
//
// CONFIG.md in the repository is the reference for the config language the
// library defines.
//
// The module must stay free of any platform; internal/boundary enforces it.
// systemd-whilst (github.com/PeterSR/systemd-whilst) is the application for
// Linux desktops with systemd.
package whilst

# Config reference

This is everything the library itself defines in a config. An application adds fact kinds (a key that names where a fact's value comes from), action kinds and outputs (rule keys), and documents those itself; [systemd-whilst](https://github.com/PeterSR/systemd-whilst) is an example.

Configs are [TOML](https://toml.io). Every key is checked: an unknown key anywhere is an error, so a typo is caught at load rather than silently ignored.

## Top level

```toml
include = ["preset:power", "more.toml"]  # optional
tick    = "30s"                          # optional, main file only
settle  = "3s"                           # optional, main file only

[fact.NAME]
# ...

[[rule]]
# ...
```

- `include`: a string or a list of strings. A path is read relative to the including file, and a leading `~` is the home directory. `preset:NAME` reads `NAME.toml` from the presets the application registered. Included files can include others (up to 8 levels deep), but no file may be included twice, and they may not set `tick` or `settle`.
- `tick`: how often expressions that use the clock in a way the engine cannot schedule exactly (see [Time](#time)) are re-evaluated. Default `"30s"`.
- `settle`: how long the engine waits at start for every fact source to report before the first evaluation. Default `"3s"`.

Durations are Go duration strings and always need a unit: `"500ms"`, `"30s"`, `"5m"`, `"1h30m"`.

## Facts

A fact is a named value that the engine keeps current, together with the time it last changed. Each `[fact.NAME]` table names exactly one source: a kind the application registered, or one of the two built-in kinds below.

Fact names are identifiers (letters, digits and `_`, not starting with a digit) and cannot be a [reserved name](#reserved-names).

Keys every fact accepts:

| Key | Meaning |
|---|---|
| `type` | `bool`, `int`, `uint`, `double`, `string`, `list`, `map` or `dyn` (the default: any value). Values are converted where that is lossless or obvious (`"1"`, `"yes"` or `1` to `true`; `"42"` to `42`), and expressions are type-checked against it. A value that cannot be converted is an error on that fact. |
| `initial` | The value before the first reading. For a fact whose source reads on its own soon after start (an `expr` fact, or a registered kind marked rereadable), it is only a guess: rules that read the fact wait for a real reading before acting. For other facts (a `state` fact, or a source that may never report, such as a signal) it is taken as the real value. |
| `debounce` | A duration: a new value must hold this long before it counts, so shorter blips are ignored. |
| `transform` | Only for registered kinds. A CEL expression applied to the raw value, which it sees as `value`, such as `"!value"` or `"value / 1000"`. The result is then converted to `type`. |

A registered kind may accept more keys of its own.

### expr facts

```toml
[fact.home]
expr = "ssid == 'home-net'"
type = "bool"        # optional
```

An `expr` fact is a CEL expression over other facts (the same language as rule conditions, see [Expressions](#expressions)). It is re-evaluated whenever its inputs change. `expr` facts may read each other, as long as they do not form a cycle. A value computed from a guess is itself a guess. `debounce` on an `expr` fact is often the best place for it, since it debounces the meaning rather than the raw reading.

### state facts

```toml
[fact.mode]
state = ["normal", "away"]   # allowed values; the first is the default

[fact.count]
state   = true               # any value
type    = "int"
initial = 0                  # required with state = true
```

A `state` fact is never read: only `set` actions in rules change it (see [Actions](#actions)). With a list, the value must be one of the listed strings, `type` defaults to `string`, and `initial` defaults to the first entry. With `state = true` any value of the fact's `type` is allowed, and `initial` is required. `state = false` is an error.

A state fact's value survives restarts and reboots (as long as it is still allowed), and a reload keeps it if it is still allowed by the new definition.

## Rules

```toml
[[rule]]
name     = "focus"            # optional
while    = "mode == 'focus'"  # required
hold     = "busy"             # optional
linger   = "1m"               # optional
priority = 10                 # optional
enter    = [{ set = { mode = "away" } }]   # optional
exit     = [{ set = { mode = "normal" } }] # optional
# any other key is an output the application registered
```

| Key | Meaning |
|---|---|
| `name` | A non-empty string, unique among rules. Rule state is kept by name. Names starting with `ensure:` are reserved for temporary rules. Without a name, the rule is [named after its content](#unnamed-rules). |
| `while` | Required. A CEL condition (it must be a `bool`). While it is true the rule matches: its outputs are held. |
| `hold` | A CEL condition that keeps a matching rule matching while it is true, even when `while` is not. The rule starts only on `while`. This gives hysteresis: `while = "battery < 20"` with `hold = "battery < 30"`. |
| `linger` | A positive duration: the rule keeps matching this long after its condition (`while`, or `hold` if it has one) turns false. If the condition turns true again within that time, the rule simply carries on, with no exit or enter actions. |
| `priority` | An integer. Rules are evaluated and listed highest first, ties in file order. Temporary rules come before every config rule. |
| `enter`, `exit` | Actions to run once when the rule starts or stops matching: one action table, or a list of them. |

Every other key is an output. The application registers outputs by name and parses their values; a key that is neither one of the above nor an output is an error that lists the outputs there are.

When several rules match, each output gets the claims of all of them and holds their union.

If a condition cannot be evaluated (a fact it reads has no value yet, say), the rule keeps its current state and status shows the error. A rule whose condition reads a fact whose value is still a guess also keeps its state until the fact has really been read.

### Actions

An action is a table with exactly one key naming its kind, plus that kind's own keys. The library defines one kind, `set`; the application registers the others.

```toml
enter = [{ set = { mode = "away", count = 1 } }]
```

`set` writes state facts: each key must be a `state` fact, and each value must be allowed for it (it is converted to the fact's type first). Set actions are applied immediately, before the rule's other actions are handed to the application, so the next evaluation already sees the new state. A change made by a set action can start or stop other rules in the same evaluation; this repeats up to 8 times, so rules that keep flipping each other cannot hang the engine.

The other actions of a rule run in the order listed, and one rule's exit actions are queued before the next rule's enter actions.

### Unnamed rules

A rule without a `name` is named `rule-` followed by the first 8 hex digits of a SHA-256 hash of its content: `while`, `hold`, its outputs and its `enter` and `exit` actions (not `priority` or `linger`). Two rules with the same content get `-2`, `-3` and so on after the first. Because the name follows the content rather than the position, reordering rules keeps each rule's state, but editing a rule's content makes it a new rule (the old one stops matching and runs its exit actions). Give rules with actions a name.

## Expressions

Conditions, `hold`, `expr` facts and transforms are [CEL](https://github.com/google/cel-spec/blob/master/doc/langdef.md). Facts are variables, typed by their `type`. A fact without a value is absent, which makes an expression that reads it fail to evaluate (and a rule keep its state) rather than read a default.

On top of standard CEL (which includes `duration('30m')` and `timestamp('...')`):

| Name | Meaning |
|---|---|
| `since('fact')` | How long ago the fact last changed, as a duration. The argument must be a literal fact name. A fact never seen to change (since it was first read) counts as having changed about a hundred years ago, so every "within the last X" test is false. |
| `now` | The current time, as a timestamp. |
| `hour`, `minute` | The local time of day, as integers. |
| `weekday` | The local day of the week: 1 for Monday through 7 for Sunday. |

Transforms see only `value`, the raw value from the source; they cannot read facts or the clock.

### Time

A comparison of `since('fact')` with a literal duration, such as `since('ac') < duration('30m')` (with `<`, `<=`, `>` or `>=`, either way round), is scheduled exactly: the engine works out when it will flip and wakes up then, so nothing polls. Any other use of the clock (`now`, `hour`, `minute`, `weekday`, or `since()` used some other way) is re-evaluated every `tick`, and compiling the config gives a warning that says so.

### Reserved names

These cannot be used as fact names: `now`, `hour`, `minute`, `weekday`, `value`, `since`, `duration`, `timestamp`, and CEL's keywords and literals: `true`, `false`, `null`, `in`, `as`, `break`, `const`, `continue`, `else`, `for`, `function`, `if`, `import`, `let`, `loop`, `package`, `namespace`, `return`, `var`, `void`, `while`.

Rule keys (`name`, `while`, `hold`, `priority`, `linger`, `enter`, `exit`) cannot be output names.

## Persistence

With a state path set, the engine remembers fact values and when they last changed (so `since()` survives a restart), whether each rule matched, what each output saved, and temporary rules that are not tied to a caller. After a reboot (a different boot id), remembered values of facts that are read are only guesses until read again, those of other facts (except state facts) are dropped in favour of `initial`, and rule and output state starts fresh, so enter actions run again. An application that gives no boot id never restores rule or output state.

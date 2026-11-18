package whilst_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/engine"
	"github.com/PeterSR/whilst/plugin"
)

// inputSpec is the parsed form of an `input = "NAME"` fact: a value the
// application pushes in over a channel.
type inputSpec struct{ Name string }

func (s inputSpec) SourceKey() string { return "input:" + s.Name }

// inputSource reports its first value at once, then every value sent on ch.
type inputSource struct {
	fact  string
	first string
	ch    <-chan string
}

func (s *inputSource) Start(ctx context.Context, emit func(plugin.Update)) error {
	go func() {
		emit(plugin.Update{Fact: s.fact, Value: s.first})
		for {
			select {
			case <-ctx.Done():
				return
			case v := <-s.ch:
				emit(plugin.Update{Fact: s.fact, Value: v})
			}
		}
	}()
	return nil
}

func (s *inputSource) Refresh()         {} // nothing to re-read
func (s *inputSource) Describe() string { return "input" }

// logOutput "holds" a set of names by printing when it changes, and tells
// changed about each change.
type logOutput struct {
	held    map[string]bool
	changed chan struct{}
}

func (*logOutput) Name() string { return "log" }

func (*logOutput) Parse(raw any) (any, error) {
	s, ok := raw.(string)
	if !ok || s == "" {
		return nil, fmt.Errorf("log must be a name")
	}
	return s, nil
}

func (o *logOutput) Reconcile(wants []plugin.Want) error {
	want := map[string]bool{}
	for _, w := range wants {
		want[w.Claim.(string)] = true
	}
	var lines []string
	for n := range want {
		if !o.held[n] {
			lines = append(lines, "on: "+n)
		}
	}
	for n := range o.held {
		if !want[n] {
			lines = append(lines, "off: "+n)
		}
	}
	o.held = want
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
	if len(lines) > 0 {
		o.changed <- struct{}{}
	}
	return nil
}

func (o *logOutput) Status() []plugin.Item {
	var items []plugin.Item
	for n := range o.held {
		items = append(items, plugin.Item{Key: n, Held: true})
	}
	return items
}

func (*logOutput) Save() json.RawMessage      { return nil }
func (*logOutput) Load(json.RawMessage) error { return nil }
func (*logOutput) Close()                     {}

const exampleConfig = `
[fact.mode]
input = "mode"

[[rule]]
name  = "focus"
while = "mode == 'focus'"
log   = "do not disturb"
`

// An application registers its fact kinds and outputs, then runs the
// engine and talks to it while it runs.
func Example() {
	reg := config.NewRegistry()
	reg.AddFactKind(config.FactKind{
		Name:       "input",
		Rereadable: true, // reports on its own as soon as it starts
		Parse: func(v any, _ map[string]any) (config.FactSpec, error) {
			n, ok := v.(string)
			if !ok || n == "" {
				return nil, fmt.Errorf("input must be a name")
			}
			return inputSpec{Name: n}, nil
		},
	})
	out := &logOutput{changed: make(chan struct{}, 8)}
	outputs := []plugin.Output{out}

	cfg, err := config.Parse(exampleConfig, reg)
	if err != nil {
		panic(err)
	}
	comp, err := engine.Compile(cfg, outputs)
	if err != nil {
		panic(err)
	}
	mode := make(chan string)
	e := engine.New(comp, engine.Deps{
		NewSource: func(f *config.Fact) (plugin.Source, error) {
			if _, ok := f.Spec.(inputSpec); !ok {
				return nil, nil // expr and state facts have no source
			}
			return &inputSource{fact: f.Name, first: "normal", ch: mode}, nil
		},
		Outputs: outputs,
	})
	if err := e.Restore(); err != nil { // no StatePath: nothing to restore
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error)
	go func() { stopped <- e.Run(ctx) }()

	mode <- "focus"
	<-out.changed

	// Engine state belongs to the engine goroutine: read it through Do.
	var st engine.Status
	e.Do(func() { st = e.Snapshot() })
	fmt.Println("rule", st.Rules[0].Name, "matched:", st.Rules[0].Matched)

	mode <- "normal"
	<-out.changed

	cancel()
	if err := <-stopped; err != nil {
		panic(err)
	}
	// Output:
	// on: do not disturb
	// rule focus matched: true
	// off: do not disturb
}

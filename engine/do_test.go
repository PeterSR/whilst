package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PeterSR/whilst/plugin"
)

// runEngine runs h.e in the background and returns a function that stops it
// and waits for Run to return.
func runEngine(t *testing.T, h *harness) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := h.e.Run(ctx); err != nil {
			t.Error(err)
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

func TestDoRunsOnEngine(t *testing.T) {
	h := newHarness(t, "[fact.x]\ncommand = \"true\"\n", "")
	stop := runEngine(t, h)
	defer stop()
	var got string
	if !h.e.Do(func() { got = h.e.Snapshot().Facts[0].Name }) {
		t.Fatal("Do reported that fn did not run")
	}
	if got != "x" {
		t.Fatalf("snapshot fact = %q", got)
	}
}

func TestDoAfterStop(t *testing.T) {
	h := newHarness(t, "[fact.x]\ncommand = \"true\"\n", "")
	stop := runEngine(t, h)
	stop()

	ran := make(chan bool, 1)
	go func() {
		ok := h.e.Do(func() { t.Error("fn ran after Run returned") })
		ran <- ok
	}()
	select {
	case ok := <-ran:
		if ok {
			t.Fatal("Do returned true after Run returned")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do blocked after Run returned")
	}
}

// After Run returns, nothing drains the queue: posting more than it holds
// must not block a source or a lifetimes callback.
func TestPostAfterStopDoesNotBlock(t *testing.T) {
	h := newHarness(t, "[fact.x]\ncommand = \"true\"\n", "")
	stop := runEngine(t, h)
	stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 3 * cap(h.e.ch) {
			h.e.Wake()
			h.e.post(func() { h.e.onUpdate(plugin.Update{Fact: "x", Value: "on"}) })
		}
		h.e.Resumed()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("post blocked after Run returned")
	}
}

// Do calls racing with shutdown all return, and report truthfully whether
// their fn ran.
func TestDoRacingStop(t *testing.T) {
	for range 20 {
		h := newHarness(t, "[fact.x]\ncommand = \"true\"\n", "")
		stop := runEngine(t, h)
		var wg sync.WaitGroup
		var ran, reported atomic.Int64
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if h.e.Do(func() { ran.Add(1) }) {
					reported.Add(1)
				}
			}()
		}
		stop()
		waited := make(chan struct{})
		go func() { wg.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Fatal("Do blocked while Run returned")
		}
		if ran.Load() != reported.Load() {
			t.Fatalf("fn ran %d times, Do reported %d", ran.Load(), reported.Load())
		}
	}
}

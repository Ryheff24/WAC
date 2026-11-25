package lab

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// result is one call's outcome.
type result struct {
	code            int
	output, refused string
}

// start runs request through the broker in the background, under ctx, and returns its outcome's channel.
func (f *fixture) start(ctx context.Context, request Request) <-chan result {
	done := make(chan result, 1)
	go func() {
		var out strings.Builder
		var mu sync.Mutex
		refused := ""
		code := f.broker.Handle(ctx, Self{}, request, func(frame Frame) error {
			mu.Lock()
			defer mu.Unlock()
			out.WriteString(frame.Data)
			if frame.Refused != "" {
				refused = frame.Refused
			}
			return nil
		})
		mu.Lock()
		defer mu.Unlock()
		done <- result{code, out.String(), refused}
	}()
	return done
}

// holders is how many calls hold the VM now.
func (f *fixture) holders() int {
	f.broker.mu.Lock()
	defer f.broker.mu.Unlock()
	return len(f.broker.lockFor("proj-lab").holders)
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestCallsShareTheVM: a call runs, status answers and push works while a long run holds the VM (the
// owner's request, 2026-11-21: a second call waited two minutes and was refused as busy).
func TestCallsShareTheVM(t *testing.T) {
	f := newFixture(t)
	f.broker.LockWait = 50 * time.Millisecond
	long, stopLong := context.WithCancel(context.Background())
	defer stopLong()
	sleeping := f.start(long, Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}})
	waitFor(t, "the long run to hold the VM", func() bool { return f.holders() == 1 })
	if code, out, refused := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"tail", "log"}}); code != 0 || !strings.Contains(out, "ran tail log") {
		t.Errorf("a second run during a long one: %d %q %q", code, out, refused)
	}
	if code, _, refused := f.call(Request{Verb: VerbStatus, Project: "proj"}); code != 0 {
		t.Errorf("status during a long run: %q", refused)
	}
	stopLong()
	<-sleeping
	if f.holders() != 0 {
		t.Errorf("%d holders left", f.holders())
	}
}

// TestResetCancelsEveryHolder: a reset cancels every call sharing the VM, then holds it alone; a call
// that comes while it waits goes after it.
func TestResetCancelsEveryHolder(t *testing.T) {
	f := newFixture(t)
	f.broker.LockWait = 2 * time.Second
	first := f.start(context.Background(), Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}})
	second := f.start(context.Background(), Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}})
	waitFor(t, "two runs to hold the VM", func() bool { return f.holders() == 2 })
	if code, _, refused := f.call(Request{Verb: VerbReset, Project: "proj"}); code != 0 {
		t.Fatalf("reset during two runs: %q", refused)
	}
	for i, done := range []<-chan result{first, second} {
		select {
		case r := <-done:
			if r.code == 0 {
				t.Errorf("run %d was not stopped by the reset: %+v", i, r)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("run %d still holds the VM after the reset", i)
		}
	}
	if f.fake.ResetCount != 1 {
		t.Errorf("%d resets, want 1", f.fake.ResetCount)
	}
}

// TestOneStart: calls that all find the VM stopped start it once.
func TestOneStart(t *testing.T) {
	f := newFixture(t)
	f.fake.Update(func(fake *Fake) { fake.Info_.State = "Off" })
	f.fake.StartDelay = 50 * time.Millisecond
	var calls []<-chan result
	for i := 0; i < 4; i++ {
		calls = append(calls, f.start(context.Background(), Request{Verb: VerbRun, Project: "proj", Argv: []string{"hostname"}}))
	}
	for i, done := range calls {
		if r := <-done; r.code != 0 {
			t.Errorf("call %d: %+v", i, r)
		}
	}
	if f.fake.Started != 1 {
		t.Errorf("the VM was started %d times, want once", f.fake.Started)
	}
}

// TestFailedRecheckStopsEveryHolder: when the host check fails during runs (an adapter appears), the
// VM is turned off and every call on it ends, refused.
func TestFailedRecheckStopsEveryHolder(t *testing.T) {
	f := newFixture(t)
	f.broker.RecheckEvery = 20 * time.Millisecond
	first := f.start(context.Background(), Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}})
	second := f.start(context.Background(), Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}})
	waitFor(t, "two runs to hold the VM", func() bool { return f.holders() == 2 })
	f.fake.Update(func(fake *Fake) { fake.Info_.Adapters = []Adapter{{Switch: "Default Switch", SwitchType: "Internal"}} })
	for i, done := range []<-chan result{first, second} {
		select {
		case r := <-done:
			if r.code != 2 || r.refused == "" {
				t.Errorf("run %d after the adapter appeared: %+v", i, r)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("run %d went on with an adapter on the VM", i)
		}
	}
	if f.fake.Stopped == 0 {
		t.Error("the VM was not turned off")
	}
}

// TestSharedWaitsBehindAReset: a reset waiting for the VM is not starved by new shared calls, and a
// reset that gives up lets them go.
func TestSharedWaitsBehindAReset(t *testing.T) {
	f := newFixture(t)
	f.broker.LockWait = 100 * time.Millisecond
	held, cancelHeld := context.WithCancel(context.Background())
	defer cancelHeld()
	// A holder that ignores its cancellation, so the reset must wait.
	release, why := f.broker.acquire(held, "proj-lab", false, func() {})
	if why != "" {
		t.Fatal(why)
	}
	resetting := f.start(context.Background(), Request{Verb: VerbReset, Project: "proj"})
	waitFor(t, "the reset to wait", func() bool {
		f.broker.mu.Lock()
		defer f.broker.mu.Unlock()
		return f.broker.lockFor("proj-lab").waiting == 1
	})
	// A shared call that comes now waits behind the reset (here until its own deadline).
	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	if r := <-f.start(short, Request{Verb: VerbStatus, Project: "proj"}); !strings.Contains(r.refused, "waiting for VM") {
		t.Errorf("a shared call while a reset waits: %+v, want it to wait behind the reset", r)
	}
	if r := <-resetting; !strings.Contains(r.refused, "busy") {
		t.Errorf("the reset that could not get the VM: %+v", r)
	}
	if code, _, refused := f.call(Request{Verb: VerbStatus, Project: "proj"}); code != 0 {
		t.Errorf("a shared call once the reset gave up: %q", refused)
	}
	release()
}

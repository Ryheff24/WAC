package lab

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWatchTurnsOffARunningVMThatFailsItsHostCheck(t *testing.T) {
	f := newFixture(t)
	stopped := func() int {
		f.fake.mu.Lock()
		defer f.fake.mu.Unlock()
		return f.fake.Stopped
	}
	// Contained and running: left alone. proj-srv has no state: left to the gate.
	if got := f.broker.WatchOnce(context.Background()); len(got) != 0 || stopped() != 0 {
		t.Fatalf("a contained VM: turned off %q (%d stops)", got, stopped())
	}
	// Running with its disks' volume under the floor (a guest filling the host): turned off, recorded.
	f.fake.Info_.DisksFree = DiskFloor - 1
	if got := f.broker.WatchOnce(context.Background()); len(got) != 1 || got[0] != "proj-lab" || stopped() != 1 {
		t.Fatalf("an uncontained running VM: turned off %q (%d stops)", got, stopped())
	}
	if log := f.log.String(); !strings.Contains(log, `"event":"watch"`) || !strings.Contains(log, "running between calls") || !strings.Contains(log, "GiB free") {
		t.Errorf("the watcher did not record it: %s", log)
	}
	// Off: nothing to turn off.
	if got := f.broker.WatchOnce(context.Background()); len(got) != 0 || stopped() != 1 {
		t.Errorf("a VM already off: turned off %q (%d stops)", got, stopped())
	}
	// A VM that cannot be read is left to the gate.
	f.fake.mu.Lock()
	f.fake.Info_.State = "Running"
	f.fake.InfoFails = 1
	f.fake.mu.Unlock()
	if got := f.broker.WatchOnce(context.Background()); len(got) != 0 {
		t.Errorf("an unreadable VM: turned off %q", got)
	}
	// Watch repeats it every RecheckEvery until its context ends.
	f.broker.RecheckEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.broker.Watch(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for stopped() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if stopped() < 2 {
		t.Errorf("Watch did not turn the VM off again (%d stops)", stopped())
	}
}

func TestWatchCancelsTheCallsOnTheVM(t *testing.T) {
	f := newFixture(t)
	held, cancelHeld := context.WithCancel(context.Background())
	defer cancelHeld()
	release, refused := f.broker.acquire(held, "proj-lab", false, cancelHeld)
	if refused != "" {
		t.Fatal(refused)
	}
	defer release()
	f.fake.Info_.DisksFree = 0
	if got := f.broker.WatchOnce(context.Background()); len(got) != 1 {
		t.Fatalf("turned off %q", got)
	}
	if held.Err() == nil {
		t.Error("a call holding the VM was not cancelled when the watcher turned it off")
	}
}

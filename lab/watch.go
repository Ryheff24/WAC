package lab

import (
	"context"
	"strings"
	"time"
)

// Watch runs WatchOnce every RecheckEvery (default 1 minute) until ctx ends. A run already rechecks
// its VM every minute, but a VM left running between calls was watched by nothing: on 2026-11-23 a
// guest's drive optimisation, started by itself between calls, wrote hundreds of GB to its disks, and
// the day before one filled the host's C:.
func (b *Broker) Watch(ctx context.Context) {
	ticker := time.NewTicker(b.duration(b.RecheckEvery, time.Minute))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.WatchOnce(ctx)
		}
	}
}

// WatchOnce turns off every registered lab VM that is running while its host check fails, ends the
// calls on it, and records it in the log. It returns the VMs it turned off. It holds each VM as a
// call does, so it waits for a reset (which swaps the disks, and turns the VM off and on itself) and
// is cancelled by one. A VM it cannot read or whose state does not load is left to the gate, which
// refuses it on the next call.
func (b *Broker) WatchOnce(ctx context.Context) []string {
	var stopped []string
	seen := map[string]bool{}
	for _, project := range b.Projects {
		for _, vm := range project.LabVMs {
			key := strings.ToLower(vm)
			if seen[key] {
				continue
			}
			seen[key] = true
			if why := b.watchVM(ctx, vm); why != "" {
				stopped = append(stopped, vm)
			}
		}
	}
	return stopped
}

// watchVM checks one VM and turns it off if it runs uncontained. It returns why it did, or "".
func (b *Broker) watchVM(ctx context.Context, vm string) string {
	state, err := LoadState(b.StateDir, vm)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, b.duration(b.GateTimeout, 15*time.Minute))
	defer cancel()
	release, refused := b.acquire(ctx, vm, false, cancel)
	if refused != "" {
		return ""
	}
	defer release()
	info, err := b.H.Info(ctx, vm)
	if err != nil || ctx.Err() != nil || !strings.EqualFold(info.State, "Running") {
		return ""
	}
	why := HostCheck(info, state)
	if why == "" {
		return ""
	}
	// Every call on the VM runs under the changed configuration; this cancels the watcher's own hold too.
	b.cancelHolders(vm)
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancelStop()
	result := "the VM was turned off"
	if err := b.H.Stop(stopCtx, vm); err != nil {
		result = "turning the VM off failed: " + err.Error()
	}
	_ = b.record(map[string]any{"event": "watch", "time": b.now().UTC().Format(time.RFC3339Nano), "vm": vm,
		"refused": "running between calls: " + why, "result": result})
	return why
}

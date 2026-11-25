package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ryheff24/WAC/registry"
)

// LabOut is the directory under a project's root that pull publishes into, and LabIn the one agents
// put files to push into (the only place on Windows the WSL sandbox lets them write). What is in
// either never runs on the host.
const (
	LabOut = "lab-out"
	LabIn  = "lab-in"
)

// Broker answers requests. It runs as SYSTEM, so it decides every path itself, and does host file
// I/O as the caller (transfer.go).
type Broker struct {
	H        Hypervisor
	Projects []registry.Project
	StateDir string
	// Log receives one JSON line before and one after each call. A call whose first line cannot be
	// written is refused: no call goes unrecorded (review #8).
	Log io.Writer
	// HostAddresses lists the host's own addresses for the probe; nil uses the machine's.
	HostAddresses func() []string
	// Now is time.Now unless a test sets it.
	Now func() time.Time
	// LockWait bounds the wait for a VM another call is using (default 2 minutes); RecheckEvery is how
	// often the host check repeats during a run (default 1 minute); GateTimeout bounds the gate,
	// starting a VM included (default 15 minutes). Tests shorten them.
	LockWait, RecheckEvery, GateTimeout time.Duration
	// MaxPush bounds what one push stages (default DefaultMaxPush), and a push must also leave
	// PushReserve free on the staging volume, which holds the VMs' disks too: a full volume pauses them.
	// Tests lower both.
	MaxPush, PushReserve int64

	mu    sync.Mutex
	locks map[string]*vmLock
}

// vmLock is one VM's calls. Any number share it (run, status, push, pull: each has its own
// PowerShell Direct session and proves the gate itself), or one reset holds it alone. A reset first
// cancels every holder, and new shared calls wait behind a reset that is waiting, so it is never
// starved. Starting a stopped VM (the gate) is one call at a time (start), so two first calls do not
// both start it.
type vmLock struct {
	shared  int
	alone   bool
	waiting int                        // resets waiting for the VM
	changed chan struct{}              // closed and replaced whenever a holder lets go
	holders map[int]context.CancelFunc // every call holding the VM, which a reset or a failed recheck cancels
	next    int
	start   chan struct{} // one token: the gate, which may start the VM
}

// Handle answers one request from caller, writing frames to emit. It returns the exit code it
// reported. If a frame cannot be delivered (the client went away), the call is cancelled.
func (b *Broker) Handle(ctx context.Context, caller Caller, request Request, emit func(Frame) error) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	deliver := func(frame Frame) error {
		err := emit(frame)
		if err != nil {
			cancel()
		}
		return err
	}
	started := b.now()
	if err := b.record(map[string]any{"event": "start", "time": started.UTC().Format(time.RFC3339Nano), "client_pid": caller.PID(),
		"verb": request.Verb, "project": request.Project, "vm": request.VM, "argv": request.Argv, "cwd": request.Cwd, "timeout_s": request.Timeout,
		"host_path": request.HostPath, "guest_path": request.Guest}); err != nil {
		_ = deliver(Frame{Refused: "the broker's log cannot be written, so nothing runs: " + err.Error()})
		return 2
	}
	code, refused := b.handle(ctx, cancel, caller, request, deliver)
	if refused != "" {
		code = 2
		_ = deliver(Frame{Refused: refused})
	} else {
		_ = deliver(Frame{Exit: &code})
	}
	_ = b.record(map[string]any{"event": "end", "time": b.now().UTC().Format(time.RFC3339Nano), "client_pid": caller.PID(),
		"seconds": b.now().Sub(started).Seconds(), "exit": code, "refused": refused})
	return code
}

func (b *Broker) handle(ctx context.Context, cancel context.CancelFunc, caller Caller, request Request, emit func(Frame) error) (int, string) {
	switch request.Verb {
	case VerbRun, VerbStatus, VerbPush, VerbPull, VerbReset:
	default:
		return 0, fmt.Sprintf("unknown verb %q (run, status, push, pull, reset)", request.Verb)
	}
	project, state, refused := b.resolve(request)
	if refused != "" {
		return 0, refused
	}
	// Calls share the VM; a reset holds it alone, and frees it from a stuck call by cancelling every
	// holder first (review 2026-11-21, main 41dada9).
	release, refused := b.acquire(ctx, state.VM, request.Verb == VerbReset, cancel)
	if refused != "" {
		return 0, refused
	}
	defer release()
	gateCtx, cancelGate := context.WithTimeout(ctx, b.duration(b.GateTimeout, 15*time.Minute))
	defer cancelGate()
	switch request.Verb {
	case VerbStatus:
		return b.status(gateCtx, state, emit)
	case VerbReset:
		// The host check before the reset: a reset starts the VM, and a misconfigured VM never starts.
		info, err := b.H.Info(gateCtx, state.VM)
		if err != nil {
			return 0, "cannot read VM " + state.VM + ": " + err.Error()
		}
		if why := HostCheck(info, state); why != "" {
			return 0, "REFUSED: " + why
		}
		if err := b.H.Reset(gateCtx, state.VM, state); err != nil {
			return 0, "reset of " + state.VM + " failed: " + err.Error()
		}
	}
	if why := b.gate(gateCtx, state); why != "" {
		return 0, "REFUSED: " + why
	}
	switch request.Verb {
	case VerbReset:
		_ = emit(Frame{Stream: "stdout", Data: state.VM + ": reset; host configuration contained, guest diagnostics passed\n"})
		return 0, ""
	case VerbRun:
		return b.run(ctx, request, state, emit)
	case VerbPush:
		return b.push(ctx, caller, request, project, state, emit)
	}
	return b.pull(ctx, caller, request, project, state, emit)
}

// status reports the gate without starting anything: a stopped VM gets the host check only.
func (b *Broker) status(ctx context.Context, state State, emit func(Frame) error) (int, string) {
	info, err := b.H.Info(ctx, state.VM)
	if err != nil {
		return 0, "cannot read VM " + state.VM + ": " + err.Error()
	}
	if why := HostCheck(info, state); why != "" {
		return 0, "REFUSED: " + why
	}
	if !strings.EqualFold(info.State, "Running") {
		_ = emit(Frame{Stream: "stdout", Data: fmt.Sprintf("%s: %s; host configuration contained (no network adapter, recorded disks only). A run starts it.\n", state.VM, info.State)})
		return 0, ""
	}
	if why := Probe(ctx, b.H, state, b.hostAddresses()); why != "" {
		return 0, "REFUSED: " + why
	}
	_ = emit(Frame{Stream: "stdout", Data: state.VM + ": running; host configuration contained (no network adapter, recorded disks only), guest diagnostics passed\n"})
	return 0, ""
}

// resolve finds the request's project, its VM and the VM's state.
func (b *Broker) resolve(request Request) (registry.Project, State, string) {
	var project registry.Project
	found := false
	for _, p := range b.Projects {
		if p.Name == request.Project {
			project, found = p, true
		}
	}
	if !found {
		return project, State{}, fmt.Sprintf("project %q is not registered", request.Project)
	}
	vm := request.VM
	if vm == "" {
		vm = project.LabVMs[0]
	}
	if !containsFold(project.LabVMs, vm) {
		return project, State{}, fmt.Sprintf("VM %q is not one of %s's lab VMs (%s)", vm, project.Name, strings.Join(project.LabVMs, ", "))
	}
	state, err := LoadState(b.StateDir, vm)
	if err != nil {
		return project, State{}, err.Error()
	}
	if state.Project != project.Name {
		return project, State{}, fmt.Sprintf("VM %s belongs to project %s, not %s", vm, state.Project, project.Name)
	}
	return project, state, ""
}

// run runs argv in the guest. The host check repeats every RecheckEvery; if it fails, or the client
// goes away, the run is cancelled.
func (b *Broker) run(ctx context.Context, request Request, state State, emit func(Frame) error) (int, string) {
	if len(request.Argv) == 0 || request.Argv[0] == "" {
		return 0, "run: no program given"
	}
	timeout := DefaultTimeout
	if request.Timeout > 0 {
		timeout = time.Duration(request.Timeout) * time.Second
	}
	if timeout > MaxTimeout {
		return 0, fmt.Sprintf("run: timeout over the maximum of %s", MaxTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var mu sync.Mutex
	recheck := ""
	go func() {
		ticker := time.NewTicker(b.duration(b.RecheckEvery, time.Minute))
		defer ticker.Stop()
		unread := 0 // consecutive failed reads: one is retried at the next tick, two stop the run
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				info, err := b.H.Info(ctx, state.VM)
				why := ""
				if err != nil && ctx.Err() == nil {
					if unread++; unread >= 2 {
						why = "cannot read VM " + state.VM + " twice in a row: " + err.Error()
					}
				} else if err == nil {
					unread = 0
					why = HostCheck(info, state)
				}
				if why != "" {
					mu.Lock()
					recheck = why
					mu.Unlock()
					cancel()
					// Every other call on the VM runs under the same changed configuration.
					b.cancelHolders(state.VM)
					return
				}
			}
		}
	}()
	stdout := &frameWriter{stream: "stdout", emit: emit}
	stderr := &frameWriter{stream: "stderr", emit: emit}
	code, err := b.H.Run(ctx, state.VM, request.Argv, request.Cwd, stdout, stderr)
	mu.Lock()
	why := recheck
	mu.Unlock()
	switch {
	case why != "":
		// The guest keeps running whatever it was running; turn the VM off, so nothing goes on with
		// the changed configuration (review 2026-11-21, main 41dada9).
		stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancelStop()
		if err := b.H.Stop(stopCtx, state.VM); err != nil {
			return 0, "REFUSED during the run: " + why + "; turning the VM off also failed: " + err.Error()
		}
		return 0, "REFUSED during the run: " + why + " (the VM was turned off)"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return 0, fmt.Sprintf("run: stopped after the %s timeout%s", timeout, detail(err))
	case ctx.Err() != nil:
		return 0, "run: cancelled (the client went away, or a reset)" + detail(err)
	case err != nil:
		return 0, "run failed: " + err.Error()
	}
	return code, ""
}

// detail is ": " and err's text, or "" (what the hypervisor said about a run it cut short).
func detail(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// frameWriter turns writes into stream frames.
type frameWriter struct {
	stream string
	emit   func(Frame) error
}

func (w *frameWriter) Write(p []byte) (int, error) {
	if err := w.emit(Frame{Stream: w.stream, Data: string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// lockFor is vm's lock, made on first use. b.mu must be held.
func (b *Broker) lockFor(vm string) *vmLock {
	if b.locks == nil {
		b.locks = map[string]*vmLock{}
	}
	key := strings.ToLower(vm)
	if b.locks[key] == nil {
		b.locks[key] = &vmLock{changed: make(chan struct{}), holders: map[int]context.CancelFunc{}, start: make(chan struct{}, 1)}
	}
	return b.locks[key]
}

// acquire takes the VM's lock, shared or (a reset) alone, waiting at most LockWait, and returns its
// release. Alone, it first cancels every call holding the VM, so a stuck run cannot keep it forever.
// cancel is this call's own, recorded so a later reset or a failed recheck can do the same to it.
func (b *Broker) acquire(ctx context.Context, vm string, alone bool, cancel context.CancelFunc) (func(), string) {
	b.mu.Lock()
	l := b.lockFor(vm)
	if alone {
		l.waiting++
		for _, holder := range l.holders {
			holder()
		}
	}
	b.mu.Unlock()
	giveUp := func() {
		if alone {
			b.mu.Lock()
			l.waiting--
			close(l.changed) // shared calls waiting behind this reset may go
			l.changed = make(chan struct{})
			b.mu.Unlock()
		}
	}
	wait := b.duration(b.LockWait, 2*time.Minute)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		b.mu.Lock()
		free := !l.alone && (alone && l.shared == 0 || !alone && l.waiting == 0)
		if free {
			if alone {
				l.waiting--
				l.alone = true
			} else {
				l.shared++
			}
			id := l.next
			l.next++
			l.holders[id] = cancel
			b.mu.Unlock()
			return func() {
				b.mu.Lock()
				delete(l.holders, id)
				if alone {
					l.alone = false
				} else {
					l.shared--
				}
				close(l.changed)
				l.changed = make(chan struct{})
				b.mu.Unlock()
			}, ""
		}
		changed := l.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			giveUp()
			return nil, fmt.Sprintf("VM %s is busy with a reset (waited %s); try again", vm, wait)
		case <-ctx.Done():
			giveUp()
			return nil, "cancelled while waiting for VM " + vm
		}
	}
}

// cancelHolders cancels every call holding vm: its configuration changed under them.
func (b *Broker) cancelHolders(vm string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, holder := range b.lockFor(vm).holders {
		holder()
	}
}

// gate is Gate with the VM's start token held, so of calls finding it stopped, one starts it and the
// others then find it running.
func (b *Broker) gate(ctx context.Context, state State) string {
	b.mu.Lock()
	token := b.lockFor(state.VM).start
	b.mu.Unlock()
	select {
	case token <- struct{}{}:
	case <-ctx.Done():
		return "cancelled while another call started VM " + state.VM
	}
	defer func() { <-token }()
	return Gate(ctx, b.H, state, b.hostAddresses())
}

func (b *Broker) duration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// hostAddresses are the host's own unicast addresses, as probe targets on SMB's port: a guest that
// reaches one has a path to the host.
func (b *Broker) hostAddresses() []string {
	if b.HostAddresses != nil {
		return b.HostAddresses()
	}
	var targets []string
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		targets = append(targets, net.JoinHostPort(ip.String(), "445"))
	}
	return targets
}

// record writes one log line.
func (b *Broker) record(fields map[string]any) error {
	if b.Log == nil {
		return errors.New("no log")
	}
	for key, value := range fields {
		fields[key] = capped(value)
	}
	line, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = b.Log.Write(append(line, '\n'))
	return err
}

// LogFieldLimit bounds each string the log records (an argv element, a path): a request may be up to
// 1 MiB, and every one is logged (review 2026-11-21, A6).
const LogFieldLimit = 4096

// capped is value with every string cut to LogFieldLimit bytes, marked where it was cut.
func capped(value any) any {
	cut := func(text string) string {
		if len(text) <= LogFieldLimit {
			return text
		}
		return text[:LogFieldLimit] + fmt.Sprintf("...(%d bytes)", len(text))
	}
	switch v := value.(type) {
	case string:
		return cut(v)
	case []string:
		out := make([]string, len(v))
		for i, text := range v {
			out[i] = cut(text)
		}
		return out
	}
	return value
}

// DailyLog appends to Dir\broker-YYYYMMDD.log, a new file each UTC day. Its directory is readable by
// SYSTEM and Administrators only (install.ps1): argv can hold anything the caller typed.
type DailyLog struct {
	Dir string
	Now func() time.Time
	mu  sync.Mutex
}

func (l *DailyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	file, err := os.OpenFile(filepath.Join(l.Dir, "broker-"+now().UTC().Format("20060102")+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := file.Write(p)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

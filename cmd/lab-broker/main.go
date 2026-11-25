// Command lab-broker is the only route into a lab VM. The owner's install runs it as SYSTEM from
// C:\Program Files\agent-containment\lab-broker.exe, as a scheduled task at startup that restarts on
// failure. It listens on lab.PipeName, which only SYSTEM, Administrators and the owner's account may
// open, proves the VM's containment before every call (lab.Gate), and refuses everything else.
//
//	lab-broker serve           answer requests until stopped
//	lab-broker check           one OK or FAIL line per fact the broker rests on; exit 2 if any fails
//	lab-broker set-owner SID   record SID as every lab state's client (the owner's account; install.ps1)
//
// The directories, the pipe and the hypervisor are fixed in the binary. Only the suite's builds set
// them (-ldflags -X), to run against a fake VM; check reports a build that is not the installed kind.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ryheff24/WAC/lab"
	"github.com/Ryheff24/WAC/registry"
)

var (
	registryDirectory = registry.Directory
	stateDirectory    = lab.StateDirectory
	pipeName          = lab.PipeName
	hypervisorKind    = "hyperv"
)

const usage = "usage: lab-broker serve | check | set-owner SID"

func main() {
	switch {
	case len(os.Args) == 2 && os.Args[1] == "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "lab-broker: "+err.Error())
			os.Exit(1)
		}
	case len(os.Args) == 2 && os.Args[1] == "check":
		os.Exit(check(os.Stdout))
	case len(os.Args) == 3 && os.Args[1] == "set-owner":
		if err := setOwner(stateDirectory, os.Args[2], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "lab-broker: set-owner: "+err.Error())
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(64)
	}
}

// serve answers requests until interrupted.
func serve() error {
	projects, err := registry.Load(registryDirectory)
	if err != nil {
		return err
	}
	client, err := ownerSID(stateDirectory)
	if err != nil {
		return err
	}
	if err := lab.ClearStaging(stateDirectory); err != nil {
		return fmt.Errorf("clearing staging left by an earlier run: %w", err)
	}
	logDirectory := filepath.Join(stateDirectory, "log")
	if err := os.MkdirAll(logDirectory, 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if hypervisorKind == "fake" {
		// The suite's builds stop when the file fake-stop appears (then one more connection wakes the
		// pipe), so their coverage is written at a normal exit. The installed kind has no such file.
		go func() {
			for ctx.Err() == nil {
				if _, err := os.Stat(filepath.Join(stateDirectory, "fake-stop")); err == nil {
					stop()
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
	broker := &lab.Broker{H: hypervisor(), Projects: projects, StateDir: stateDirectory, Log: &lab.DailyLog{Dir: logDirectory}}
	// Between calls too: a running lab VM whose host check fails is turned off.
	go broker.Watch(ctx)
	fmt.Println("lab-broker: serving " + pipeName + " (" + hypervisorKind + ")")
	server, err := lab.ProcessSID()
	if err != nil {
		return err
	}
	return lab.Serve(ctx, pipeName, lab.PipeSDDL(client, server), broker)
}

// hypervisor is Hyper-V, or a fake VM in the suite's builds.
func hypervisor() lab.Hypervisor {
	states := func(vm string) (lab.State, error) { return lab.LoadState(stateDirectory, vm) }
	if hypervisorKind == "fake" {
		return &fakeLabs{states: states, root: filepath.Join(stateDirectory, "fake-guest"), fakes: map[string]*lab.Fake{}}
	}
	return lab.HyperV{States: states}
}

// ownerSID is the one client SID every lab state records (the owner's account, which lab-run runs
// as, from Windows or WSL); the pipe admits it.
func ownerSID(directory string) (string, error) {
	states, err := allStates(directory)
	if err != nil {
		return "", err
	}
	owners := map[string]bool{}
	for _, state := range states {
		owners[state.ClientSID] = true
	}
	if len(owners) != 1 {
		return "", fmt.Errorf("the lab states in %s name %d owner accounts; there must be exactly one", filepath.Join(directory, "labs"), len(owners))
	}
	for sid := range owners {
		return sid, nil
	}
	return "", nil
}

// allStates loads every lab state, refusing the set if any is invalid.
func allStates(directory string) ([]lab.State, error) {
	entries, err := os.ReadDir(filepath.Join(directory, "labs"))
	if err != nil {
		return nil, fmt.Errorf("no lab states: %w", err)
	}
	var states []lab.State
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		state, err := lab.LoadState(directory, strings.TrimSuffix(name, filepath.Ext(name)))
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	if len(states) == 0 {
		return nil, fmt.Errorf("no lab states in %s", filepath.Join(directory, "labs"))
	}
	sort.Slice(states, func(i, j int) bool { return states[i].VM < states[j].VM })
	return states, nil
}

// setOwner records sid as the client of every lab state, rewriting only the states that differ. Each
// file is replaced whole (written beside it, then renamed over it), and every state is checked before
// any is written: an invalid SID or state changes nothing.
func setOwner(directory, sid string, out io.Writer) error {
	states, err := allStates(directory)
	if err != nil {
		return err
	}
	var changed []lab.State
	for _, state := range states {
		if state.ClientSID == sid {
			fmt.Fprintf(out, "lab state %s: client_sid is already %s\n", state.VM, sid)
			continue
		}
		state.ClientSID = sid
		if err := state.Validate(); err != nil {
			return fmt.Errorf("lab state %s: %w", state.VM, err)
		}
		changed = append(changed, state)
	}
	for _, state := range changed {
		text, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(directory, "labs", state.VM+".json")
		if err := os.WriteFile(path+".new", append(text, '\n'), 0o644); err != nil {
			return err
		}
		if err := os.Rename(path+".new", path); err != nil {
			os.Remove(path + ".new")
			return err
		}
		fmt.Fprintf(out, "lab state %s: client_sid set to %s\n", state.VM, sid)
	}
	return nil
}

// check reports the facts the broker rests on. It reports; it is not the boundary.
func check(out io.Writer) int {
	failed := false
	say := func(ok bool, text string) {
		status := "OK  "
		if !ok {
			status, failed = "FAIL", true
		}
		fmt.Fprintln(out, status+" "+text)
	}
	self, err := os.Executable()
	say(err == nil && strings.EqualFold(self, `C:\Program Files\agent-containment\lab-broker.exe`), "this binary runs from C:\\Program Files\\agent-containment (it is "+self+")")
	say(hypervisorKind == "hyperv", "the hypervisor is Hyper-V (this build: "+hypervisorKind+")")
	say(strings.EqualFold(stateDirectory, lab.StateDirectory) && strings.EqualFold(registryDirectory, registry.Directory) && pipeName == lab.PipeName,
		"directories and pipe are the installed ones ("+stateDirectory+", "+registryDirectory+", "+pipeName+")")
	projects, err := registry.Load(registryDirectory)
	say(err == nil, fmt.Sprintf("registry: %d projects %v", len(projects), err))
	states, err := allStates(stateDirectory)
	if err != nil {
		say(false, "lab states: "+err.Error())
	}
	for _, state := range states {
		say(true, fmt.Sprintf("lab state %s: project %s, %d disks, %s", state.VM, state.Project, len(state.Disks), map[bool]string{true: "Phase A (no adapter)", false: "Phase B (" + state.PrivateSwitch + ")"}[state.PrivateSwitch == ""]))
	}
	if sid, err := ownerSID(stateDirectory); err != nil {
		say(false, "owner: "+err.Error())
	} else {
		say(true, "owner: the pipe admits "+sid+" (and SYSTEM and Administrators)")
	}
	if failed {
		return 2
	}
	return 0
}

// fakeLabs is one lab.Fake per VM, for the suite's builds. A file named fake-adapter in the state
// directory gives every VM a network adapter, so the suite can watch the gate refuse.
type fakeLabs struct {
	states func(string) (lab.State, error)
	root   string
	fakes  map[string]*lab.Fake
}

func (f *fakeLabs) get(vm string) (*lab.Fake, error) {
	if fake, ok := f.fakes[vm]; ok {
		f.adapter(fake)
		return fake, nil
	}
	state, err := f.states(vm)
	if err != nil {
		return nil, err
	}
	fake := lab.NewFake(vm, state.Hostname, state, filepath.Join(f.root, vm))
	f.fakes[vm] = fake
	f.adapter(fake)
	return fake, nil
}

func (f *fakeLabs) adapter(fake *lab.Fake) {
	fake.Info_.Adapters = nil
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.root), "fake-adapter")); err == nil {
		fake.Info_.Adapters = []lab.Adapter{{Switch: "Default Switch", SwitchType: "Internal"}}
	}
}

func (f *fakeLabs) Info(ctx context.Context, vm string) (lab.VMInfo, error) {
	fake, err := f.get(vm)
	if err != nil {
		return lab.VMInfo{}, err
	}
	return fake.Info(ctx, vm)
}
func (f *fakeLabs) Stop(ctx context.Context, vm string) error {
	fake, err := f.get(vm)
	if err != nil {
		return err
	}
	return fake.Stop(ctx, vm)
}
func (f *fakeLabs) Start(ctx context.Context, vm string) error {
	fake, err := f.get(vm)
	if err != nil {
		return err
	}
	return fake.Start(ctx, vm)
}
func (f *fakeLabs) Probe(ctx context.Context, vm string, targets []string) (lab.ProbeResult, error) {
	fake, err := f.get(vm)
	if err != nil {
		return lab.ProbeResult{}, err
	}
	return fake.Probe(ctx, vm, targets)
}
func (f *fakeLabs) Run(ctx context.Context, vm string, argv []string, cwd string, stdout, stderr io.Writer) (int, error) {
	fake, err := f.get(vm)
	if err != nil {
		return 0, err
	}
	return fake.Run(ctx, vm, argv, cwd, stdout, stderr)
}
func (f *fakeLabs) CopyIn(ctx context.Context, vm, hostPath, guestDirectory string) error {
	fake, err := f.get(vm)
	if err != nil {
		return err
	}
	return fake.CopyIn(ctx, vm, hostPath, guestDirectory)
}
func (f *fakeLabs) CopyOut(ctx context.Context, vm, guestPath, hostArchive string, max int64) error {
	fake, err := f.get(vm)
	if err != nil {
		return err
	}
	return fake.CopyOut(ctx, vm, guestPath, hostArchive, max)
}
func (f *fakeLabs) Reset(ctx context.Context, vm string, state lab.State) error {
	fake, err := f.get(vm)
	if err != nil {
		return err
	}
	return fake.Reset(ctx, vm, state)
}

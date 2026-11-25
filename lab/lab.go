// Package lab is the broker's side of the only route into a lab VM: the request protocol, the
// gate that proves a VM's containment before every call, and the handling of each verb. Hyper-V is
// reached through the Hypervisor interface (hyperv.go is the real one), so every rule here is
// tested without a VM.
package lab

import (
	"context"
	"io"
	"time"
)

// PipeName is the named pipe lab-broker listens on. Its ACL admits SYSTEM, Administrators and the
// owner's account only (Serve), and it refuses remote clients.
const PipeName = `\\.\pipe\agent-containment-lab`

// StateDirectory holds each lab's state (labs\<vm>.json), the broker's log and its staging. Administrators
// and SYSTEM may write it; others may read the lab states, never the log, staging or secrets.
const StateDirectory = `C:\ProgramData\agent-containment`

// GuestInbox is where push copies into, and GuestWork where sync clones and builds happen. Both are
// in the guest; the install creates them.
const (
	GuestInbox = `C:\inbox`
	GuestWork  = `C:\work`
)

// Verbs the broker answers.
const (
	VerbRun    = "run"    // run argv in the guest
	VerbStatus = "status" // prove the gate and report it, running nothing
	VerbPush   = "push"   // copy a host file or directory into the guest's inbox
	VerbPull   = "pull"   // copy a guest file or directory into the project's lab-out
	VerbReset  = "reset"  // turn the VM off, rebuild its differencing disks, start it
)

// Request is one call, one JSON object on the pipe.
type Request struct {
	Verb     string   `json:"verb"`
	Project  string   `json:"project"`
	VM       string   `json:"vm,omitempty"`        // default: the project's first lab VM
	Argv     []string `json:"argv,omitempty"`      // run
	Cwd      string   `json:"cwd,omitempty"`       // run: the guest directory
	Timeout  int      `json:"timeout_s,omitempty"` // run: seconds; 0 is DefaultTimeout
	HostPath string   `json:"host_path,omitempty"` // push
	Guest    string   `json:"guest_path,omitempty"`
}

// Frame is one JSON object the broker writes back. A call ends with exactly one frame holding
// Exit, or one holding Refused (then the exit code is 2).
type Frame struct {
	Stream  string `json:"stream,omitempty"` // "stdout" or "stderr"
	Data    string `json:"data,omitempty"`
	Exit    *int   `json:"exit,omitempty"`
	Refused string `json:"refused,omitempty"`
}

// DefaultMaxPush and DefaultPushReserve bound a push (Broker.MaxPush, Broker.PushReserve).
const (
	DefaultMaxPush     = 32 << 30
	DefaultPushReserve = 20 << 30
)

// DiskFloor is the least free space the gate allows on the volumes holding a lab's disks. Below it the
// VM does not start, and a run's recheck turns it off: a guest rewriting its game volume grew the game
// disk's child by tens of GB in one boot, filled the host's C: and paused the VM (2026-11-23). It is under
// DefaultPushReserve, so a push that leaves the reserve still leaves room to run.
const DiskFloor = 10 << 30

// DefaultTimeout and MaxTimeout bound a run.
const (
	DefaultTimeout = time.Hour
	MaxTimeout     = 24 * time.Hour
)

// Adapter is one network adapter on a VM.
type Adapter struct {
	Switch     string `json:"switch"`
	SwitchType string `json:"switch_type"` // Private, Internal, External, or "" when not connected
}

// VMInfo is what the host side of the gate reads about a VM: Hyper-V's own configuration, which the
// guest cannot influence. It is the lock; the in-guest probe is a diagnostic (review 2026-11-21, #7).
type VMInfo struct {
	Name                  string    `json:"name"`
	State                 string    `json:"state"` // Running, Off, …
	Adapters              []Adapter `json:"adapters"`
	GuestServiceInterface bool      `json:"guest_service_interface"`
	EnhancedSession       bool      `json:"enhanced_session"` // the host's EnableEnhancedSessionMode
	GPUPartitions         int       `json:"gpu_partitions"`
	Disks                 []Disk    `json:"disks"`
	DVDDrives             int       `json:"dvd_drives"`
	COMPorts              []string  `json:"com_ports"`        // ports connected to a named pipe
	AssignedDevices       int       `json:"assigned_devices"` // devices passed through (DDA), a NIC included
	FibreChannel          int       `json:"fibre_channel"`
	// DisksFree is the least space free on any volume holding an attached disk's file: the
	// differencing disks grow there as the guest writes.
	DisksFree int64 `json:"disks_free"`
}

// Disk is an attached virtual disk and its differencing parent ("" when it has none). A disk
// attached as a pass-through physical disk has no path.
type Disk struct {
	Path   string `json:"path"`
	Parent string `json:"parent"`
}

// Caller runs host file I/O as the client that made the request (ImpersonateNamedPipeClient), so the
// broker, which runs as SYSTEM, never reads or writes a host path the caller could not (review
// 2026-11-21: pull and push, #1 to #3 of both reviews).
type Caller interface {
	As(f func() error) error
	PID() uint32
}

// Self is a Caller that does nothing special: the process acts as itself. The tests use it, and so
// does any transport that has no client to impersonate.
type Self struct{}

func (Self) As(f func() error) error { return f() }
func (Self) PID() uint32             { return 0 }

// ProbeResult is what the in-guest probe reports.
type ProbeResult struct {
	Hostname   string   `json:"hostname"`
	AdaptersUp int      `json:"adapters_up"`
	Reached    []string `json:"reached"`  // targets a TCP connect reached: must be empty
	DNS        bool     `json:"dns"`      // a public name resolved: must be false
	Loopback   bool     `json:"loopback"` // the positive control: must be true
}

// Hypervisor is the broker's reach into Hyper-V and the guest. The real one (hyperv.go) runs fixed
// Windows PowerShell scripts as SYSTEM; the tests use a fake.
type Hypervisor interface {
	Info(ctx context.Context, vm string) (VMInfo, error)
	Start(ctx context.Context, vm string) error
	Stop(ctx context.Context, vm string) error // turn the VM off now (Stop-VM -TurnOff)
	Probe(ctx context.Context, vm string, targets []string) (ProbeResult, error)
	Run(ctx context.Context, vm string, argv []string, cwd string, stdout, stderr io.Writer) (int, error)
	CopyIn(ctx context.Context, vm string, hostPath, guestDirectory string) error
	// CopyOut writes a tar archive of guestPath (its last name as the archive's top entry) to the new
	// file hostArchive, refusing a tree holding links and an archive over max bytes.
	CopyOut(ctx context.Context, vm string, guestPath, hostArchive string, max int64) error
	Reset(ctx context.Context, vm string, state State) error
}

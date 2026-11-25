package lab

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// ProbeTargets are what the guest must not reach: a public resolver's HTTPS port and DNS port, and a
// documentation-range address (RFC 5737: nobody answers it, but a broken containment could still send
// toward a gateway). The host's own addresses are added per call (HostAddresses).
var ProbeTargets = []string{"1.1.1.1:443", "8.8.8.8:53", "192.0.2.1:80"}

// HostCheck is the gate's lock: Hyper-V's own configuration of the VM against its recorded state.
// It has no side effects and the guest cannot influence it, so it runs before anything can start the
// VM (review 2026-11-21, #1), again after a start, and every RecheckEvery during a run. "" means the
// configuration is contained.
//
//   - Phase A only: a lab state naming a Private switch (Phase B) is refused until Phase B proves the
//     switch's membership and its peers' topology (review #7).
//   - No network adapter, no DVD drive, no COM port connected to a pipe, no assigned (passed-through)
//     device, no Fibre Channel adapter.
//   - Guest Service Interface off; the host's Enhanced Session mode off.
//   - Exactly the recorded disks, each with its recorded differencing parent: os_diff over os_base,
//     game_diff over the game parent, every other disk with none (review #4).
//   - The game parent at its recorded size and write time.
//   - At least DiskFloor free on every volume holding an attached disk.
func HostCheck(info VMInfo, state State) string {
	switch {
	case state.PrivateSwitch != "":
		return "Phase B (a Private switch) is not built yet: its switch membership and peers are not proved, so lab state " + state.VM + " is refused"
	case len(info.Adapters) > 0:
		return fmt.Sprintf("VM %s has %d network adapters; a Phase A lab has none (Remove-VMNetworkAdapter, as the owner)", state.VM, len(info.Adapters))
	case info.DVDDrives > 0:
		return fmt.Sprintf("VM %s has %d DVD drives; a lab has none (media from the host is a way in)", state.VM, info.DVDDrives)
	case len(info.COMPorts) > 0:
		return "VM " + state.VM + " has a COM port connected to " + strings.Join(info.COMPorts, ", ") + "; a lab has none"
	case info.AssignedDevices > 0:
		return fmt.Sprintf("VM %s has %d assigned host devices (one could be a network card); a lab has none", state.VM, info.AssignedDevices)
	case info.FibreChannel > 0:
		return fmt.Sprintf("VM %s has %d Fibre Channel adapters; a lab has none", state.VM, info.FibreChannel)
	case info.GuestServiceInterface:
		return "VM " + state.VM + " has Guest Service Interface on; it copies files between host and guest outside the broker"
	case info.EnhancedSession:
		return "the host's Enhanced Session mode is on; it can share host drives and the clipboard with a guest"
	}
	if why := disks(info.Disks, state); why != "" {
		return why
	}
	if state.GameParent != nil {
		if why := unchanged(*state.GameParent); why != "" {
			return why
		}
	}
	if info.DisksFree < DiskFloor {
		return fmt.Sprintf("the volume holding VM %s's disks has %.1f GiB free, under the %d GiB a running lab needs (its differencing disks grow there; a full volume pauses the VM and starves the host)",
			state.VM, float64(info.DisksFree)/(1<<30), DiskFloor>>30)
	}
	return ""
}

// disks requires exactly the recorded disks, each over its recorded parent.
func disks(attached []Disk, state State) string {
	if len(attached) != len(state.Disks) {
		return fmt.Sprintf("VM %s has %d disks attached; the lab state records %d (%s)", state.VM, len(attached), len(state.Disks), strings.Join(state.Disks, ", "))
	}
	for _, disk := range attached {
		if disk.Path == "" {
			return "VM " + state.VM + " has a physical disk passed through"
		}
		if !containsFold(state.Disks, disk.Path) {
			return "VM " + state.VM + " has a disk the lab state does not list: " + disk.Path
		}
		want := ""
		switch {
		case equalFoldPath(disk.Path, state.OSDiff):
			want = state.OSBase
		case state.GameDiff != "" && equalFoldPath(disk.Path, state.GameDiff):
			want = state.GameParent.Path
		}
		if (want == "") != (disk.Parent == "") || want != "" && !equalFoldPath(disk.Parent, want) {
			return fmt.Sprintf("VM %s's disk %s has parent %q; the lab state records %q", state.VM, disk.Path, disk.Parent, want)
		}
	}
	return ""
}

// Gate proves a lab VM's containment before a call uses it, as d1's C gate proves the game's. It
// returns "" when the VM may be used, else the one-line reason.
//
// The host check runs first, with no side effect, so a misconfigured VM is never started. Only then
// is a stopped VM started, and the host check repeated against the running VM. Last, a fixed probe in
// the guest over PowerShell Direct: no adapter up, no TCP connection to the probe targets or the
// host's addresses, no public DNS, and the positive control in the same call (a loopback connection
// succeeds; the computer name is the recorded one). The probe is a diagnostic: the guest's
// administrator could forge it, so the host check is what holds (review #7).
func Gate(ctx context.Context, h Hypervisor, state State, hostAddresses []string) string {
	info, err := h.Info(ctx, state.VM)
	if err != nil {
		return "cannot read VM " + state.VM + ": " + err.Error()
	}
	if why := HostCheck(info, state); why != "" {
		return why
	}
	if !strings.EqualFold(info.State, "Running") {
		if err := h.Start(ctx, state.VM); err != nil {
			return "VM " + state.VM + " is " + info.State + " and did not start: " + err.Error()
		}
		if info, err = h.Info(ctx, state.VM); err != nil {
			return "cannot read VM " + state.VM + " after starting it: " + err.Error()
		}
		if why := HostCheck(info, state); why != "" {
			return "after starting: " + why
		}
	}
	return Probe(ctx, h, state, hostAddresses)
}

// Probe is the gate's in-guest diagnostic, with its positive control.
func Probe(ctx context.Context, h Hypervisor, state State, hostAddresses []string) string {
	targets := append(append([]string{}, ProbeTargets...), hostAddresses...)
	result, err := h.Probe(ctx, state.VM, targets)
	if err != nil {
		return "the in-guest probe did not run: " + err.Error()
	}
	switch {
	case !result.Loopback:
		return "positive control failed: a loopback connection in the guest did not succeed, so the probe's refusals prove nothing"
	case !strings.EqualFold(result.Hostname, state.Hostname):
		return fmt.Sprintf("positive control failed: the guest is %q, the lab state says %q", result.Hostname, state.Hostname)
	case len(result.Reached) > 0:
		return "the guest reached " + strings.Join(result.Reached, ", ") + ": it has a network path"
	case result.DNS:
		return "a public name resolved in the guest: it has a network path"
	case result.AdaptersUp > 0:
		return fmt.Sprintf("the guest reports %d network adapters up; a Phase A lab has none", result.AdaptersUp)
	}
	return ""
}

// unchanged checks a file against its recorded size and write time.
func unchanged(fact FileFact) string {
	info, err := os.Stat(fact.Path)
	if err != nil {
		return "the game disk " + fact.Path + " cannot be read: " + err.Error()
	}
	if info.Size() != fact.Size || !info.ModTime().Equal(fact.Modified) {
		return fmt.Sprintf("the game disk %s changed (size %d, written %s; recorded %d, %s): its parent must never be written",
			fact.Path, info.Size(), info.ModTime().UTC().Format("2006-01-02T15:04:05Z"), fact.Size, fact.Modified.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return ""
}

package lab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// State is one lab VM's facts as the owner's install recorded them, in
// StateDirectory\labs\<vm>.json. The gate compares the VM against it on every call.
type State struct {
	VM       string `json:"vm"`
	Project  string `json:"project"`
	Hostname string `json:"hostname"` // the guest's computer name, the positive control
	// ClientSID is the account lab-run calls from: the owner's (install.ps1 sets it). The pipe admits it.
	ClientSID string `json:"client_sid"`
	// PrivateSwitch is empty in Phase A (no adapter at all). In Phase B it names the Hyper-V Private
	// switch the lab's VMs share, and every adapter must be on it.
	PrivateSwitch string `json:"private_switch,omitempty"`
	// Disks are the only virtual disks the VM may have attached.
	Disks []string `json:"disks"`
	// OSBase and OSDiff: the sealed system disk and its differencing child, which reset rebuilds.
	OSBase string `json:"os_base"`
	OSDiff string `json:"os_diff"`
	// GameParent is the read-only game disk; GameDiff its child. Empty when the lab has no game disk.
	GameParent *FileFact `json:"game_parent,omitempty"`
	GameDiff   string    `json:"game_diff,omitempty"`
	// Credential is the DPAPI (LocalMachine) blob of the guest account's password, readable by SYSTEM
	// and Administrators only.
	Credential string `json:"credential"`
	// GuestUser is the guest's local administrator, the account PowerShell Direct signs in as.
	GuestUser string `json:"guest_user"`
}

// FileFact is a file's size and last write time, to tell whether it changed.
type FileFact struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,15}$`)
var sidPattern = regexp.MustCompile(`^S-1-5-21-\d+-\d+-\d+-\d+$`)

// Validate reports the first problem with s, or nil.
func (s State) Validate() error {
	switch {
	case !vmPattern.MatchString(s.VM):
		return fmt.Errorf("vm %q: letters, digits and '-' only", s.VM)
	case s.Project == "":
		return errors.New("project is empty")
	case !hostnamePattern.MatchString(s.Hostname):
		return fmt.Errorf("hostname %q: 1-15 letters, digits and '-'", s.Hostname)
	case !sidPattern.MatchString(s.ClientSID):
		return fmt.Errorf("client_sid %q is not an account SID", s.ClientSID)
	case len(s.Disks) == 0:
		return errors.New("disks is empty")
	case s.OSBase == "" || s.OSDiff == "":
		return errors.New("os_base and os_diff are required")
	case s.Credential == "" || s.GuestUser == "":
		return errors.New("credential and guest_user are required")
	case (s.GameParent == nil) != (s.GameDiff == ""):
		return errors.New("game_parent and game_diff come together")
	case s.PrivateSwitch != "" && !vmPattern.MatchString(s.PrivateSwitch):
		return fmt.Errorf("private_switch %q: letters, digits and '-' only", s.PrivateSwitch)
	}
	if !containsFold(s.Disks, s.OSDiff) {
		return errors.New("os_diff is not among disks")
	}
	if s.GameDiff != "" && !containsFold(s.Disks, s.GameDiff) {
		return errors.New("game_diff is not among disks")
	}
	return nil
}

var vmPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// LoadState reads and validates StateDirectory\labs\<vm>.json (directory is StateDirectory in
// production), refusing unknown fields.
func LoadState(directory, vm string) (State, error) {
	if !vmPattern.MatchString(vm) {
		return State{}, fmt.Errorf("lab VM name %q is not valid", vm)
	}
	path := filepath.Join(directory, "labs", vm+".json")
	text, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("lab state %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("lab state %s: %w", path, err)
	}
	if state.VM != vm {
		return State{}, fmt.Errorf("lab state %s names vm %q", path, state.VM)
	}
	if err := state.Validate(); err != nil {
		return State{}, fmt.Errorf("lab state %s: %w", path, err)
	}
	return state, nil
}

func containsFold(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(filepath.Clean(item), filepath.Clean(value)) {
			return true
		}
	}
	return false
}

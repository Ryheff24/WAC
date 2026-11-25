package lab

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Ryheff24/WAC/registry"
)

// fixture is a project, a lab state on disk, a fake VM and a broker over them.
type fixture struct {
	root, stateDir string
	project        registry.Project
	state          State
	fake           *Fake
	broker         *Broker
	log            *lockedBuffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{root: filepath.Join(base, "proj"), stateDir: filepath.Join(base, "state"), log: &lockedBuffer{}}
	for _, d := range []string{f.root, filepath.Join(f.root, "prior-art"), filepath.Join(f.stateDir, "labs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.project = registry.Project{Name: "proj", Root: f.root, LabVMs: []string{"proj-lab", "proj-srv"}}
	f.state = State{VM: "proj-lab", Project: "proj", Hostname: "PROJ-LAB", ClientSID: "S-1-5-21-1-2-3-1001",
		Disks: []string{`C:\vm\os-diff.vhdx`, `C:\vm\work.vhdx`}, OSBase: `C:\vm\os-base.vhdx`, OSDiff: `C:\vm\os-diff.vhdx`,
		Credential: `C:\vm\guest.cred`, GuestUser: "lab"}
	f.writeState(t, f.state)
	f.fake = NewFake("proj-lab", "PROJ-LAB", f.state, filepath.Join(base, "guest"))
	f.broker = &Broker{H: f.fake, Projects: []registry.Project{f.project}, StateDir: f.stateDir, Log: f.log,
		HostAddresses: func() []string { return []string{"10.0.0.5:445"} },
		Now:           func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }}
	return f
}

func (f *fixture) writeState(t *testing.T, state State) {
	t.Helper()
	text, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(f.stateDir, "labs", state.VM+".json"), text, 0o644); err != nil {
		t.Fatal(err)
	}
}

// call runs a request through the broker and returns the exit code, the output and the refusal.
func (f *fixture) call(request Request) (int, string, string) {
	var out strings.Builder
	refused := ""
	code := f.broker.Handle(context.Background(), Self{}, request, func(frame Frame) error {
		out.WriteString(frame.Data)
		if frame.Refused != "" {
			refused = frame.Refused
		}
		return nil
	})
	return code, out.String(), refused
}

func TestHostCheck(t *testing.T) {
	f := newFixture(t)
	gameDisk := filepath.Join(t.TempDir(), "game.vhdx")
	os.WriteFile(gameDisk, []byte("disk"), 0o644)
	info, _ := os.Stat(gameDisk)
	withGame := func(s *State) {
		s.GameParent = &FileFact{Path: gameDisk, Size: info.Size(), Modified: info.ModTime()}
		s.GameDiff = `C:\vm\game-diff.vhdx`
		s.Disks = append(append([]string{}, s.Disks...), s.GameDiff)
	}
	cases := []struct {
		name  string
		setup func(*VMInfo, *State)
		want  string
	}{
		{"contained", func(*VMInfo, *State) {}, ""},
		{"an adapter", func(i *VMInfo, _ *State) { i.Adapters = []Adapter{{Switch: "Default Switch", SwitchType: "Internal"}} }, "a Phase A lab has none"},
		{"Phase B refused", func(_ *VMInfo, s *State) { s.PrivateSwitch = "lab-private" }, "Phase B"},
		{"a DVD drive", func(i *VMInfo, _ *State) { i.DVDDrives = 1 }, "DVD"},
		{"a COM port", func(i *VMInfo, _ *State) { i.COMPorts = []string{`\\.\pipe\x`} }, "COM port"},
		{"an assigned device", func(i *VMInfo, _ *State) { i.AssignedDevices = 1 }, "assigned host devices"},
		{"fibre channel", func(i *VMInfo, _ *State) { i.FibreChannel = 1 }, "Fibre Channel"},
		{"guest services", func(i *VMInfo, _ *State) { i.GuestServiceInterface = true }, "Guest Service Interface"},
		{"enhanced session", func(i *VMInfo, _ *State) { i.EnhancedSession = true }, "Enhanced Session"},
		{"disks' volume nearly full", func(i *VMInfo, _ *State) { i.DisksFree = DiskFloor - 1 }, "GiB free"},
		{"disks' volume unmeasured", func(i *VMInfo, _ *State) { i.DisksFree = 0 }, "GiB free"},
		{"disks' volume at the floor", func(i *VMInfo, _ *State) { i.DisksFree = DiskFloor }, ""},
		{"an extra disk", func(i *VMInfo, _ *State) { i.Disks = append(i.Disks, Disk{Path: `D:\host.vhdx`}) }, "disks attached"},
		{"a missing disk", func(i *VMInfo, _ *State) { i.Disks = i.Disks[:1] }, "disks attached"},
		{"a swapped disk", func(i *VMInfo, _ *State) { i.Disks[1] = Disk{Path: `D:\other.vhdx`} }, "does not list"},
		{"a passed-through disk", func(i *VMInfo, _ *State) { i.Disks[1] = Disk{} }, "physical disk"},
		{"os diff over another parent", func(i *VMInfo, _ *State) { i.Disks[0].Parent = `D:\evil-base.vhdx` }, "has parent"},
		{"work disk with a parent", func(i *VMInfo, _ *State) { i.Disks[1].Parent = `D:\x.vhdx` }, "has parent"},
		{"game disk unchanged", func(i *VMInfo, s *State) {
			withGame(s)
			i.Disks = append(i.Disks, Disk{Path: s.GameDiff, Parent: gameDisk})
		}, ""},
		{"game diff over another parent", func(i *VMInfo, s *State) {
			withGame(s)
			i.Disks = append(i.Disks, Disk{Path: s.GameDiff, Parent: `D:\fake-game.vhdx`})
		}, "has parent"},
		{"game disk changed", func(i *VMInfo, s *State) {
			withGame(s)
			s.GameParent.Size++
			i.Disks = append(i.Disks, Disk{Path: s.GameDiff, Parent: gameDisk})
		}, "must never be written"},
		{"game disk missing", func(i *VMInfo, s *State) {
			withGame(s)
			s.GameParent.Path = gameDisk + ".gone"
			i.Disks = append(i.Disks, Disk{Path: s.GameDiff, Parent: s.GameParent.Path})
		}, "cannot be read"},
	}
	for _, c := range cases {
		fake := NewFake("proj-lab", "PROJ-LAB", f.state, "")
		state := f.state
		c.setup(&fake.Info_, &state)
		got := HostCheck(fake.Info_, state)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: HostCheck = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGateNeverStartsAnUnsafeVM(t *testing.T) {
	f := newFixture(t)
	fake := NewFake("proj-lab", "PROJ-LAB", f.state, "")
	fake.Info_.State = "Off"
	fake.Info_.Adapters = []Adapter{{Switch: "Default Switch", SwitchType: "Internal"}}
	if why := Gate(context.Background(), fake, f.state, nil); !strings.Contains(why, "network adapters") || fake.Started != 0 {
		t.Errorf("Gate = %q, Started %d; want a refusal and no start", why, fake.Started)
	}
	fake.Info_.Adapters = nil
	if why := Gate(context.Background(), fake, f.state, nil); why != "" || fake.Started != 1 {
		t.Errorf("a contained stopped VM: Gate = %q, Started %d", why, fake.Started)
	}
	for name, setup := range map[string]func(*Fake){
		"does not start": func(h *Fake) { h.Info_.State = "Off"; h.StartErr = errors.New("no") },
		"unreadable":     func(h *Fake) { h.InfoErr = errors.New("gone") },
		"probe fails":    func(h *Fake) { h.ProbeErr = errors.New("no session") },
		"no loopback":    func(h *Fake) { h.Probe_.Loopback = false },
		"wrong guest":    func(h *Fake) { h.Probe_.Hostname = "OTHER" },
		"reached":        func(h *Fake) { h.Probe_.Reached = []string{"1.1.1.1:443"} },
		"dns":            func(h *Fake) { h.Probe_.DNS = true },
		"adapter up":     func(h *Fake) { h.Probe_.AdaptersUp = 1 },
	} {
		h := NewFake("proj-lab", "PROJ-LAB", f.state, "")
		setup(h)
		if why := Gate(context.Background(), h, f.state, nil); why == "" {
			t.Errorf("%s: Gate passed", name)
		}
	}
	// A VM that changes while starting is refused after the start.
	changing := &startChanges{Fake: NewFake("proj-lab", "PROJ-LAB", f.state, "")}
	changing.Info_.State = "Off"
	if why := Gate(context.Background(), changing, f.state, nil); !strings.HasPrefix(why, "after starting:") {
		t.Errorf("a VM that gains an adapter while starting: %q", why)
	}
}

// startChanges is a Fake that gains an adapter when started.
type startChanges struct{ *Fake }

func (s *startChanges) Start(ctx context.Context, vm string) error {
	s.Info_.Adapters = []Adapter{{Switch: "x", SwitchType: "External"}}
	return s.Fake.Start(ctx, vm)
}

func TestBrokerVerbs(t *testing.T) {
	f := newFixture(t)
	if code, out, refused := f.call(Request{Verb: VerbStatus, Project: "proj"}); code != 0 || refused != "" || !strings.Contains(out, "guest diagnostics passed") {
		t.Errorf("status: %d %q %q", code, out, refused)
	}
	f.fake.Info_.State = "Off"
	if code, out, _ := f.call(Request{Verb: VerbStatus, Project: "proj"}); code != 0 || !strings.Contains(out, "A run starts it") || f.fake.Started != 0 {
		t.Errorf("status of a stopped VM: %d %q, Started %d (status must not start it)", code, out, f.fake.Started)
	}
	if code, out, _ := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"echo", "hi"}, Cwd: `C:\work`}); code != 0 || !strings.Contains(out, "ran echo hi") || f.fake.Started != 1 {
		t.Errorf("run starts the VM: %d %q, Started %d", code, out, f.fake.Started)
	}
	if code, _, _ := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"exit", "7"}}); code != 7 {
		t.Errorf("run exit 7: got %d", code)
	}
	if code, out, _ := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"stderr", "oops"}}); code != 0 || !strings.Contains(out, "oops") {
		t.Errorf("run stderr: %d %q", code, out)
	}
	if code, _, refused := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}, Timeout: 1}); code != 2 || !strings.Contains(refused, "timeout") {
		t.Errorf("run timeout: %d %q", code, refused)
	}
	for _, r := range []Request{
		{Verb: VerbRun, Project: "proj"},
		{Verb: VerbRun, Project: "proj", Argv: []string{"x"}, Timeout: int(MaxTimeout/time.Second) + 1},
		{Verb: "frob", Project: "proj"},
		{Verb: VerbRun, Project: "nope", Argv: []string{"x"}},
		{Verb: VerbRun, Project: "proj", VM: "elsewhere", Argv: []string{"x"}},
		{Verb: VerbRun, Project: "proj", VM: "proj-srv", Argv: []string{"x"}},
		{Verb: VerbPull, Project: "proj"},
	} {
		if code, _, refused := f.call(r); code != 2 || refused == "" {
			t.Errorf("%+v: code %d, refused %q; want a refusal", r, code, refused)
		}
	}
	f.fake.RunErr = errors.New("session broke")
	if _, _, refused := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"x"}}); !strings.Contains(refused, "session broke") {
		t.Errorf("run error: %q", refused)
	}
	f.fake.RunErr = nil
	if code, _, _ := f.call(Request{Verb: VerbReset, Project: "proj"}); code != 0 || f.fake.ResetCount != 1 {
		t.Errorf("reset: %d, resets %d", code, f.fake.ResetCount)
	}
	f.fake.ResetErr = errors.New("locked")
	if _, _, refused := f.call(Request{Verb: VerbReset, Project: "proj"}); !strings.Contains(refused, "locked") {
		t.Errorf("reset error: %q", refused)
	}
	f.fake.ResetErr = nil
	f.fake.Info_.Adapters = []Adapter{{Switch: "x", SwitchType: "External"}}
	resets := f.fake.ResetCount
	if code, _, refused := f.call(Request{Verb: VerbReset, Project: "proj"}); code != 2 || !strings.HasPrefix(refused, "REFUSED: ") || f.fake.ResetCount != resets {
		t.Errorf("reset of an unsafe VM: %d %q, resets %d -> %d (it must not reset)", code, refused, resets, f.fake.ResetCount)
	}
	for _, verb := range []string{VerbRun, VerbStatus} {
		if code, _, refused := f.call(Request{Verb: verb, Project: "proj", Argv: []string{"x"}}); code != 2 || !strings.HasPrefix(refused, "REFUSED: ") {
			t.Errorf("%s on a VM with an adapter: %d %q", verb, code, refused)
		}
	}
	f.fake.InfoErr = errors.New("hyper-v gone")
	for _, verb := range []string{VerbStatus, VerbReset} {
		if _, _, refused := f.call(Request{Verb: verb, Project: "proj"}); !strings.Contains(refused, "hyper-v gone") {
			t.Errorf("%s without Hyper-V: %q", verb, refused)
		}
	}
	f.fake.InfoErr = nil
	f.fake.Info_.Adapters = nil
	f.fake.Probe_.DNS = true
	if _, _, refused := f.call(Request{Verb: VerbStatus, Project: "proj"}); !strings.Contains(refused, "resolved") {
		t.Errorf("status with DNS in the guest: %q", refused)
	}
	if !strings.Contains(f.log.String(), `"event":"start"`) || !strings.Contains(f.log.String(), `"refused":"REFUSED: `) {
		t.Errorf("the log lacks the calls: %s", f.log.String())
	}
}

func TestRunRecheckAndCancel(t *testing.T) {
	f := newFixture(t)
	f.broker.RecheckEvery = 20 * time.Millisecond
	go func() {
		time.Sleep(60 * time.Millisecond)
		f.fake.Update(func(h *Fake) { h.Info_.Adapters = []Adapter{{Switch: "late", SwitchType: "External"}} })
	}()
	if _, _, refused := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}, Timeout: 10}); !strings.Contains(refused, "REFUSED during the run") ||
		!strings.Contains(refused, "turned off") {
		t.Errorf("an adapter added mid-run: %q", refused)
	}
	if f.fake.Stopped != 1 || f.fake.Info_.State != "Off" {
		t.Errorf("a failed recheck must turn the VM off: stopped %d, state %s", f.fake.Stopped, f.fake.Info_.State)
	}
	f.fake.Info_.Adapters = nil
	f.fake.Info_.State = "Running"
	// Mid-run, one failed read is retried; two in a row stop the run. (Before a run, one refuses.)
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.fake.Update(func(h *Fake) { h.InfoFails = 1 })
	}()
	if _, _, refused := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}, Timeout: 1}); !strings.Contains(refused, "timeout") {
		t.Errorf("one transient read error mid-run must not stop it: %q", refused)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.fake.Update(func(h *Fake) { h.InfoErr = errors.New("hyper-v gone") })
	}()
	if _, _, refused := f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}, Timeout: 10}); !strings.Contains(refused, "twice in a row") {
		t.Errorf("Hyper-V unreadable mid-run: %q", refused)
	}
	f.fake.Info_.State = "Running"
	f.fake.InfoErr = nil
	// A client that cannot take frames cancels its run.
	code := f.broker.Handle(context.Background(), Self{}, Request{Verb: VerbRun, Project: "proj", Argv: []string{"echo", "x"}},
		func(Frame) error { return errors.New("client gone") })
	if code != 2 {
		t.Errorf("a gone client: code %d", code)
	}
}

func TestLockAndLog(t *testing.T) {
	f := newFixture(t)
	f.broker.LockWait = 50 * time.Millisecond
	held, cancelHeld := context.WithCancel(context.Background())
	defer cancelHeld()
	// Held alone (as a reset holds it), the VM makes other calls wait, then refuse as busy.
	release, why := f.broker.acquire(held, "proj-lab", true, cancelHeld)
	if why != "" {
		t.Fatal(why)
	}
	if _, _, refused := f.call(Request{Verb: VerbStatus, Project: "proj"}); !strings.Contains(refused, "busy") {
		t.Errorf("a held VM: %q", refused)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, why := f.broker.acquire(ctx, "proj-lab", false, cancel); !strings.Contains(why, "cancelled") {
		t.Errorf("a cancelled wait: %q", why)
	}
	// A reset cancels the call holding the VM, then takes it once that call lets go.
	go func() {
		<-held.Done()
		release()
	}()
	f.broker.LockWait = 2 * time.Second
	if code, _, refused := f.call(Request{Verb: VerbReset, Project: "proj"}); code != 0 || refused != "" {
		t.Errorf("a reset of a held VM: %d %q", code, refused)
	}
	if held.Err() == nil {
		t.Error("the reset did not cancel the call holding the VM")
	}
	f.broker.LockWait = 50 * time.Millisecond
	f.broker.Log = failingWriter{}
	if code, _, refused := f.call(Request{Verb: VerbStatus, Project: "proj"}); code != 2 || !strings.Contains(refused, "log cannot be written") {
		t.Errorf("an unwritable log: %d %q", code, refused)
	}
	f.broker.Log = nil
	if code, _, _ := f.call(Request{Verb: VerbStatus, Project: "proj"}); code != 2 {
		t.Error("no log at all must refuse")
	}
	dir := t.TempDir()
	day := time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC)
	daily := &DailyLog{Dir: dir, Now: func() time.Time { return day }}
	daily.Write([]byte("a\n"))
	day = day.Add(2 * time.Hour)
	daily.Write([]byte("b\n"))
	if files, _ := filepath.Glob(filepath.Join(dir, "broker-*.log")); len(files) != 2 {
		t.Errorf("the log did not roll: %v", files)
	}
	if _, err := (&DailyLog{Dir: filepath.Join(dir, "missing", "x")}).Write([]byte("x")); err == nil {
		t.Error("a log in a missing directory wrote")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestPushAndPull(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(f.root, "prior-art", "tool.zip")
	os.WriteFile(file, []byte("zip"), 0o644)
	if code, out, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: file}); code != 0 || !strings.Contains(out, `pushed`) {
		t.Fatalf("push: %d %q %q", code, out, refused)
	}
	if data, err := os.ReadFile(filepath.Join(f.fake.GuestRoot, "C", "inbox", "tool.zip")); err != nil || string(data) != "zip" {
		t.Errorf("the guest inbox lacks the file: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.stateDir, "staging")); len(entries) != 0 {
		t.Errorf("staging was not cleaned: %v", entries)
	}
	tree := filepath.Join(f.root, "prior-art", "tree")
	os.MkdirAll(filepath.Join(tree, "sub"), 0o755)
	os.WriteFile(filepath.Join(tree, "sub", "a.txt"), []byte("a"), 0o644)
	if code, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: tree}); code != 0 {
		t.Errorf("push a tree: %q", refused)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("s"), 0o644)
	junction := filepath.Join(f.root, "prior-art", "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, filepath.Dir(outside)).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	for name, path := range map[string]string{
		"outside the project": outside,
		"through a junction":  filepath.Join(junction, "secret.txt"), "missing": filepath.Join(f.root, "nope"),
	} {
		if code, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: path}); code != 2 || refused == "" {
			t.Errorf("push %s (%s): code %d, want a refusal", name, path, code)
		}
	}
	if _, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: filepath.Join(f.root, "prior-art")}); !strings.Contains(refused, "link or junction") {
		t.Errorf("a tree holding a junction: %q", refused)
	}
	os.Remove(junction)
	// The size limit: a push over MaxPush, or one that would leave under PushReserve free, is refused
	// and leaves nothing staged; at the limit exactly it passes (the control).
	f.broker.MaxPush = 2
	if _, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: file}); !strings.Contains(refused, "too large") {
		t.Errorf("a 3-byte push over a 2-byte limit: %q", refused)
	}
	if _, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: tree}); refused != "" {
		t.Errorf("a 1-byte tree under a 2-byte limit: %q", refused)
	}
	f.broker.MaxPush = 3
	if _, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: file}); refused != "" {
		t.Errorf("a 3-byte push at a 3-byte limit: %q", refused)
	}
	f.broker.MaxPush, f.broker.PushReserve = 0, 1<<62
	if _, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: file}); !strings.Contains(refused, "free") {
		t.Errorf("a push onto a volume under its reserve: %q", refused)
	}
	f.broker.PushReserve = 0
	if _, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: file + "::$DATA"}); !strings.Contains(refused, "stream") {
		t.Errorf("a push naming a data stream: %q", refused)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.stateDir, "staging")); len(entries) != 0 {
		t.Errorf("a refused push left staging behind: %v", entries)
	}
	f.fake.CopyErr = errors.New("copy broke")
	if _, _, refused := f.call(Request{Verb: VerbPush, Project: "proj", HostPath: file}); !strings.Contains(refused, "copy broke") {
		t.Errorf("push error: %q", refused)
	}
	if _, _, refused := f.call(Request{Verb: VerbPull, Project: "proj", Guest: `C:\out`}); !strings.Contains(refused, "copy broke") {
		t.Errorf("pull error: %q", refused)
	}
	f.fake.CopyErr = nil

	guestOut := filepath.Join(f.fake.GuestRoot, "C", "out")
	os.MkdirAll(filepath.Join(guestOut, "sub"), 0o755)
	os.WriteFile(filepath.Join(guestOut, "song.ogg"), []byte("ogg"), 0o644)
	os.WriteFile(filepath.Join(guestOut, "sub", "track.wav"), []byte("wav"), 0o644)
	// Over its limit a pull is refused and publishes nothing; at the default it passes (the control).
	f.broker.MaxPush = 4
	if _, _, refused := f.call(Request{Verb: VerbPull, Project: "proj", Guest: `C:\out`}); refused == "" {
		t.Error("a pull over its limit passed")
	}
	if published, _ := os.ReadDir(filepath.Join(f.root, LabOut)); len(published) != 0 {
		t.Errorf("a refused pull published %v", published)
	}
	if f.fake.LastMax != 4 {
		t.Errorf("pull gave the guest side a limit of %d, want the broker's 4", f.fake.LastMax)
	}
	f.broker.MaxPush = 0
	code, out, refused := f.call(Request{Verb: VerbPull, Project: "proj", Guest: `C:\out`})
	if code != 0 || !strings.Contains(out, "2 files") {
		t.Fatalf("pull: %d %q %q", code, out, refused)
	}
	published, _ := filepath.Glob(filepath.Join(f.root, LabOut, "20261121-120000-*", "out", "song.ogg"))
	if len(published) != 1 {
		t.Fatalf("pull did not publish one song.ogg: %v", published)
	}
	if zone, err := os.ReadFile(published[0] + ":Zone.Identifier"); err != nil || !strings.Contains(string(zone), "ZoneId=3") {
		t.Errorf("no Mark of the Web on %s: %v %q", published[0], err, zone)
	}
	// A planted junction at lab-out: nothing is published there.
	os.RemoveAll(filepath.Join(f.root, LabOut))
	elsewhere := t.TempDir()
	exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(f.root, LabOut), elsewhere).Run()
	if code, _, refused := f.call(Request{Verb: VerbPull, Project: "proj", Guest: `C:\out`}); code != 2 || !strings.Contains(refused, "junction") {
		t.Errorf("pull into a junction: %d %q", code, refused)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("pull wrote through the junction: %v", entries)
	}
}

// TestPushRaceSwap: a link swapped in after push's checks by name (the sandbox can write lab-in
// while a push runs) is caught when the object is opened: its final path is outside the root.
func TestPushRaceSwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	os.MkdirAll(filepath.Join(outside, "tree"), 0o755)
	os.WriteFile(filepath.Join(root, "plain.txt"), []byte("p"), 0o644)
	junction := filepath.Join(root, "swapped")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	final, err := finalPathOf(Self{}, root)
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	// The control: a plain file inside the root stages.
	budget := int64(1 << 20)
	if err := stageFromCaller(Self{}, final, filepath.Join(root, "plain.txt"), filepath.Join(stage, "plain.txt"), &budget); err != nil {
		t.Fatalf("a plain file: %v", err)
	}
	for _, name := range []string{"secret.txt", "tree"} {
		err := stageFromCaller(Self{}, final, filepath.Join(junction, name), filepath.Join(stage, name), &budget)
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("%s through a swapped-in junction: %v, want refused as outside", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stage, "secret.txt")); err == nil {
		t.Error("the file outside was staged")
	}
}

// TestExtractArchive: a guest's archive unpacks to plain files and directories only; links are left
// out and counted; a bad name, another kind of entry, passing the byte budget or the entry count
// refuses it; the controls unpack.
func TestExtractArchive(t *testing.T) {
	type entry struct {
		name string
		kind byte
		body string
	}
	build := func(entries []entry) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "a.tar")
		out, _ := os.Create(path)
		tw := tar.NewWriter(out)
		for _, e := range entries {
			header := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: 0o644, Size: int64(len(e.body))}
			if e.kind == tar.TypeSymlink {
				header.Linkname, header.Size = "/etc/passwd", 0
			}
			if e.kind == tar.TypeLink {
				header.Linkname = e.body
			}
			if e.kind != tar.TypeReg {
				header.Size = 0
			}
			if err := tw.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if e.kind == tar.TypeReg {
				tw.Write([]byte(e.body))
			}
		}
		tw.Close()
		out.Close()
		return path
	}
	good := build([]entry{{"./out/", tar.TypeDir, ""}, {"./out/song.ogg", tar.TypeReg, "ogg"}, {"./out/sub/track.wav", tar.TypeReg, "wav"},
		{"./out/link", tar.TypeSymlink, ""}, {"./out/CLAUDE.md.txt", tar.TypeReg, "x"}, {"./out/console.log", tar.TypeReg, ""}, {"./out/COM10", tar.TypeReg, ""}})
	destination := t.TempDir()
	files, links, err := extractArchive(good, destination, 1<<20, 100)
	if err != nil || files != 5 || links != 1 {
		t.Fatalf("a good archive: %d files, %d links, %v", files, links, err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "out", "sub", "track.wav")); err != nil || string(data) != "wav" {
		t.Errorf("track.wav: %q %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "out", "link")); err == nil {
		t.Error("a link was made")
	}
	for _, bad := range []string{"../x", "/abs", "./a/../../x", `a\b`, "c:x", "a//b", "out/CON", "nul.txt", "com1", "LPT9.log", "x.", "x ", "aux"} {
		if _, _, err := extractArchive(build([]entry{{bad, tar.TypeReg, "x"}}), t.TempDir(), 1<<20, 100); err == nil {
			t.Errorf("entry %q accepted", bad)
		}
	}
	if _, _, err := extractArchive(build([]entry{{"dev", tar.TypeChar, ""}}), t.TempDir(), 1<<20, 100); err == nil {
		t.Error("a device entry accepted")
	}
	if _, _, err := extractArchive(build([]entry{{"a", tar.TypeReg, "12345"}}), t.TempDir(), 4, 100); err == nil {
		t.Error("an archive over its budget accepted")
	}
	if _, _, err := extractArchive(build([]entry{{"a", tar.TypeReg, "1234"}}), t.TempDir(), 4, 100); err != nil {
		t.Errorf("an archive at its budget (the control): %v", err)
	}
	if _, _, err := extractArchive(build([]entry{{"a", tar.TypeReg, "1"}, {"b", tar.TypeReg, "1"}, {"c", tar.TypeReg, "1"}}), t.TempDir(), 100, 2); err == nil {
		t.Error("an archive over its entry count accepted")
	}
	if _, _, err := extractArchive(build([]entry{{"a", tar.TypeReg, "1"}, {"a", tar.TypeReg, "2"}}), t.TempDir(), 100, 10); err == nil {
		t.Error("an archive naming one file twice accepted")
	}
	// A hard link to a file unpacked before it is copied; one to anything else refuses the archive.
	hard := func(name, first string) entry { return entry{name: name, kind: tar.TypeLink, body: first} }
	linked := t.TempDir()
	if files, _, err := extractArchive(build([]entry{{"a", tar.TypeReg, "same"}, hard("b", "./a")}), linked, 100, 10); err != nil || files != 2 {
		t.Errorf("a hard link: %d files, %v", files, err)
	} else if data, _ := os.ReadFile(filepath.Join(linked, "b")); string(data) != "same" {
		t.Errorf("the hard link's copy holds %q", data)
	}
	for _, first := range []string{"../etc/passwd", "missing", "/abs"} {
		if _, _, err := extractArchive(build([]entry{{"a", tar.TypeReg, "x"}, hard("b", first)}), t.TempDir(), 100, 10); err == nil {
			t.Errorf("a hard link to %q accepted", first)
		}
	}
}

// TestLogFieldsCapped: a long argv element or path is cut in the log, and the cut is marked.
func TestLogFieldsCapped(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("x", LogFieldLimit+100)
	f.call(Request{Verb: VerbRun, Project: "proj", Argv: []string{"tool", long}, HostPath: long})
	text := f.log.String()
	if strings.Contains(text, long) || !strings.Contains(text, fmt.Sprintf("...(%d bytes)", len(long))) {
		t.Errorf("the log holds the whole field, or no mark of the cut")
	}
}

// TestClearStaging: what a stopped broker left in staging is removed; no staging at all is fine.
func TestClearStaging(t *testing.T) {
	dir := t.TempDir()
	if err := ClearStaging(dir); err != nil {
		t.Errorf("no staging: %v", err)
	}
	os.MkdirAll(filepath.Join(dir, "staging", "abc", "sub"), 0o700)
	os.WriteFile(filepath.Join(dir, "staging", "abc", "sub", "f"), []byte("x"), 0o600)
	if err := ClearStaging(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "staging")); len(entries) != 0 {
		t.Errorf("left: %v", entries)
	}
}

func TestMarkUntrustedRemovesLinks(t *testing.T) {
	directory := t.TempDir()
	os.WriteFile(filepath.Join(directory, "a.txt"), []byte("a"), 0o644)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(directory, "j"), os.TempDir()).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	// A file after the junction (WalkDir goes in name order) is marked too: removing a linked directory
	// once ended the whole walk (review 2026-11-21, A7).
	os.WriteFile(filepath.Join(directory, "k.txt"), []byte("k"), 0o644)
	files, links, err := markUntrusted(directory)
	if err != nil || files != 2 || links != 1 {
		t.Errorf("markUntrusted = %d files, %d links, %v; want 2, 1", files, links, err)
	}
	if zone, err := os.ReadFile(filepath.Join(directory, "k.txt") + ":Zone.Identifier"); err != nil || !strings.Contains(string(zone), "ZoneId=3") {
		t.Errorf("the file after the junction is not marked: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "j")); err == nil {
		t.Error("the junction is still there")
	}
}

func TestState(t *testing.T) {
	f := newFixture(t)
	if _, err := LoadState(f.stateDir, "proj-lab"); err != nil {
		t.Fatal(err)
	}
	good := f.state
	bad := map[string]func(*State){
		"vm":       func(s *State) { s.VM = "bad name" },
		"project":  func(s *State) { s.Project = "" },
		"hostname": func(s *State) { s.Hostname = "MUCH-TOO-LONG-NAME" },
		"sid":      func(s *State) { s.ClientSID = "S-1-5-18" },
		"disks":    func(s *State) { s.Disks = nil },
		"os":       func(s *State) { s.OSBase = "" },
		"cred":     func(s *State) { s.Credential = "" },
		"game":     func(s *State) { s.GameDiff = `C:\vm\game-diff.vhdx` },
		"switch":   func(s *State) { s.PrivateSwitch = "bad switch" },
		"os_diff":  func(s *State) { s.OSDiff = `C:\vm\other.vhdx` },
		"game_diff": func(s *State) {
			s.GameParent = &FileFact{Path: `D:\game.vhdx`}
			s.GameDiff = `C:\vm\game-diff.vhdx`
		},
	}
	for name, change := range bad {
		state := good
		change(&state)
		if state.Validate() == nil {
			t.Errorf("%s: an invalid state validated", name)
		}
	}
	if _, err := LoadState(f.stateDir, "bad name"); err == nil {
		t.Error("an invalid VM name loaded")
	}
	if _, err := LoadState(f.stateDir, "missing"); err == nil {
		t.Error("a missing state loaded")
	}
	os.WriteFile(filepath.Join(f.stateDir, "labs", "x.json"), []byte(`{"vm":"x","surprise":1}`), 0o644)
	if _, err := LoadState(f.stateDir, "x"); err == nil {
		t.Error("an unknown field loaded")
	}
	other := good
	other.VM = "other"
	text, _ := json.Marshal(other)
	os.WriteFile(filepath.Join(f.stateDir, "labs", "y.json"), text, 0o644)
	if _, err := LoadState(f.stateDir, "y"); err == nil {
		t.Error("a state naming another VM loaded")
	}
	invalid := good
	invalid.Hostname = ""
	f.writeState(t, invalid)
	if _, err := LoadState(f.stateDir, "proj-lab"); err == nil {
		t.Error("an invalid state loaded")
	}
	f.writeState(t, State{VM: "proj-lab", Project: "other", Hostname: "H", ClientSID: good.ClientSID, Disks: good.Disks,
		OSBase: good.OSBase, OSDiff: good.OSDiff, Credential: "c", GuestUser: "u"})
	if code, _, refused := f.call(Request{Verb: VerbStatus, Project: "proj"}); code != 2 || !strings.Contains(refused, "belongs to project other") {
		t.Errorf("a VM of another project: %d %q", code, refused)
	}
}

// testSID is the current account's SID, for a pipe this test may open.
func testSID(t *testing.T) string {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return account.Uid
}

var pipeCounter struct {
	sync.Mutex
	n int
}

func pipeName(t *testing.T) string {
	pipeCounter.Lock()
	defer pipeCounter.Unlock()
	pipeCounter.n++
	return fmt.Sprintf(`\\.\pipe\agent-containment-test-%d-%d`, time.Now().UnixNano(), pipeCounter.n)
}

func TestPipe(t *testing.T) {
	f := newFixture(t)
	name := pipeName(t)
	self, _ := os.Executable()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, name, PipeSDDL(testSID(t), testSID(t)), f.broker) }()
	time.Sleep(200 * time.Millisecond)

	var out, errs bytes.Buffer
	if code := Call(name, self, Request{Verb: VerbRun, Project: "proj", Argv: []string{"echo", "piped"}}, &out, &errs); code != 0 || !strings.Contains(out.String(), "ran echo piped") {
		t.Errorf("call: %d %q %q", code, out.String(), errs.String())
	}
	// Push and pull over the pipe: the broker reads and writes the host as the client (impersonation).
	file := filepath.Join(f.root, "prior-art", "piped.bin")
	os.WriteFile(file, []byte("p"), 0o644)
	if code := Call(name, self, Request{Verb: VerbPush, Project: "proj", HostPath: file}, &out, &errs); code != 0 {
		t.Errorf("push over the pipe: %d %q", code, errs.String())
	}
	os.MkdirAll(filepath.Join(f.fake.GuestRoot, "C", "res"), 0o755)
	os.WriteFile(filepath.Join(f.fake.GuestRoot, "C", "res", "r.txt"), []byte("r"), 0o644)
	if code := Call(name, self, Request{Verb: VerbPull, Project: "proj", Guest: `C:\res`}, &out, &errs); code != 0 {
		t.Errorf("pull over the pipe: %d %q", code, errs.String())
	}
	errs.Reset()
	if code := Call(name, self, Request{Verb: VerbRun, Project: "proj", Argv: []string{"stderr", "to-err"}}, &out, &errs); code != 0 || !strings.Contains(errs.String(), "to-err") {
		t.Errorf("stderr frame: %d %q", code, errs.String())
	}
	errs.Reset()
	if code := Call(name, self, Request{Verb: VerbRun, Project: "nope", Argv: []string{"x"}}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "not registered") {
		t.Errorf("refusal: %d %q", code, errs.String())
	}
	errs.Reset()
	if code := Call(name, `C:\Program Files\agent-containment\lab-broker.exe`, Request{Verb: VerbStatus, Project: "proj"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "refusing to talk to it") {
		t.Errorf("an unexpected server image: %d %q", code, errs.String())
	}
	if _, err := Listen(name, PipeSDDL(testSID(t), testSID(t)), true); err == nil || !strings.Contains(err.Error(), "another process") {
		t.Errorf("a second first instance: %v, want a refusal", err)
	}
	// A request the broker cannot read gets a refusal frame, and is logged.
	connection, err := Dial(name, self)
	if err != nil {
		t.Fatal(err)
	}
	connection.Write([]byte("{not json}\n"))
	var frame Frame
	json.NewDecoder(connection).Decode(&frame)
	connection.Close()
	if !strings.Contains(frame.Refused, "not one the broker reads") || !strings.Contains(f.log.String(), `"event":"unreadable"`) {
		t.Errorf("garbage request: %+v; log %s", frame, f.log.String())
	}
	// A client that sends nothing is disconnected at the deadline.
	// The evidence is the broker's: it logs the cut-off. Whether the client's next write fails depends
	// on timing (on the CI runner it can land in the pipe's buffer first).
	RequestDeadline = 100 * time.Millisecond
	before := strings.Count(f.log.String(), "no request read")
	connection, _ = Dial(name, self)
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(f.log.String(), "no request read") == before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	connection.Close()
	RequestDeadline = 30 * time.Second
	if strings.Count(f.log.String(), "no request read") == before {
		t.Errorf("the broker did not cut off a client that sent nothing; log %s", f.log.String())
	}
	// A client that goes away during a run that prints nothing cancels the run at the next check,
	// not at the run's timeout.
	ClientCheckEvery = 50 * time.Millisecond
	connection, _ = Dial(name, self)
	request, _ := json.Marshal(Request{Verb: VerbRun, Project: "proj", Argv: []string{"sleep"}, Timeout: 60})
	connection.Write(append(request, '\n'))
	time.Sleep(200 * time.Millisecond)
	connection.Close()
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(f.log.String(), "the client went away") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	ClientCheckEvery = 5 * time.Second
	if !strings.Contains(f.log.String(), "the client went away") {
		t.Errorf("a silent run outlived its client; log %s", f.log.String())
	}
	cancel()
	if connection, err := Dial(name, self); err == nil {
		connection.Close() // wakes Serve, which sees ctx is done
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Serve did not stop")
	}
	if code := Call(`\\.\pipe\agent-containment-test-nobody`, self, Request{}, &out, &errs); code != 2 {
		t.Error("a missing pipe did not refuse")
	}
	if _, err := Listen(pipeName(t), "not sddl", true); err == nil {
		t.Error("a bad security descriptor was accepted")
	}
	if !strings.Contains(PipeSDDL("S-1-5-21-1-2-3-4", "S-1-5-18"), "0x12019b;;;S-1-5-21-1-2-3-4") || strings.Contains(PipeSDDL("S-1-5-21-1-2-3-4", "S-1-5-18"), "GRGW") {
		t.Error("the client's pipe rights are not the explicit mask")
	}
}

func TestImpersonation(t *testing.T) {
	name := pipeName(t)
	accepted := make(chan *Connection, 1)
	go func() {
		connection, err := Listen(name, PipeSDDL(testSID(t), testSID(t)), true)
		if err != nil {
			t.Error(err)
			accepted <- nil
			return
		}
		accepted <- connection
	}()
	time.Sleep(100 * time.Millisecond)
	self, _ := os.Executable()
	client, err := Dial(name, self)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.Write([]byte("x\n"))
	server := <-accepted
	if server == nil {
		t.FailNow()
	}
	defer server.Close()
	buffer := make([]byte, 2)
	server.Read(buffer) // impersonation needs a read first
	ran := false
	if err := server.As(func() error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("As: %v, ran %v", err, ran)
	}
	if server.PID() != uint32(os.Getpid()) {
		t.Errorf("PID = %d, want this process %d", server.PID(), os.Getpid())
	}
	bad := &Connection{handle: 0}
	if err := bad.As(func() error { return nil }); err == nil {
		t.Error("impersonating through no pipe succeeded")
	}
}

func TestPowerShellPlumbing(t *testing.T) {
	encoded := encodeCommand("Write-Output 'é'")
	raw, _ := base64.StdEncoding.DecodeString(encoded)
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if string(utf16.Decode(units)) != "Write-Output 'é'" {
		t.Error("encodeCommand does not round-trip")
	}
	var out bytes.Buffer
	if err := powershell(context.Background(), `Write-Output ("got " + $in.word + " " + $in.list.Count)`, map[string]any{"word": "a'b;c", "list": []string{"x", "y"}}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "got a'b;c 2" {
		t.Errorf("LAB_INPUT did not arrive intact: %q", out.String())
	}
	if err := powershell(context.Background(), `throw 'broken on purpose'`, map[string]any{}, &out, nil); err == nil || !strings.Contains(err.Error(), "broken on purpose") {
		t.Errorf("a failing script: %v", err)
	}
	if _, err := encodeInput(map[string]any{"bad": make(chan int)}); err == nil {
		t.Error("unencodable input accepted")
	}
	if err := powershell(context.Background(), "", map[string]any{"bad": make(chan int)}, &out, nil); err == nil {
		t.Error("powershell with unencodable input ran")
	}
	info, err := parseInfo("warning\n" + `{"name":"v","state":"Running","adapters":[{"switch":"s","switch_type":"Private"}],"guest_service_interface":false,"enhanced_session":false,"gpu_partitions":1,"disks":[{"path":"C:\\a.vhdx","parent":"C:\\b.vhdx"}],"dvd_drives":0,"com_ports":[],"assigned_devices":0,"fibre_channel":0,"disks_free":9223372036854775807}`)
	if err != nil || info.GPUPartitions != 1 || len(info.Adapters) != 1 || info.Disks[0].Parent != `C:\b.vhdx` || info.DisksFree != 1<<63-1 {
		t.Errorf("parseInfo: %+v %v", info, err)
	}
	if _, err := parseInfo("not json"); err == nil {
		t.Error("parseInfo accepted garbage")
	}
	// A reset's new children are swapped in under paths already attached, where Hyper-V never granted
	// the VM's account: each must be granted before it is moved into place, or the VM cannot start
	// ("Account does not have permission to open attachment", 2026-11-23).
	grant, swap := strings.Index(resetScript, "icacls $fresh /grant"), strings.Index(resetScript, "Move-Item -LiteralPath $fresh")
	if !strings.Contains(resetScript, `'NT VIRTUAL MACHINE\' + (Get-VM -Name $in.vm).Id`) || grant < 0 || swap < grant {
		t.Errorf("resetScript does not grant the VM's account each new child before swapping it in:\n%s", resetScript)
	}
	// Every script the broker runs parses in Windows PowerShell 5.1 (a parse error shows only on the
	// real VM otherwise).
	scripts := map[string]string{"prelude": prelude, "info": infoScript, "start": startScript, "stop": stopScript,
		"probe": probeScript, "labJobType": labJobType, "guestChild": guestChild, "guestRun": guestRun,
		"guestStop": guestStop, "run": runScript, "stopRun": stopRunScript, "copyIn": copyInScript,
		"copyOut": copyOutScript, "reset": resetScript}
	directory := t.TempDir()
	var files []string
	for name, text := range scripts {
		file := filepath.Join(directory, name+".ps1")
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	check := `$bad = 0
foreach ($path in $env:LAB_PARSE -split '\|') {
  $tokens = $null; $errors = $null
  [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$errors) | Out-Null
  foreach ($e in $errors) { Write-Output ('{0} line {1}: {2}' -f $path, $e.Extent.StartLineNumber, $e.Message); $bad++ }
}
if ($bad) { exit 1 }
'parsed'`
	checker := filepath.Join(t.TempDir(), "check.ps1")
	if err := os.WriteFile(checker, []byte(check), 0o644); err != nil {
		t.Fatal(err)
	}
	parse := exec.Command(WindowsPowerShell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", checker)
	parse.Env = append(os.Environ(), "LAB_PARSE="+strings.Join(files, "|"))
	if output, err := parse.CombinedOutput(); err != nil || !strings.Contains(string(output), "parsed") {
		t.Errorf("the broker's scripts do not all parse (%v):\n%s", err, output)
	}
	// infoScript measures the disks' volumes; without disks_free the gate reads 0 and refuses.
	if !strings.Contains(infoScript, "disks_free") {
		t.Error("infoScript does not report disks_free")
	}
	probe, err := parseProbe(`{"hostname":"H","adapters_up":0,"reached":[],"dns":false,"loopback":true}`)
	if err != nil || !probe.Loopback {
		t.Errorf("parseProbe: %+v %v", probe, err)
	}
	if _, err := parseProbe(""); err == nil {
		t.Error("parseProbe accepted nothing")
	}
	// PowerShell's serialized error stream is reduced to its messages.
	clixml := `#< CLIXML` + "\r\n" + `<Objs Version="1.1.0.1"><S S="Error">The given path's format is not supported._x000D__x000A_</S><S S="Error">&quot;C:\x::$DATA&quot; &amp; more_x000D__x000A_</S><S S="Error">At line:1 char:5_x000D__x000A_</S></Objs>`
	if got := oneLine(clixml); got != `The given path's format is not supported. "C:\x::$DATA" & more` {
		t.Errorf("oneLine(CLIXML) = %q", got)
	}
	record := "C:\\out holds 1 links or junctions; pull a tree without them\r\nAt line:23 char:3\r\n+   $archive = Invoke-Command -Session $session ...\r\n    + CategoryInfo          : OperationStopped: (:) [], RuntimeException\r\n    + FullyQualifiedErrorId : x"
	if got := oneLine(record); got != "C:\\out holds 1 links or junctions; pull a tree without them" {
		t.Errorf("oneLine(error record) = %q", got)
	}
	if got := oneLine("plain\n  text"); got != "plain text" {
		t.Errorf("oneLine(plain) = %q", got)
	}
	var relayed bytes.Buffer
	if code, found := relayRun(strings.NewReader("one\ntwo\n__LAB_EXIT__ 9\nM 3\n"), &relayed, "M"); !found || code != 3 || relayed.String() != "one\ntwo\n__LAB_EXIT__ 9\n" {
		t.Errorf("relayRun = %d %v, %q", code, found, relayed.String())
	}
	if _, found := relayRun(strings.NewReader("no marker\n"), &relayed, "M"); found {
		t.Error("relayRun without a marker found one")
	}
	if code, found := relayRun(strings.NewReader("M -196608\n"), &relayed, "M"); !found || code != -196608 {
		t.Errorf("relayRun of a negative code = %d %v", code, found)
	}
	// A negative code is the program's, not a failure; only a missing marker is (review 2026-11-21).
	if code, err := runResult(-196608, true, nil); code != -196608 || err != nil {
		t.Errorf("runResult(-196608) = %d %v", code, err)
	}
	if _, err := runResult(0, false, nil); err == nil {
		t.Error("runResult without a marker succeeded")
	}
	if _, err := runResult(0, false, errors.New("powershell broke")); err == nil || !strings.Contains(err.Error(), "powershell broke") {
		t.Errorf("runResult keeps PowerShell's error: %v", err)
	}
}

// TestMain: with LAB_ECHO_ARGS=1 the test binary is a program that prints the arguments it received
// as one JSON line, for TestGuestArguments. ASCII only (\u escapes): PowerShell decodes a program's
// output with the console's code page.
func TestMain(m *testing.M) {
	// LAB_SLEEP=1: write this process's id to LAB_PIDFILE and sleep; LAB_SPAWN=1: start one such
	// child of its own, then sleep too (a run's program and what it starts, for TestGuestStop).
	if os.Getenv("LAB_SLEEP") == "1" {
		os.WriteFile(os.Getenv("LAB_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	}
	if os.Getenv("LAB_SPAWN") == "1" {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "LAB_SPAWN=0", "LAB_SLEEP=1")
		child.Start()
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	}
	if os.Getenv("LAB_ECHO_ARGS") == "1" {
		quoted := make([]string, len(os.Args)-1)
		for i, argument := range os.Args[1:] {
			quoted[i] = strconv.QuoteToASCII(argument)
		}
		fmt.Println("[" + strings.Join(quoted, ",") + "]")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestGuestArguments runs the guest side of a run (guestRun) through real Windows PowerShell 5.1,
// as the guest does, and checks each program receives exactly the arguments lab-run was given:
// quotes, backslashes before quotes, trailing backslashes, spaces, empty arguments, % signs and
// PowerShell's own syntax (2026-11-21: quotes were stripped on the way into the guest).
func TestGuestArguments(t *testing.T) {
	t.Setenv("LAB_ECHO_ARGS", "1")
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := ""
	var runFound func(argv []string) (string, int, bool)
	run := func(argv []string) (string, int) {
		t.Helper()
		output, code, found := runFound(argv)
		if !found {
			t.Errorf("%q: no exit marker; output %q", argv, output)
		}
		return output, code
	}
	runFound = func(argv []string) (string, int, bool) {
		t.Helper()
		var out bytes.Buffer
		marker := newMarker()
		script := "& {\n" + guestRun + "\n} $in.argv $in.cwd $in.line $in.marker $in.child $in.job 2>&1 | ForEach-Object { [Console]::Out.WriteLine(\"$_\") }"
		input := map[string]any{"argv": argv, "cwd": cwd, "line": commandLine(argv[1:]), "marker": marker, "child": encodeCommand(guestChild), "job": `Global\lab-run-` + marker}
		if err := powershell(context.Background(), script, input, &out, nil); err != nil {
			t.Fatalf("%q: %v", argv, err)
		}
		var relayed bytes.Buffer
		code, found := relayRun(&out, &relayed, marker)
		return strings.TrimSpace(relayed.String()), code, found
	}
	cases := [][]string{
		{"-NoProfile", "-Command", `"adapters: " + (1, 2).Count`},
		{`a"b`, `"`, `""`, `\`, `a\`, `a\\`, `a\"b`, `a\\"b`, `C:\path with space\`, `\\server\share\`},
		{"", "a b", "", "\t", "x\ty", "  "},
		{"%PATH%", "%LAB_ARGS%", "100%", "--%", "&", "|", "<a>", "^", "'single'", "$x", "@(1)", "`tick", ";"},
		{"ünïcødé", "日本"},
		{},
	}
	for _, args := range cases {
		output, code := run(append([]string{program}, args...))
		var got []string
		if err := json.Unmarshal([]byte(output), &got); err != nil || code != 0 {
			t.Errorf("%q: exit %d, output %q (%v)", args, code, output, err)
			continue
		}
		if len(got) != len(args) || (len(args) > 0 && strings.Join(got, "\x00") != strings.Join(args, "\x00")) {
			t.Errorf("sent %q\nthe program got %q", args, got)
		}
	}
	// A cmdlet still gets its arguments as PowerShell arguments; the exit marker reports success.
	if output, code := run([]string{"Write-Output", `a"b c`}); output != `a"b c` || code != 0 {
		t.Errorf("a cmdlet: %q exit %d", output, code)
	}
	// The exit code of a program comes back, also when it fails before printing anything (a -File that
	// does not exist: game's run, 2026-11-21).
	if _, code := run([]string{WindowsPowerShell, "-NoProfile", "-Command", "exit 7"}); code != 7 {
		t.Errorf("exit code: %d, want 7", code)
	}
	if output, code := run([]string{WindowsPowerShell, "-NoProfile", "-File", `C:\no\such\file.ps1`}); code == 0 {
		t.Errorf("a -File that does not exist: exit %d, output %q; want the program's nonzero code", code, output)
	}
	// A program that does not exist, and a working directory that does not exist, end with exit 1 and
	// the reason, never with the program run somewhere else (review 2026-11-21, #9).
	if output, code := run([]string{`C:\no\such\program.exe`}); code != 1 || !strings.Contains(output, "lab-run (guest)") {
		t.Errorf("a missing program: exit %d, output %q", code, output)
	}
	cwd = `C:\no\such\directory'; whoami; '`
	if output, code := run([]string{program, "x"}); code != 1 || !strings.Contains(output, "lab-run (guest)") || strings.Contains(output, `["x"]`) {
		t.Errorf("a missing working directory: exit %d, output %q", code, output)
	}
	cwd = os.TempDir()
	if output, code := run([]string{program, "x"}); code != 0 || output != `["x"]` {
		t.Errorf("an existing working directory (the control): exit %d, output %q", code, output)
	}
}

// TestGuestStop: a run's program and what it started are stopped together by the stop script (as a
// cancelled run's are, from a second PowerShell Direct session), and only they: the run itself still
// ends with its marker (game, 2026-11-21: a cancelled run's workers went on for 30 minutes).
func TestGuestStop(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("LAB_SPAWN", "1")
	t.Setenv("LAB_PIDFILE", pidFile)
	marker := newMarker()
	job := `Global\lab-run-` + marker
	done := make(chan string, 1)
	go func() {
		var out bytes.Buffer
		script := "& {\n" + guestRun + "\n} $in.argv $in.cwd $in.line $in.marker $in.child $in.job 2>&1 | ForEach-Object { [Console]::Out.WriteLine(\"$_\") }"
		powershell(context.Background(), script, map[string]any{"argv": []string{program}, "cwd": "", "line": "", "marker": marker,
			"child": encodeCommand(guestChild), "job": job}, &out, nil)
		done <- out.String()
	}()
	var pid string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && pid == ""; time.Sleep(200 * time.Millisecond) {
		data, _ := os.ReadFile(pidFile)
		pid = strings.TrimSpace(string(data))
	}
	if pid == "" {
		t.Fatal("the run's grandchild never started")
	}
	running := func() bool {
		out, _ := exec.Command("tasklist", "/FI", "PID eq "+pid, "/NH").Output()
		return strings.Contains(string(out), " "+pid+" ")
	}
	if !running() {
		t.Fatalf("process %s is not running before the stop (the control)", pid)
	}
	var out bytes.Buffer
	if err := powershell(context.Background(), "& {\n"+guestStop+"\n} $in.job", map[string]any{"job": job}, &out, nil); err != nil || !strings.Contains(out.String(), "stopped") {
		t.Fatalf("the stop script: %v %q", err, out.String())
	}
	select {
	case output := <-done:
		if !strings.Contains(output, marker+" ") {
			t.Errorf("the run did not end with its marker: %q", output)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not end after its job was stopped")
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && running(); time.Sleep(200 * time.Millisecond) {
	}
	if running() {
		t.Errorf("process %s, started by the run's program, outlived the stop", pid)
	}
	// Stopping a job that no longer exists says so.
	out.Reset()
	if err := powershell(context.Background(), "& {\n"+guestStop+"\n} $in.job", map[string]any{"job": job}, &out, nil); err != nil || !strings.Contains(out.String(), "none") {
		t.Errorf("stopping a finished run: %v %q", err, out.String())
	}
}

func TestHyperVErrors(t *testing.T) {
	// Without a lab state every call fails before PowerShell, and without the VM every call fails
	// in PowerShell (on a runner without Hyper-V, at Get-VM). Both must be errors, never success.
	missing := HyperV{States: func(string) (State, error) { return State{}, errors.New("no state") }}
	ctx := context.Background()
	var out bytes.Buffer
	if err := missing.Start(ctx, "x"); err == nil {
		t.Error("Start without state")
	}
	if _, err := missing.Probe(ctx, "x", nil); err == nil {
		t.Error("Probe without state")
	}
	if _, err := missing.Run(ctx, "x", []string{"a"}, "", &out, &out); err == nil {
		t.Error("Run without state")
	}
	if err := missing.CopyIn(ctx, "x", "a", "b"); err == nil {
		t.Error("CopyIn without state")
	}
	if err := missing.CopyOut(ctx, "x", "a", "b", 1); err == nil {
		t.Error("CopyOut without state")
	}
	if err := missing.Reset(ctx, "x", State{GameParent: &FileFact{Path: "p"}, GameDiff: "d"}); err == nil {
		t.Error("Reset without state")
	}
	ghost := HyperV{States: func(vm string) (State, error) {
		return State{VM: vm, Credential: `C:\nonexistent\cred`, GuestUser: "u"}, nil
	}}
	if _, err := ghost.Info(ctx, "agent-containment-no-such-vm"); err == nil {
		t.Error("Info of a VM that does not exist succeeded")
	}
	if _, err := ghost.Probe(ctx, "agent-containment-no-such-vm", []string{"1.1.1.1:443"}); err == nil {
		t.Error("Probe of a VM that does not exist succeeded")
	}
	if _, err := ghost.Run(ctx, "agent-containment-no-such-vm", []string{"hostname"}, "", &out, &out); err == nil {
		t.Error("Run in a VM that does not exist succeeded")
	}
}

// lockedBuffer is a log the broker's connection goroutines and the test may use at once.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// TestImageOfProcess: the image of a process is read without opening it, so a standard account can
// read a SYSTEM process's, as lab-run (the owner, not elevated) must for the broker. wininit.exe always runs
// as SYSTEM; run unelevated, opening it is denied, and this still answers.
func TestImageOfProcess(t *testing.T) {
	self, _ := os.Executable()
	if image, err := imageOfProcess(uint32(os.Getpid())); err != nil || !equalFoldPath(image, self) {
		t.Errorf("this process: %q %v, want %s", image, err, self)
	}
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq wininit.exe", "/FO", "CSV", "/NH").Output()
	fields := strings.Split(strings.TrimSpace(string(out)), ",")
	if err != nil || len(fields) < 2 {
		t.Fatalf("tasklist: %v %q", err, out)
	}
	id, _ := strconv.Atoi(strings.Trim(fields[1], `"`))
	want := filepath.Join(os.Getenv("SystemRoot"), "System32", "wininit.exe")
	if image, err := imageOfProcess(uint32(id)); err != nil || !equalFoldPath(image, want) {
		t.Errorf("wininit.exe (SYSTEM, process %d): %q %v, want %s", id, image, err, want)
	}
	if _, err := imageOfProcess(0xfffffff0); err == nil {
		t.Error("a process that does not exist has an image")
	}
}

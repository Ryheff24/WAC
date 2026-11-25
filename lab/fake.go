package lab

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake is a Hypervisor without Hyper-V, for the tests and for lab-broker built with
// -X main.hypervisorKind=fake (the suite's end-to-end runs). It reports a contained VM unless a field
// says otherwise. The guest's files live under GuestRoot: a guest path C:\x is GuestRoot\C\x.
type Fake struct {
	Info_     VMInfo
	InfoErr   error
	InfoFails int // the next InfoFails reads fail, then reads succeed again
	StartErr  error
	Probe_    ProbeResult
	ProbeErr  error
	RunErr    error
	CopyErr   error
	// LastMax is the byte limit the broker gave the last CopyOut.
	LastMax   int64
	ResetErr  error
	GuestRoot string
	Started   int
	// StartDelay makes Start take that long, so calls can overlap it.
	StartDelay time.Duration
	ResetCount int
	Stopped    int
	mu         sync.Mutex
}

// NewFake is a contained, running VM with hostname and the disks its state records.
func NewFake(vm, hostname string, state State, guestRoot string) *Fake {
	var disks []Disk
	for _, path := range state.Disks {
		disk := Disk{Path: path}
		switch {
		case equalFoldPath(path, state.OSDiff):
			disk.Parent = state.OSBase
		case state.GameDiff != "" && equalFoldPath(path, state.GameDiff):
			disk.Parent = state.GameParent.Path
		}
		disks = append(disks, disk)
	}
	return &Fake{
		Info_:     VMInfo{Name: vm, State: "Running", Disks: disks, DisksFree: 1 << 50},
		Probe_:    ProbeResult{Hostname: hostname, Loopback: true},
		GuestRoot: guestRoot,
	}
}

func (f *Fake) Info(context.Context, string) (VMInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.InfoFails > 0 {
		f.InfoFails--
		return VMInfo{}, fmt.Errorf("a transient Hyper-V error")
	}
	return f.Info_, f.InfoErr
}

// Update changes the fake under its lock, for a test that changes it while a call reads it.
func (f *Fake) Update(change func(*Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *Fake) Stop(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stopped++
	f.Info_.State = "Off"
	return nil
}

func (f *Fake) Start(context.Context, string) error {
	time.Sleep(f.StartDelay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Started++
	if f.StartErr == nil {
		f.Info_.State = "Running"
	}
	return f.StartErr
}

func (f *Fake) Probe(context.Context, string, []string) (ProbeResult, error) {
	return f.Probe_, f.ProbeErr
}

// Run echoes its argv. "exit N" exits N; "stderr TEXT" writes TEXT to stderr; "sleep" waits for ctx.
func (f *Fake) Run(ctx context.Context, _ string, argv []string, cwd string, stdout, stderr io.Writer) (int, error) {
	if f.RunErr != nil {
		return 0, f.RunErr
	}
	switch argv[0] {
	case "exit":
		code, _ := strconv.Atoi(argv[1])
		return code, nil
	case "stderr":
		fmt.Fprintln(stderr, strings.Join(argv[1:], " "))
		return 0, nil
	case "sleep":
		<-ctx.Done()
		return 0, ctx.Err()
	}
	fmt.Fprintf(stdout, "ran %s in %q\n", strings.Join(argv, " "), cwd)
	return 0, nil
}

func (f *Fake) guestPath(path string) string {
	volume := filepath.VolumeName(path)
	return filepath.Join(f.GuestRoot, strings.TrimSuffix(volume, ":"), strings.TrimPrefix(path, volume))
}

func (f *Fake) CopyIn(_ context.Context, _ string, hostPath, guestDirectory string) error {
	if f.CopyErr != nil {
		return f.CopyErr
	}
	return copyTree(hostPath, filepath.Join(f.guestPath(guestDirectory), filepath.Base(hostPath)))
}

func (f *Fake) CopyOut(_ context.Context, _ string, guestPath, hostArchive string, max int64) error {
	if f.CopyErr != nil {
		return f.CopyErr
	}
	f.LastMax = max
	return writeArchive(f.guestPath(guestPath), hostArchive, max)
}

// writeArchive packs source (its base name as the top entry) into the new tar file archive, as the
// guest's tar.exe does, refusing one over max bytes. Links are stored as links, for the tests.
func writeArchive(source, archive string, max int64) error {
	out, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(out)
	parent := filepath.Dir(source)
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(parent, path)
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(path)
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = "./" + filepath.ToSlash(relative)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			_, err = io.Copy(tw, in)
			return err
		}
		return nil
	})
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		if info, statErr := os.Stat(archive); statErr == nil && info.Size() > max {
			err = fmt.Errorf("the archive is %d bytes, over %d", info.Size(), max)
		}
	}
	return err
}

func (f *Fake) Reset(context.Context, string, State) error {
	f.ResetCount++
	return f.ResetErr
}

// copyTree copies a file or a directory tree.
func copyTree(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o644)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyTree(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

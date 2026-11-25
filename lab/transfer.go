package lab

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ryheff24/WAC/registry"
)

// Transfers never let SYSTEM touch a host path on the caller's say-so (review 2026-11-21: pull #1 and
// #2, push #2 and #3). Host paths are read and written as the caller (Caller.As), so a junction the
// caller planted leads only where the caller could go anyway. The privileged side works only in
// staging, a fresh directory under the broker's state directory, which only SYSTEM and Administrators
// can write.

// ClearStaging removes what a broker that was stopped mid-transfer left in staging. lab-broker serve
// calls it at start, before any call can be using staging (review 2026-11-21, A8).
func ClearStaging(stateDirectory string) error {
	entries, err := os.ReadDir(filepath.Join(stateDirectory, "staging"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(stateDirectory, "staging", entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// staging makes a fresh staging directory and returns it with its removal.
func (b *Broker) staging() (string, func(), error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, err
	}
	directory := filepath.Join(b.StateDir, "staging", hex.EncodeToString(suffix))
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		return "", nil, err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", nil, err
	}
	return directory, func() { os.RemoveAll(directory) }, nil
}

// push copies a file or directory from inside the project's root (its lab exchange) into the guest's
// inbox. Every object is checked as it is read, as the caller: inside the root, not a link or
// junction (descendants included). Only the staged copy reaches the guest.
//
// The sandboxed agent can write lab-in while a push runs, so a name checked plain could be swapped
// for a link before it is opened. Each object is therefore checked again after it is opened: the
// handle's final path (every link followed) must be inside the root's.
func (b *Broker) push(ctx context.Context, caller Caller, request Request, project registry.Project, state State, emit func(Frame) error) (int, string) {
	path := registry.Normalize(request.HostPath)
	if !filepath.IsAbs(path) || !registry.Within(project.Root, path) {
		return 0, "push: " + request.HostPath + " is not inside " + project.Root
	}
	// A colon after the drive names a stream (file::$DATA, file:name), not the file.
	if strings.Contains(path[2:], ":") {
		return 0, "push: " + request.HostPath + " names an alternate data stream; push the file itself"
	}
	if why := ancestorsPlain(caller, project.Root, filepath.Dir(path)); why != "" {
		return 0, "push: " + why
	}
	staging, remove, err := b.staging()
	if err != nil {
		return 0, "push: staging: " + err.Error()
	}
	defer remove()
	root, err := finalPathOf(caller, project.Root)
	if err != nil {
		return 0, "push: " + err.Error()
	}
	budget, err := b.transferBudget(staging, 1)
	if err != nil {
		return 0, "push: " + err.Error()
	}
	staged := filepath.Join(staging, filepath.Base(path))
	if err := stageFromCaller(caller, root, path, staged, &budget); err != nil {
		return 0, "push: " + err.Error()
	}
	if err := b.H.CopyIn(ctx, state.VM, staged, GuestInbox); err != nil {
		return 0, "push failed: " + err.Error()
	}
	_ = emit(Frame{Stream: "stdout", Data: fmt.Sprintf("pushed %s to %s\\%s\n", path, GuestInbox, filepath.Base(path))})
	return 0, ""
}

// stageFromCaller copies source (opened as the caller) to destination (written as the broker),
// refusing links, junctions, anything that is not a file or a directory, and anything whose opened
// handle is not inside root (a final path, from finalPathOf).
//
// budget is the bytes still allowed; a copy that would pass it is refused.
func stageFromCaller(caller Caller, root, source, destination string, budget *int64) error {
	var info os.FileInfo
	if err := caller.As(func() (err error) { info, err = os.Lstat(source); return }); err != nil {
		return err
	}
	if isReparse(info) {
		return errors.New(source + " is a link or junction")
	}
	if why := badName(filepath.Base(source)); why != "" {
		return errors.New(source + ": " + why)
	}
	switch {
	case info.IsDir():
		var entries []os.DirEntry
		err := caller.As(func() error {
			directory, err := openInside(root, source)
			if err != nil {
				return err
			}
			defer directory.Close()
			entries, err = directory.ReadDir(-1)
			return err
		})
		if err != nil {
			return err
		}
		if err := os.Mkdir(destination, 0o700); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := stageFromCaller(caller, root, filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()), budget); err != nil {
				return err
			}
		}
		return nil
	case info.Mode().IsRegular():
		var in *os.File
		if err := caller.As(func() (err error) { in, err = openFileInside(root, source); return }); err != nil {
			return err
		}
		defer in.Close()
		if opened, err := in.Stat(); err != nil || !opened.Mode().IsRegular() {
			return fmt.Errorf("%s changed while it was read (%v)", source, err)
		}
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		// One byte over the budget is enough to refuse; a sparse file of terabytes is never read out.
		n, err := io.Copy(out, io.LimitReader(in, *budget+1))
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err == nil && n > *budget {
			return fmt.Errorf("push is too large: it passes its limit at %s (a push stages at most %d GiB, and less when the disk holding the VMs is nearly full)", source, DefaultMaxPush>>30)
		}
		*budget -= n
		return err
	}
	return errors.New(source + " is not a file or a directory")
}

// pull copies a guest path as one tar archive into staging (the guest refuses links and an over-size
// tree; the host counts the bytes), unpacks it there (extractArchive: plain files and directories with
// safe names only, within the budget), marks every file as from the internet, and only then publishes
// it as the caller into a new directory under the project's lab-out. A failure publishes nothing.
// Mark of the Web records where the files came from; what may run on the host is decided by the WSL
// sandbox, not by the mark.
func (b *Broker) pull(ctx context.Context, caller Caller, request Request, project registry.Project, state State, emit func(Frame) error) (int, string) {
	if request.Guest == "" {
		return 0, "pull: give the guest path"
	}
	staging, remove, err := b.staging()
	if err != nil {
		return 0, "pull: staging: " + err.Error()
	}
	defer remove()
	// A pull is on the host volume three times over (the archive, the unpacked tree, lab-out).
	budget, err := b.transferBudget(staging, 3)
	if err != nil {
		return 0, "pull: " + err.Error()
	}
	archive := filepath.Join(staging, "pull.tar")
	if err := b.H.CopyOut(ctx, state.VM, request.Guest, archive, budget); err != nil {
		return 0, "pull failed: " + err.Error()
	}
	tree := filepath.Join(staging, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		return 0, "pull: " + err.Error()
	}
	files, links, err := extractArchive(archive, tree, budget, MaxPullEntries)
	if err != nil {
		return 0, "pull: unpacking the guest's archive: " + err.Error()
	}
	os.Remove(archive)
	if _, _, err := markUntrusted(tree); err != nil {
		return 0, "pull: marking the copy failed: " + err.Error()
	}
	labOut := project.Abs(LabOut)
	if why := ancestorsPlain(caller, project.Root, filepath.Dir(labOut)); why != "" {
		return 0, "pull: " + why
	}
	suffix := make([]byte, 3)
	rand.Read(suffix)
	destination := filepath.Join(labOut, b.now().UTC().Format("20060102-150405")+"-"+hex.EncodeToString(suffix))
	err = caller.As(func() error {
		if err := os.MkdirAll(labOut, 0o755); err != nil {
			return err
		}
		if info, err := os.Lstat(labOut); err != nil || isReparse(info) {
			return fmt.Errorf("%s is a link or junction, or unreadable (%v)", labOut, err)
		}
		return os.Mkdir(destination, 0o755) // exclusive: a planted name fails here
	})
	if err != nil {
		return 0, "pull: " + err.Error()
	}
	if err := publishAsCaller(caller, tree, destination); err != nil {
		_ = caller.As(func() error { return os.RemoveAll(destination) })
		return 0, "pull: publishing failed, nothing was kept: " + err.Error()
	}
	note := ""
	if links > 0 {
		note = fmt.Sprintf(" (%d links in the archive left out)", links)
	}
	_ = emit(Frame{Stream: "stdout", Data: fmt.Sprintf("pulled %s to %s: %d files, marked as from the internet%s\n", request.Guest, destination, files, note)})
	return 0, ""
}

// MaxPullEntries bounds the files and directories one pull unpacks.
const MaxPullEntries = 200000

// extractArchive unpacks the tar file archive into the empty directory destination. Only directories
// and regular files are made; links are counted and left out; any other kind of entry, a name that is
// not a plain relative path (absolute, "..", a backslash, a colon), a Windows device name (CON, NUL,
// COM1, ...) or a name ending in a dot or space refuses the whole archive, as does passing budget bytes
// or maxEntries entries.
func extractArchive(archive, destination string, budget int64, maxEntries int) (files, links int, err error) {
	in, err := os.Open(archive)
	if err != nil {
		return 0, 0, err
	}
	defer in.Close()
	reader := tar.NewReader(in)
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files, links, nil
		}
		if err != nil {
			return files, links, err
		}
		if entries++; entries > maxEntries {
			return files, links, fmt.Errorf("more than %d entries", maxEntries)
		}
		name := strings.TrimPrefix(header.Name, "./")
		name = strings.TrimSuffix(name, "/")
		if name == "" || name == "." {
			continue
		}
		if why := archiveName(name); why != "" {
			return files, links, fmt.Errorf("entry %q: %s", header.Name, why)
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return files, links, err
			}
		case tar.TypeReg:
			if header.Size > budget {
				return files, links, fmt.Errorf("the files pass the limit at %q", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return files, links, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return files, links, err
			}
			n, err := io.Copy(out, io.LimitReader(reader, budget+1))
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return files, links, err
			}
			if n > budget {
				return files, links, fmt.Errorf("the files pass the limit at %q", header.Name)
			}
			budget -= n
			files++
		case tar.TypeLink:
			// tar stores a file's second hard link as a reference to the first: copy that file again.
			first := strings.TrimSuffix(strings.TrimPrefix(header.Linkname, "./"), "/")
			if why := archiveName(first); why != "" {
				return files, links, fmt.Errorf("entry %q links to %q: %s", header.Name, header.Linkname, why)
			}
			n, err := copyStaged(filepath.Join(destination, filepath.FromSlash(first)), target, budget)
			if err != nil {
				return files, links, fmt.Errorf("entry %q: %w", header.Name, err)
			}
			budget -= n
			files++
		case tar.TypeSymlink:
			links++
		default:
			return files, links, fmt.Errorf("entry %q is not a file or a directory (type %q)", header.Name, header.Typeflag)
		}
	}
}

// copyStaged copies the regular file source (already unpacked) to the new file target, within budget.
func copyStaged(source, target string, budget int64) (int64, error) {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return 0, fmt.Errorf("its first copy %s is not a file unpacked before it", source)
	}
	if info.Size() > budget {
		return 0, errors.New("the files pass the limit")
	}
	in, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

// archiveName is why name (an archive entry's relative path, / separated) is refused, or "".
func archiveName(name string) string {
	if strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") {
		return "not a plain relative path"
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "not a plain relative path"
		}
		if why := badName(part); why != "" {
			return why
		}
	}
	return ""
}

// badName is why a single file name cannot be used on Windows as given, or "": a device name (CON,
// PRN, AUX, NUL, COM0-9, LPT0-9, with any extension, any case), which opening as a file reaches the
// device instead, or a trailing dot or space, which Windows strips (so the name means another file).
func badName(name string) string {
	// filepath.IsLocal knows Windows' reserved names, the superscript COM¹ forms included.
	if !filepath.IsLocal(name) || strings.ContainsAny(name, `\/`) {
		return name + " is not a plain file name on Windows"
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "a name ending in a dot or space is another file's name on Windows"
	}
	stem := strings.ToUpper(name)
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.TrimRight(stem, " ")
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return name + " is a Windows device name"
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9' {
		return name + " is a Windows device name"
	}
	return ""
}

// publishAsCaller copies a staged tree (read as the broker) into destination, creating every file and
// directory as the caller, with its Mark of the Web.
func publishAsCaller(caller Caller, staged, destination string) error {
	return filepath.WalkDir(staged, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(staged, path)
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		if info, err := os.Lstat(path); err != nil || isReparse(info) {
			return fmt.Errorf("%s is a link or junction, or unreadable (%v)", path, err)
		}
		if entry.IsDir() {
			return caller.As(func() error { return os.Mkdir(target, 0o755) })
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		return caller.As(func() error {
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, in)
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
			return os.WriteFile(target+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644)
		})
	})
}

// transferBudget is the bytes a transfer may stage: MaxPush, or less if the staging volume would keep
// under PushReserve free once copies (how many times the bytes land on it) are written.
func (b *Broker) transferBudget(staging string, copies int64) (int64, error) {
	limit := b.MaxPush
	if limit <= 0 {
		limit = DefaultMaxPush
	}
	reserve := b.PushReserve
	if reserve <= 0 {
		reserve = DefaultPushReserve
	}
	free, err := freeBytes(staging)
	if err != nil {
		return 0, fmt.Errorf("free space of %s: %w", staging, err)
	}
	if (free-reserve)/copies < limit {
		limit = (free - reserve) / copies
	}
	if limit <= 0 {
		return 0, fmt.Errorf("the staging volume has %d bytes free, under the %d kept for the VMs' disks", free, reserve)
	}
	return limit, nil
}

// openInside opens path and returns it only if its final path is inside root (itself a final path).
func openInside(root, path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	final, err := finalPath(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("%s: its final path cannot be read: %w", path, err)
	}
	if !registry.Within(root, final) {
		file.Close()
		return nil, fmt.Errorf("%s leads to %s, outside %s (a link swapped in?)", path, final, root)
	}
	return file, nil
}

// openFileInside opens the regular file path without following a link at its last name (a link
// swapped in after the checks is refused, not followed: review 2026-11-21, C4), then requires its
// final path inside root as openInside does.
func openFileInside(root, path string) (*os.File, error) {
	file, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	final, err := finalPath(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("%s: its final path cannot be read: %w", path, err)
	}
	if !registry.Within(root, final) {
		file.Close()
		return nil, fmt.Errorf("%s leads to %s, outside %s (a link swapped in?)", path, final, root)
	}
	return file, nil
}

// finalPathOf is path's final path, every link followed, opened as the caller.
func finalPathOf(caller Caller, path string) (string, error) {
	var final string
	err := caller.As(func() error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		final, err = finalPath(file)
		return err
	})
	return final, err
}

// ancestorsPlain refuses when any existing directory from root down to directory is a link or
// junction, checked as the caller.
func ancestorsPlain(caller Caller, root, directory string) string {
	root = registry.Normalize(root)
	current := registry.Normalize(directory)
	for {
		var info os.FileInfo
		err := caller.As(func() (err error) { info, err = os.Lstat(current); return })
		if err == nil && isReparse(info) {
			return current + " is a link or junction"
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err.Error()
		}
		if strings.EqualFold(current, root) || current == filepath.Dir(current) {
			return ""
		}
		current = filepath.Dir(current)
	}
}

// markUntrusted walks a staged copy: it deletes links (WalkDir does not follow them) and writes a
// Zone.Identifier stream (ZoneId=3, Internet) on every file.
func markUntrusted(directory string) (int, int, error) {
	files, links := 0, 0
	err := filepath.WalkDir(directory, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if isReparse(info) {
			links++
			if err := os.Remove(p); err != nil {
				return err
			}
			if entry.IsDir() {
				return fs.SkipDir // skips only this entry: it is gone
			}
			return nil
		}
		if entry.Type().IsRegular() {
			files++
			return os.WriteFile(p+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644)
		}
		return nil
	})
	return files, links, err
}

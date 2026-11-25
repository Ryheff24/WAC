package lab

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procGetFinalPathNameByHandle = kernel32.NewProc("GetFinalPathNameByHandleW")
	procGetDiskFreeSpaceEx       = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// freeBytes is the space free to this process on the volume holding directory.
func freeBytes(directory string) (int64, error) {
	pointer, err := syscall.UTF16PtrFromString(directory)
	if err != nil {
		return 0, err
	}
	var available uint64
	if r, _, err := procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(pointer)), uintptr(unsafe.Pointer(&available)), 0, 0); r == 0 {
		return 0, err
	}
	return int64(available), nil
}

const fileAttributeReparsePoint = 0x400

// isReparse reports whether info is a reparse point: a symbolic link, junction or mount point.
func isReparse(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return data.FileAttributes&fileAttributeReparsePoint != 0
	}
	return false
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// equalFoldPath compares two Windows paths after cleaning, without case.
func equalFoldPath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// finalPath is the path of the file an open handle refers to, every link and junction followed, in
// the C:\... form (VOLUME_NAME_DOS, the \\?\ prefix removed). A file reached over a network share
// keeps its \\?\UNC\ form, which is inside no project root.
func finalPath(file *os.File) (string, error) {
	buffer := make([]uint16, 512)
	for {
		n, _, err := procGetFinalPathNameByHandle.Call(file.Fd(), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0)
		if n == 0 {
			return "", err
		}
		if int(n) < len(buffer) {
			path := syscall.UTF16ToString(buffer[:n])
			if strings.HasPrefix(path, `\\?\UNC\`) {
				return path, nil
			}
			return strings.TrimPrefix(path, `\\?\`), nil
		}
		buffer = make([]uint16, n+1)
	}
}

const (
	fileFlagOpenReparsePoint = 0x00200000
	fileFlagBackupSemantics  = 0x02000000
)

// openNoFollow opens path for reading without following a link or junction at its last name, and
// refuses one.
func openNoFollow(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, fileFlagOpenReparsePoint|fileFlagBackupSemantics, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		syscall.CloseHandle(handle)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		syscall.CloseHandle(handle)
		return nil, errors.New(path + " is a link or junction")
	}
	return os.NewFile(uintptr(handle), path), nil
}

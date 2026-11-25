package lab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Standard library only: the few Windows calls a named pipe server needs, through syscall.
var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	advapi32                     = syscall.NewLazyDLL("advapi32.dll")
	procCreateNamedPipe          = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe         = kernel32.NewProc("ConnectNamedPipe")
	procDisconnectNamedPipe      = kernel32.NewProc("DisconnectNamedPipe")
	procFlushFileBuffers         = kernel32.NewProc("FlushFileBuffers")
	procLocalFree                = kernel32.NewProc("LocalFree")
	procConvertStringSDToSD      = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procImpersonate              = advapi32.NewProc("ImpersonateNamedPipeClient")
	procRevertToSelf             = advapi32.NewProc("RevertToSelf")
	procGetNamedPipeClientID     = kernel32.NewProc("GetNamedPipeClientProcessId")
	procGetNamedPipeServerID     = kernel32.NewProc("GetNamedPipeServerProcessId")
	procQueryDosDevice           = kernel32.NewProc("QueryDosDeviceW")
	procNtQuerySystemInformation = syscall.NewLazyDLL("ntdll.dll").NewProc("NtQuerySystemInformation")
	procWaitNamedPipe            = kernel32.NewProc("WaitNamedPipeW")
	procCancelIoEx               = kernel32.NewProc("CancelIoEx")
	procPeekNamedPipe            = kernel32.NewProc("PeekNamedPipe")
)

const (
	pipeAccessDuplex        = 0x3
	fileFlagFirstInstance   = 0x80000
	pipeTypeByte            = 0x0
	pipeRejectRemoteClients = 0x8
	pipeUnlimitedInstances  = 255
	errorPipeConnected      = syscall.Errno(535)
	errorPipeBusy           = syscall.Errno(231)
	errorNoData             = syscall.Errno(232)
)

// RequestDeadline bounds reading a request; WriteDeadline bounds delivering one frame. A client that
// stalls is disconnected, which cancels its call (review #6).
var (
	RequestDeadline = 30 * time.Second
	WriteDeadline   = 60 * time.Second
	// ClientCheckEvery is how often a call checks that its client is still connected, so a run that
	// prints nothing does not hold the VM until its timeout after the client is gone (review
	// 2026-11-21, main 41dada9).
	ClientCheckEvery = 5 * time.Second
)

// clientRights is what the client account may do on the pipe: read and write data, attributes and
// extended attributes, read the security descriptor, synchronize. Not FILE_CREATE_PIPE_INSTANCE (the
// same bit as FILE_APPEND_DATA, 0x4), which generic write would include (review #5).
const clientRights = "0x12019b"

// PipeSDDL is the pipe's security descriptor: full access for SYSTEM, Administrators and the server's
// own account (SYSTEM when installed; the suite's account in its builds), clientRights for the client
// account, nobody else, and nothing inherited (P). Creating each further instance needs
// FILE_CREATE_PIPE_INSTANCE, which only the full-access entries have.
func PipeSDDL(clientSID, serverSID string) string {
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + serverSID + ")(A;;" + clientRights + ";;;" + clientSID + ")"
}

// ProcessSID is this process's account SID.
func ProcessSID() (string, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String()
}

type securityAttributes struct {
	Length             uint32
	SecurityDescriptor uintptr
	InheritHandle      uint32
}

// instance creates one pipe instance. first fails if any process already holds the name.
func instance(name, sddl string, first bool) (syscall.Handle, error) {
	var descriptor uintptr
	sddlPointer, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	if r, _, err := procConvertStringSDToSD.Call(uintptr(unsafe.Pointer(sddlPointer)), 1, uintptr(unsafe.Pointer(&descriptor)), 0); r == 0 {
		return syscall.InvalidHandle, fmt.Errorf("security descriptor %q: %w", sddl, err)
	}
	defer procLocalFree.Call(descriptor)
	attributes := securityAttributes{Length: uint32(unsafe.Sizeof(securityAttributes{})), SecurityDescriptor: descriptor}
	namePointer, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	openMode := uintptr(pipeAccessDuplex)
	if first {
		openMode |= fileFlagFirstInstance
	}
	handle, _, err := procCreateNamedPipe.Call(uintptr(unsafe.Pointer(namePointer)), openMode,
		pipeTypeByte|pipeRejectRemoteClients, pipeUnlimitedInstances, 64<<10, 64<<10, 0, uintptr(unsafe.Pointer(&attributes)))
	if syscall.Handle(handle) == syscall.InvalidHandle {
		if first {
			return syscall.InvalidHandle, fmt.Errorf("create pipe %s: %w (is another process holding the name?)", name, err)
		}
		return syscall.InvalidHandle, fmt.Errorf("create pipe %s: %w", name, err)
	}
	return syscall.Handle(handle), nil
}

// connect waits for a client on handle.
func connect(handle syscall.Handle) error {
	if r, _, err := procConnectNamedPipe.Call(uintptr(handle), 0); r == 0 && !errors.Is(err, errorPipeConnected) {
		return err
	}
	return nil
}

// Listen creates one instance of the pipe and waits for a client. Serve uses instance and connect
// directly, to keep a spare instance; Listen is for one-off use.
func Listen(name, sddl string, first bool) (*Connection, error) {
	handle, err := instance(name, sddl, first)
	if err != nil {
		return nil, err
	}
	if err := connect(handle); err != nil {
		syscall.CloseHandle(handle)
		return nil, fmt.Errorf("connect pipe %s: %w", name, err)
	}
	return newConnection(handle, name), nil
}

// Connection is one client's connection, and the Caller for its request.
type Connection struct {
	*os.File
	handle syscall.Handle
	once   sync.Once
}

func newConnection(handle syscall.Handle, name string) *Connection {
	return &Connection{File: os.NewFile(uintptr(handle), name), handle: handle}
}

// Disconnect ends the client's side of the connection. It first cancels any read or write blocked on
// the handle (CancelIoEx), which a disconnect alone does not interrupt.
func (c *Connection) Disconnect() {
	procCancelIoEx.Call(uintptr(c.handle), 0)
	procDisconnectNamedPipe.Call(uintptr(c.handle))
}

func (c *Connection) Close() error {
	var err error
	c.once.Do(func() {
		procFlushFileBuffers.Call(uintptr(c.handle))
		c.Disconnect()
		err = c.File.Close()
	})
	return err
}

// As runs f on a locked OS thread impersonating the client. If reverting fails, the thread is never
// unlocked, so Go discards it rather than reuse it with the client's identity.
func (c *Connection) As(f func() error) error {
	runtime.LockOSThread()
	if r, _, err := procImpersonate.Call(uintptr(c.handle)); r == 0 {
		runtime.UnlockOSThread()
		return fmt.Errorf("impersonating the client: %w", err)
	}
	result := f()
	if r, _, err := procRevertToSelf.Call(); r == 0 {
		return fmt.Errorf("reverting from the client's identity failed (%v); this thread is discarded", err)
	}
	runtime.UnlockOSThread()
	return result
}

// PID is the client's process id, for the log.
func (c *Connection) PID() uint32 {
	var id uint32
	procGetNamedPipeClientID.Call(uintptr(c.handle), uintptr(unsafe.Pointer(&id)))
	return id
}

// Serve accepts connections until ctx ends, answering each with broker: one request per connection.
// A new instance exists before each connection is handed off, so the name is never without a server
// instance between connections (review #5).
func Serve(ctx context.Context, name, sddl string, broker *Broker) error {
	handle, err := instance(name, sddl, true)
	if err != nil {
		return err
	}
	for {
		if err := connect(handle); err != nil {
			// A client that opened and closed before the server waited for it leaves the instance at
			// ERROR_NO_DATA: disconnect it and wait again, rather than stop serving (review 2026-11-21, A5).
			if errors.Is(err, errorNoData) {
				procDisconnectNamedPipe.Call(uintptr(handle))
				continue
			}
			syscall.CloseHandle(handle)
			return fmt.Errorf("connect pipe %s: %w", name, err)
		}
		connection := newConnection(handle, name)
		if ctx.Err() != nil {
			connection.Close() // the connection that woke a stopping server
			return nil
		}
		next, err := instance(name, sddl, false)
		go Answer(ctx, connection, broker)
		if err != nil {
			return err
		}
		handle = next
	}
}

// Answer reads one request from connection, answers it and closes the connection. The read and each
// frame's write have deadlines.
func Answer(ctx context.Context, connection *Connection, broker *Broker) {
	defer connection.Close()
	stall := time.AfterFunc(RequestDeadline, connection.Disconnect)
	line, err := bufio.NewReader(io.LimitReader(connection, 1<<20)).ReadBytes('\n')
	stall.Stop()
	encoder := json.NewEncoder(connection)
	emit := func(frame Frame) error {
		timer := time.AfterFunc(WriteDeadline, connection.Disconnect)
		defer timer.Stop()
		return encoder.Encode(frame)
	}
	if err != nil && len(line) == 0 {
		broker.refuseUnread(connection, "no request read: "+err.Error(), emit)
		return
	}
	var request Request
	decoder := json.NewDecoder(bytesReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		broker.refuseUnread(connection, "the request is not one the broker reads: "+err.Error(), emit)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(ClientCheckEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !connection.connected() {
					cancel()
					return
				}
			}
		}
	}()
	broker.Handle(ctx, connection, request, emit)
}

// connected reports whether the client still holds its end: PeekNamedPipe fails (broken pipe) once
// the client closes it. It reads nothing.
func (c *Connection) connected() bool {
	var available uint32
	r, _, _ := procPeekNamedPipe.Call(uintptr(c.handle), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
	return r != 0
}

// refuseUnread logs and refuses a request that could not be read (review #8: malformed requests are
// recorded too).
func (b *Broker) refuseUnread(caller Caller, why string, emit func(Frame) error) {
	_ = b.record(map[string]any{"event": "unreadable", "time": b.now().UTC().Format(time.RFC3339Nano), "client_pid": caller.PID(), "refused": why})
	_ = emit(Frame{Refused: why})
}

// Dial opens the broker's pipe as a client, waiting up to five seconds while every instance is busy,
// and refuses a server that is not the expected program (serverImage, compared without case).
func Dial(name, serverImage string) (io.ReadWriteCloser, error) {
	namePointer, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var file *os.File
	for attempt := 0; ; attempt++ {
		file, err = os.OpenFile(name, os.O_RDWR, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, errorPipeBusy) || attempt >= 5 {
			return nil, fmt.Errorf("the lab broker is not reachable at %s (is the agent-containment lab-broker task running?): %w", name, err)
		}
		procWaitNamedPipe.Call(uintptr(unsafe.Pointer(namePointer)), 1000)
	}
	image, err := serverImageOf(syscall.Handle(file.Fd()))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("cannot tell which program serves %s: %w", name, err)
	}
	if !equalFoldPath(image, serverImage) {
		file.Close()
		return nil, fmt.Errorf("%s is served by %s, not %s: refusing to talk to it", name, image, serverImage)
	}
	return file, nil
}

// serverImageOf is the full path of the program serving the pipe handle.
func serverImageOf(handle syscall.Handle) (string, error) {
	var id uint32
	if r, _, err := procGetNamedPipeServerID.Call(uintptr(handle), uintptr(unsafe.Pointer(&id))); r == 0 {
		return "", err
	}
	return imageOfProcess(id)
}

// imageOfProcess is the full path of a process's program, read without opening the process:
// NtQuerySystemInformation(SystemProcessIdInformation) answers any account. Opening it
// (OpenProcess, then QueryFullProcessImageName) is denied to a standard account when the process runs
// as SYSTEM, as the broker does, so every agent call failed with "Access is denied" (tested as
// claude-agent, 2026-11-21).
func imageOfProcess(id uint32) (string, error) {
	buffer := make([]uint16, 32768)
	info := systemProcessIDInformation{ProcessID: uintptr(id)}
	info.ImageName.MaximumLength = uint16(len(buffer)*2 - 2)
	info.ImageName.Buffer = &buffer[0]
	if status, _, _ := procNtQuerySystemInformation.Call(systemProcessIDInformationClass, uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info), 0); status != 0 {
		return "", fmt.Errorf("NtQuerySystemInformation(SystemProcessIdInformation) for process %d: status 0x%08x", id, uint32(status))
	}
	return dosPath(syscall.UTF16ToString(buffer[:info.ImageName.Length/2]))
}

const systemProcessIDInformationClass = 88

type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

type systemProcessIDInformation struct {
	ProcessID uintptr
	ImageName unicodeString
}

// dosPath turns a device path (\Device\HarddiskVolume3\Program Files\x.exe) into a drive-letter path
// (C:\Program Files\x.exe), through the drive letters' device names (QueryDosDevice).
func dosPath(device string) (string, error) {
	for letter := 'A'; letter <= 'Z'; letter++ {
		drive := string(letter) + ":"
		drivePointer, _ := syscall.UTF16PtrFromString(drive)
		target := make([]uint16, 1024)
		if r, _, _ := procQueryDosDevice.Call(uintptr(unsafe.Pointer(drivePointer)), uintptr(unsafe.Pointer(&target[0])), uintptr(len(target))); r == 0 {
			continue
		}
		prefix := syscall.UTF16ToString(target)
		if prefix != "" && len(device) > len(prefix) && strings.EqualFold(device[:len(prefix)], prefix) && device[len(prefix)] == '\\' {
			return drive + device[len(prefix):], nil
		}
	}
	return "", fmt.Errorf("no drive letter for %s", device)
}

// Call sends request on a new connection to a server running serverImage and relays the frames:
// stream data to stdout and stderr, a refusal to stderr. It returns the exit code (2 for a refusal
// or a broken reply).
func Call(name, serverImage string, request Request, stdout, stderr io.Writer) int {
	connection, err := Dial(name, serverImage)
	if err != nil {
		fmt.Fprintln(stderr, "lab-run: "+err.Error())
		return 2
	}
	defer connection.Close()
	line, _ := json.Marshal(request)
	if _, err := connection.Write(append(line, '\n')); err != nil {
		fmt.Fprintln(stderr, "lab-run: sending the request: "+err.Error())
		return 2
	}
	decoder := json.NewDecoder(connection)
	for {
		var frame Frame
		if err := decoder.Decode(&frame); err != nil {
			fmt.Fprintln(stderr, "lab-run: the broker's reply ended without an exit code: "+err.Error())
			return 2
		}
		switch {
		case frame.Refused != "":
			fmt.Fprintln(stderr, "lab-run: "+frame.Refused)
			return 2
		case frame.Exit != nil:
			return *frame.Exit
		case frame.Stream == "stderr":
			io.WriteString(stderr, frame.Data)
		default:
			io.WriteString(stdout, frame.Data)
		}
	}
}

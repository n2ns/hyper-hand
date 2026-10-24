package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

// launch starts a program detached from the request: not in the exec job object and with no captured stdio, so it
// outlives the request. Admin starts it elevated through the admin worker. AUMID activates a packaged application
// instead, with Args joined by single spaces, unquoted, as its activation arguments.
func launch(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.LaunchArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	if a.AUMID != "" {
		if a.Path != "" || a.Cwd != "" || a.Admin {
			return nil, nil, errors.New("launch takes aumid or path/cwd/admin, not both")
		}
		pid, err := activatePackagedApp(a.AUMID, strings.Join(a.Args, " "))
		if err != nil {
			return nil, nil, err
		}
		return proto.LaunchResult{PID: pid}, nil, nil
	}
	if a.Path == "" {
		return nil, nil, errors.New("launch requires a path")
	}
	if a.Admin {
		return launchAdminProcess(ctx, a)
	}
	pid, err := startDetached(a)
	if err != nil {
		return nil, nil, err
	}
	return proto.LaunchResult{PID: pid}, nil, nil
}

var pGetApplicationUserModelId = kernel32.NewProc("GetApplicationUserModelId")

const (
	activateOptionsNoErrorUI = 0x2 // AO_NOERRORUI: no error dialog in the guest when activation fails
	appModelIDMaxLength      = 130 // APPLICATION_USER_MODEL_ID_MAX_LENGTH, including the terminating null
)

// activatePackagedApp activates the packaged (UWP/MSIX) application aumid with arguments through
// IApplicationActivationManager::ActivateApplication and returns the PID of its process. An application that is
// already running is brought to the front and its existing PID returned.
func activatePackagedApp(aumid, arguments string) (uint32, error) {
	if !packagedAUMID(aumid) {
		return 0, fmt.Errorf("aumid %q is not a packaged application's ID (PackageFamilyName!ApplicationId); take launch.aumid from vm_apps", aumid)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		var oe *ole.OleError
		if !errors.As(err, &oe) || oe.Code() != 1 { // S_FALSE: already initialized on this thread
			return 0, err
		}
	}
	defer ole.CoUninitialize()
	manager, err := ole.CreateInstance(ole.NewGUID("{45BA127D-10A8-46EA-8AB7-56EA9078943C}"), ole.NewGUID("{2E941141-7F97-4756-BA1D-9DECDE894A3D}"))
	if err != nil {
		return 0, fmt.Errorf("ApplicationActivationManager: %w", err)
	}
	defer manager.Release()
	id, err := windows.UTF16PtrFromString(aumid)
	if err != nil {
		return 0, err
	}
	var args *uint16
	if arguments != "" {
		if args, err = windows.UTF16PtrFromString(arguments); err != nil {
			return 0, err
		}
	}
	var pid uint32
	// IApplicationActivationManager::ActivateApplication is vtable slot 3, after IUnknown's three methods.
	method := *(*uintptr)(unsafe.Add(unsafe.Pointer(manager.RawVTable), 3*unsafe.Sizeof(uintptr(0))))
	hr, _, _ := syscall.SyscallN(method, uintptr(unsafe.Pointer(manager)), uintptr(unsafe.Pointer(id)), uintptr(unsafe.Pointer(args)), activateOptionsNoErrorUI, uintptr(unsafe.Pointer(&pid)))
	runtime.KeepAlive(manager)
	runtime.KeepAlive(id)
	runtime.KeepAlive(args)
	if int32(hr) < 0 {
		if uint32(hr)>>16&0x1fff == 7 { // FACILITY_WIN32: the low word is a Win32 error code
			return 0, fmt.Errorf("ActivateApplication %s: HRESULT 0x%08x: %v", aumid, uint32(hr), syscall.Errno(uint32(hr)&0xffff))
		}
		return 0, fmt.Errorf("ActivateApplication %s: HRESULT 0x%08x", aumid, uint32(hr))
	}
	return pid, nil
}

// processAUMID is the Application User Model ID of a packaged process (GetApplicationUserModelId), "" for a process
// without package identity or when it cannot be read. p needs PROCESS_QUERY_LIMITED_INFORMATION.
func processAUMID(p windows.Handle) string {
	buf := make([]uint16, appModelIDMaxLength+1)
	n := uint32(len(buf))
	if r, _, _ := pGetApplicationUserModelId.Call(uintptr(p), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 || n == 0 || int(n) > len(buf) {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// startDetached starts a.Path with a.Args in a.Cwd and releases the process handle at once.
func startDetached(a proto.LaunchArgs) (uint32, error) {
	cmd := exec.Command(a.Path, a.Args...)
	cmd.Dir = a.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := uint32(cmd.Process.Pid)
	cmd.Process.Release()
	return pid, nil
}

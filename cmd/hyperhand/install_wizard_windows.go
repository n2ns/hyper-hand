package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// installWizard is the synchronous, dialog-free bridge used by the setup wizard.
// The wizard starts without elevation, so owner is captured before UAC can change
// the account. Only the child installs; the wizard waits for its actual result.
func installWizard(elevated bool) error {
	owner, err := setupOwner(nil)
	if err != nil {
		return err
	}
	ownerArgs := []string{"--owner-sid", owner.SID, "--owner-user", owner.User}
	if elevated {
		_, err := runSetup("install", ownerArgs)
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "hyperhand-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	worker, err := stageWizardInstall(self, dir)
	if err != nil {
		return err
	}
	args := append([]string{"install", "--quiet"}, ownerArgs...)
	for i, arg := range args {
		args[i] = syscall.EscapeArg(arg)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err != nil {
		return err
	}
	defer windows.CoUninitialize()
	process, err := startWizardInstall(worker, strings.Join(args, " "))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return waitWizardInstall(process)
}

// The installer stops other processes running its source image. A separate image
// keeps that from stopping the wizard's waiting parent. Release builds embed the
// agent, so the worker needs no files beside it.
func stageWizardInstall(self, dir string) (string, error) {
	data, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	worker := filepath.Join(dir, "hyperhand-setup-worker.exe")
	if err := os.WriteFile(worker, data, 0o755); err != nil {
		return "", err
	}
	return worker, nil
}

// wizardShellExecuteInfo is SHELLEXECUTEINFOW.
type wizardShellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIcon        uintptr
	hProcess     windows.Handle
}

func startWizardInstall(file, params string) (windows.Handle, error) {
	sei := wizardShellExecuteInfo{
		fMask:        0x40 | 0x100 | 0x400, // NOCLOSEPROCESS | NOASYNC | FLAG_NO_UI
		lpVerb:       windows.StringToUTF16Ptr("runas"),
		lpFile:       windows.StringToUTF16Ptr(file),
		lpParameters: windows.StringToUTF16Ptr(params),
		nShow:        windows.SW_HIDE,
	}
	sei.cbSize = uint32(unsafe.Sizeof(sei))
	proc := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
	if r, _, err := proc.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		return 0, fmt.Errorf("start elevated installation: %w", err)
	}
	if sei.hProcess == 0 {
		return 0, fmt.Errorf("elevated installation returned no process handle")
	}
	return sei.hProcess, nil
}

func waitWizardInstall(process windows.Handle) error {
	state, err := windows.WaitForSingleObject(process, windows.INFINITE)
	if err != nil {
		return fmt.Errorf("wait for elevated installation: %w", err)
	}
	if state != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("unexpected installation wait result: %d", state)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return fmt.Errorf("read installation exit code: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("elevated installation failed (exit code %d); see the installing administrator's HyperHand log", code)
	}
	return nil
}

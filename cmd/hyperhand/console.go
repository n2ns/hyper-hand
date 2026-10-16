package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"hyperhand/internal/broker"
	"hyperhand/internal/hyperv"
)

// consoleTask is registered by install: it runs the fixed console helper with the owner's full token, because
// VMConnect needs Hyper-V rights that the owner's filtered (non-elevated) token does not have.
const consoleTask = "HyperHand Console"

// openConsole brings vm's open VMConnect window to the front, or else opens VMConnect for vm through consoleTask,
// without a UAC prompt. A second VMConnect for the same VM would ask to take over the console connection.
func openConsole(vm string) error {
	return requestConsole(vm, "open")
}

func requestConsole(vm, mode string) error {
	vms, err := (&broker.Client{}).ListVMs()
	if err != nil {
		return err
	}
	if err := checkConsoleVM(vm, vms); err != nil {
		return err
	}
	var id string
	for _, v := range vms {
		if v.Name == vm {
			id = v.ID
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 { // S_FALSE = already initialized
			return err
		}
	}
	defer ole.CoUninitialize()
	unk, err := oleutil.CreateObject("Schedule.Service")
	if err != nil {
		return err
	}
	defer unk.Release()
	svc, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer svc.Release()
	if _, err := oleutil.CallMethod(svc, "Connect"); err != nil {
		return err
	}
	folder, err := oleutil.CallMethod(svc, "GetFolder", `\`)
	if err != nil {
		return err
	}
	defer folder.Clear()
	task, err := oleutil.CallMethod(folder.ToIDispatch(), "GetTask", consoleTask)
	if err != nil {
		return fmt.Errorf("the %q task is missing; run hyperhand.exe install again: %w", consoleTask, err)
	}
	defer task.Clear()
	run, err := oleutil.CallMethod(task.ToIDispatch(), "Run", mode+":"+id)
	if err != nil {
		return err
	}
	run.Clear()
	return nil
}

// checkConsoleVM preserves the tray's accepted VM-name syntax and requires an exact existing name. The task itself
// receives a GUID and independently resolves it, rather than treating the caller's name as a command-line argument.
func checkConsoleVM(vm string, vms []hyperv.VM) error {
	if vm == "" || strings.ContainsAny(vm, "\"\r\n") || strings.HasSuffix(vm, `\`) {
		return fmt.Errorf("invalid VM name %q", vm)
	}
	for _, v := range vms {
		if v.Name == vm {
			return nil
		}
	}
	return fmt.Errorf("no VM named %q", vm)
}

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	pEnumWindows         = user32.NewProc("EnumWindows")
	pIsWindowVisible     = user32.NewProc("IsWindowVisible")
	pIsIconic            = user32.NewProc("IsIconic")
	pGetWindowTextW      = user32.NewProc("GetWindowTextW")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pConsoleSetWindowPos = user32.NewProc("SetWindowPos")
	pConsoleAttachInput  = user32.NewProc("AttachThreadInput")
	pSwitchToThisWindow  = user32.NewProc("SwitchToThisWindow")
	enumMu               sync.Mutex // the EnumWindows callback below is shared
	enumFound            []windows.HWND
	enumCallback         = syscall.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		enumFound = append(enumFound, h)
		return 1
	})
)

// focusConsole runs in the elevated console helper, at the same integrity level as VMConnect.
func focusConsole(vm string) bool {
	h := consoleWindow(vm)
	if h == 0 {
		return false
	}
	if r, _, _ := pIsIconic.Call(uintptr(h)); r != 0 {
		pSwitchToThisWindow.Call(uintptr(h), 1)
	}
	if err := raiseConsole(h); err != nil {
		log.Printf("console: %v", err)
		return false
	}
	return true
}

// Raise the viewer without changing its size, position or topmost status.
// Windows can refuse foreground activation; it is not evidence that the video is disconnected.
func raiseConsole(h windows.HWND) error {
	const flags = 0x0001 | 0x0002 | 0x0010 | 0x0040 | 0x4000 // NOSIZE | NOMOVE | NOACTIVATE | SHOWWINDOW | ASYNCWINDOWPOS
	if ok, _, err := pConsoleSetWindowPos.Call(uintptr(h), 0, 0, 0, 0, 0, flags); ok == 0 {
		return fmt.Errorf("raise VMConnect window %d: %w", h, err)
	}
	pSetForegroundWindow.Call(uintptr(h))
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if windows.GetForegroundWindow() == h {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	vm, ok := consoleTitleVM(windowText(h))
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(h, &pid); err != nil || !ok || pid == 0 {
		return errors.New("VMConnect focus target disappeared")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Attached input queues make activation synchronous. Isolate that attempt so a hung
	// foreground application cannot keep the console task and its VM mutex alive indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "--hyperhand-console-focus-helper", vm, strconv.FormatUint(uint64(h), 10), strconv.FormatUint(uint64(pid), 10))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("VMConnect foreground activation: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("VMConnect foreground activation: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if foreground := windows.GetForegroundWindow(); foreground != h {
		return fmt.Errorf("VMConnect window %d did not remain in the foreground (current window %d)", h, foreground)
	}
	return nil
}

func parseConsoleFocusRequest(args []string) (string, windows.HWND, uint32, error) {
	if len(args) != 3 || args[0] == "" || strings.ContainsAny(args[0], "\x00\"\r\n") || strings.HasSuffix(args[0], `\`) {
		return "", 0, 0, errors.New("invalid VMConnect focus request")
	}
	for _, value := range args[1:] {
		if value == "" || strings.Trim(value, "0123456789") != "" {
			return "", 0, 0, errors.New("invalid VMConnect focus identity")
		}
	}
	h, hErr := strconv.ParseUint(args[1], 10, strconv.IntSize)
	pid, pidErr := strconv.ParseUint(args[2], 10, 32)
	if hErr != nil || pidErr != nil || h == 0 || pid == 0 {
		return "", 0, 0, errors.New("invalid VMConnect focus identity")
	}
	return args[0], windows.HWND(h), uint32(pid), nil
}

func consoleFocusTargetMatches(vm string, pid, currentPID uint32, title, image, system, localHost string) bool {
	name, ok := consoleTitleVMForHost(title, localHost)
	return pid != 0 && pid == currentPID && ok && name == vm &&
		strings.EqualFold(image, filepath.Join(system, "vmconnect.exe"))
}

func consoleFocusCommand(args []string) error {
	vm, h, pid, err := parseConsoleFocusRequest(args)
	if err != nil {
		return err
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	localHost, err := os.Hostname()
	if err != nil {
		return err
	}
	image, title := windowProcessImage(h), windowText(h)
	var currentPID uint32
	if _, err := windows.GetWindowThreadProcessId(h, &currentPID); err != nil ||
		!consoleFocusTargetMatches(vm, pid, currentPID, title, image, system, localHost) {
		return errors.New("VMConnect focus target identity changed")
	}
	// A task-launched process may lack foreground permission. Temporarily share the current
	// foreground and viewer input queues for activation; do not inject keys or mouse input.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	foregroundThread, _ := windows.GetWindowThreadProcessId(windows.GetForegroundWindow(), nil)
	viewerThread, _ := windows.GetWindowThreadProcessId(h, nil)
	thread := windows.GetCurrentThreadId()
	if viewerThread != 0 && viewerThread != thread && viewerThread != foregroundThread {
		attached, _, _ := pConsoleAttachInput.Call(uintptr(thread), uintptr(viewerThread), 1)
		if attached != 0 {
			defer pConsoleAttachInput.Call(uintptr(thread), uintptr(viewerThread), 0)
		}
	}
	if foregroundThread != 0 && foregroundThread != thread {
		attached, _, _ := pConsoleAttachInput.Call(uintptr(thread), uintptr(foregroundThread), 1)
		if attached != 0 {
			defer pConsoleAttachInput.Call(uintptr(thread), uintptr(foregroundThread), 0)
		}
	}
	pSetForegroundWindow.Call(uintptr(h))
	if foreground := windows.GetForegroundWindow(); foreground != h {
		return fmt.Errorf("VMConnect window %d could not be brought to the foreground (current window %d)", h, foreground)
	}
	return nil
}

func consoleWindow(vm string) windows.HWND {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return 0
	}
	vmconnect := filepath.Join(system, "vmconnect.exe")
	for _, h := range consoleWindows(0) {
		if r, _, _ := pIsWindowVisible.Call(uintptr(h)); r == 0 {
			continue
		}
		if !strings.EqualFold(windowProcessImage(h), vmconnect) {
			continue
		}
		if name, ok := consoleTitleVM(windowText(h)); !ok || name != vm {
			continue
		}
		return h
	}
	return 0
}

func windowText(h windows.HWND) string {
	buf := make([]uint16, 512)
	n, _, _ := pGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

func windowProcessImage(h windows.HWND) string {
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(h, &pid); err != nil {
		return ""
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// consoleTitleVM accepts the local host's English and Chinese VMConnect title layouts. A remote host with a VM of
// the same name must not be mistaken for the local viewer that recovery is allowed to close.
func consoleTitleVM(title string) (string, bool) {
	host, _ := os.Hostname()
	return consoleTitleVMForHost(title, host)
}

func consoleTitleVMForHost(title, localHost string) (string, bool) {
	local := func(host string) bool {
		return strings.EqualFold(host, "localhost") || localHost != "" && strings.EqualFold(host, localHost)
	}
	if p, ok := strings.CutSuffix(title, " - 虚拟机连接"); ok {
		if host, name, ok := strings.Cut(p, " 上的 "); ok && name != "" && local(host) {
			return name, true
		}
	}
	if p, ok := strings.CutSuffix(title, " - Virtual Machine Connection"); ok {
		if j := strings.LastIndex(p, " on "); j > 0 && local(p[j+4:]) {
			return p[:j], true
		}
	}
	return "", false
}

// enhancedSessionAllowed reports whether the host allows enhanced session mode. VMConnect then may connect in an
// enhanced session, which moves the guest user's session off the console that HyperHand's screenshots and input use.
func enhancedSessionAllowed() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("EnhancedMode")
	return err == nil && v != 0
}

const enhancedSessionWarning = "This host allows enhanced session mode. If VMConnect opens an enhanced session, the guest session moves to it " +
	"and HyperHand's screenshots and input reach the lock screen instead. Use View > Enhanced Session to switch it off in VMConnect, " +
	"or turn off \"Allow enhanced session mode\" in the Hyper-V host settings."

// hostSettings are the per-user host settings: the MCP port and the VMs whose console opens when vm_start starts them.
type hostSettings struct {
	mu   sync.Mutex
	path string
	vms  map[string]bool
	mcp  int // MCP port; 0 means the default
}

type settingsFile struct {
	Port               int      `json:"port,omitempty"`
	OpenConsoleOnStart []string `json:"open_console_on_start"`
}

func loadHostSettings(path string) *hostSettings {
	s := &hostSettings{path: path, vms: map[string]bool{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Print("settings: ", err)
		}
		return s
	}
	var f settingsFile
	if err := json.Unmarshal(b, &f); err != nil {
		log.Print("settings: ", err)
		return s
	}
	for _, vm := range f.OpenConsoleOnStart {
		s.vms[vm] = true
	}
	s.mcp = f.Port
	return s
}

// mcpPort picks the MCP port: an explicit -port flag, else the saved setting, else the flag's default.
func mcpPort(flagPort int, explicit bool, saved int) int {
	if !explicit && saved != 0 {
		return saved
	}
	return flagPort
}

// parsePort accepts a port from 1024 to 65535.
func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || p < 1024 || p > 65535 {
		return 0, fmt.Errorf("enter a port from 1024 to 65535")
	}
	return p, nil
}

// port returns the saved MCP port, or 0 for the default.
func (s *hostSettings) port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcp
}

// setPort changes and saves the MCP port.
func (s *hostSettings) setPort(p int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mcp = p
	return s.save()
}

func (s *hostSettings) onStart(vm string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vms[vm]
}

// setOnStart changes and saves the setting for vm.
func (s *hostSettings) setOnStart(vm string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.vms[vm] = true
	} else {
		delete(s.vms, vm)
	}
	return s.save()
}

// save writes the settings; s.mu must be held.
func (s *hostSettings) save() error {
	f := settingsFile{Port: s.mcp, OpenConsoleOnStart: []string{}}
	for v := range s.vms {
		f.OpenConsoleOnStart = append(f.OpenConsoleOnStart, v)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

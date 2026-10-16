package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

var (
	pGetWindow        = user32.NewProc("GetWindow")
	pPostMessage      = user32.NewProc("PostMessageW")
	pGetClassName     = user32.NewProc("GetClassNameW")
	pEnumChildWindows = user32.NewProc("EnumChildWindows")
	pConsoleEnabled   = user32.NewProc("IsWindowEnabled")
)

// Both the old task and the replacement must belong to the installing user and have an exact known action.
func recognizedConsoleTask(t *taskInfo, p setupPaths, sid string) bool {
	if t == nil || t.userSID != sid || len(t.actions) != 1 || t.triggers != 0 || t.runLevel != 1 {
		return false
	}
	a := t.actions[0]
	path := strings.Trim(a.path, `"`)
	return strings.EqualFold(path, p.vmconnect) && a.args == `localhost "$(Arg0)"` ||
		strings.EqualFold(path, p.hostExe) && a.args == consoleTaskArgs
}

// A task parameter is a fixed mode and a GUID, never a command line, path, or untrusted VM name.
func parseConsoleRequest(args []string) (string, string, error) {
	if len(args) != 1 {
		return "", "", errors.New("console expects open:<VM GUID> or recover:<VM GUID>")
	}
	mode, id, ok := strings.Cut(args[0], ":")
	if !ok || (mode != "open" && mode != "recover") || len(id) != 36 {
		return "", "", errors.New("invalid console request")
	}
	if _, err := windows.GUIDFromString("{" + id + "}"); err != nil {
		return "", "", errors.New("invalid console VM GUID")
	}
	return mode, id, nil
}

func consoleCommand(args []string) error {
	mode, id, err := parseConsoleRequest(args)
	if err != nil {
		return err
	}
	vms, err := hyperv.ListVMs()
	if err != nil {
		return err
	}
	var vm, power string
	for _, v := range vms {
		if strings.EqualFold(v.ID, id) {
			vm = v.Name
			power = v.State
		}
	}
	if vm == "" {
		return errors.New("console VM no longer exists")
	}
	if mode == "recover" && power != "Running" {
		logConsoleResult(vm, "not_running", 0, 0, 0)
		return nil
	}
	runtime.LockOSThread() // Windows mutex ownership belongs to the acquiring thread.
	defer runtime.UnlockOSThread()
	mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr("Local\\HyperHandConsole-"+strings.ToLower(id)))
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		return err
	}
	defer windows.CloseHandle(mutex)
	state, err := windows.WaitForSingleObject(mutex, 0)
	if err != nil {
		return err
	}
	if state == uint32(windows.WAIT_TIMEOUT) {
		return nil // another request for this VM already owns the viewer
	}
	if state != windows.WAIT_OBJECT_0 && state != windows.WAIT_ABANDONED {
		return errors.New("could not acquire console request mutex")
	}
	defer windows.ReleaseMutex(mutex)
	h := consoleWindow(vm)
	if h == 0 && mode == "recover" {
		logConsoleResult(vm, "not_open", 0, 0, 0)
		return nil
	}
	var previousPID uint32
	if h != 0 {
		var pid uint32
		if _, err := windows.GetWindowThreadProcessId(h, &pid); err != nil {
			return err
		}
		disconnected, err := disconnectedConsole(h, pid)
		if err != nil {
			return err
		}
		// Hyper-V's lifecycle job can finish before VMConnect processes its disconnect notification.
		// Keep the recovery request alive briefly; opening a healthy viewer manually needs no wait.
		if mode == "recover" {
			deadline := time.Now().Add(5 * time.Second)
			for !disconnected && time.Now().Before(deadline) {
				time.Sleep(250 * time.Millisecond)
				disconnected, err = disconnectedConsole(h, pid)
				if err != nil {
					return err
				}
			}
		}
		if !disconnected {
			if mode == "open" {
				if !focusConsole(vm) {
					return errors.New("could not raise the existing VMConnect viewer")
				}
			}
			state := "unchanged"
			enabled, _, _ := pConsoleEnabled.Call(uintptr(h))
			if enabled != 0 {
				state, err = reusedConsoleState(func() (bool, error) { return consoleVideoConnected(h, pid) })
				if err != nil {
					log.Printf("console: inspect existing viewer %s (pid %d): %v", vm, pid, err)
				}
			}
			logConsoleResult(vm, state, pid, h, 0)
			return nil
		}
		if err := closeConsoleProcess(vm, h, pid); err != nil {
			return err
		}
		previousPID = pid
	}
	return launchBasicConsole(vm, previousPID, power == "Running")
}

// An existing connected viewer may be enhanced. This observation never proves basic mode or changes the viewer.
func reusedConsoleState(probe func() (bool, error)) (string, error) {
	connected, err := probe()
	if err == nil && connected {
		return "viewer_connected", nil
	}
	return "unchanged", err
}

func logConsoleResult(vm, state string, pid uint32, h windows.HWND, previous uint32) {
	result := map[string]any{"vm": vm, "state": state, "pid": pid, "window": uint64(h), "previous_pid": previous, "viewer_count": consoleCount(vm), "foreground": h != 0 && windows.GetForegroundWindow() == h}
	data, _ := json.Marshal(result)
	log.Printf("console: %s", data)
}

func consoleCount(vm string) int {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return 0
	}
	n := 0
	for _, h := range consoleWindows(0) {
		name, ok := consoleTitleVM(windowText(h))
		if ok && name == vm && windows.IsWindowVisible(h) && strings.EqualFold(windowProcessImage(h), filepath.Join(system, "vmconnect.exe")) {
			n++
		}
	}
	return n
}

func consoleWindows(parent windows.HWND) []windows.HWND {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumFound = nil
	if parent == 0 {
		pEnumWindows.Call(enumCallback, 0)
	} else {
		pEnumChildWindows.Call(uintptr(parent), enumCallback, 0)
	}
	return append([]windows.HWND(nil), enumFound...)
}

func windowClass(h windows.HWND) string {
	var buf [256]uint16
	n, _, _ := pGetClassName.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

func consoleControls(h windows.HWND, pid uint32) (proto.ControlsResult, error) {
	reply, err := readConsoleUI(struct {
		Controls proto.ControlsArgs `json:"controls"`
	}{proto.ControlsArgs{Handle: uint64(h), PID: pid, MaxDepth: 5, MaxNodes: 100}})
	return reply.Controls, err
}

type consoleUIReply struct {
	Controls proto.ControlsResult     `json:"controls"`
	Find     proto.FindControlsResult `json:"find"`
	Error    string                   `json:"error"`
}

func readConsoleUI(request any) (consoleUIReply, error) {
	self, err := os.Executable()
	if err != nil {
		return consoleUIReply{}, err
	}
	data, _ := json.Marshal(request)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(filepath.Dir(self), "hyperhand-agent.exe"), "--hyperhand-controls-helper")
	cmd.Stdin = bytes.NewReader(data)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	var reply consoleUIReply
	decodeErr := json.Unmarshal(out, &reply)
	if reply.Error != "" {
		return consoleUIReply{}, errors.New(reply.Error)
	}
	if ctx.Err() != nil {
		return consoleUIReply{}, fmt.Errorf("read VMConnect controls: %w", ctx.Err())
	}
	if runErr != nil {
		return consoleUIReply{}, fmt.Errorf("read VMConnect controls: %w; stderr: %s; stdout: %s", runErr, strings.TrimSpace(stderr.String()), strings.TrimSpace(string(out)))
	}
	if decodeErr != nil {
		return consoleUIReply{}, decodeErr
	}
	return reply, nil
}

// Request shallow status-bar searches instead of full-window patterns, values and selected text. The generic
// helper can still probe child boundaries or fall back to locating the scoped root if its screen hint is obscured.
func consoleVideoConnected(h windows.HWND, pid uint32) (bool, error) {
	return findConsoleVideo(h, pid, func(a proto.FindControlsArgs) (proto.FindControlsResult, error) {
		reply, err := readConsoleUI(struct {
			Find proto.FindControlsArgs `json:"find"`
		}{a})
		return reply.Find, err
	})
}

func findConsoleVideo(h windows.HWND, pid uint32, find func(proto.FindControlsArgs) (proto.FindControlsResult, error)) (bool, error) {
	a := proto.FindControlsArgs{Handle: uint64(h), PID: pid, AutomationID: "StatusBar", MaxDepth: 1, MaxVisited: 64, MaxMatches: 4}
	status, err := find(a)
	if err != nil {
		return false, fmt.Errorf("VMConnect status bar: %w", err)
	}
	for _, bar := range status.Matches {
		if bar.RuntimeID == "" || bar.Offscreen {
			continue
		}
		a.ControlType, a.AutomationID, a.MaxMatches = 50020, "", 16
		a.RootRuntimeID, a.HintRect = bar.RuntimeID, &bar.Rect
		locks, err := find(a)
		if err != nil {
			return false, fmt.Errorf("VMConnect video status: %w", err)
		}
		for _, lock := range locks.Matches {
			if lock.ControlType == 50020 && !lock.Offscreen && consoleVideoLabel(lock.Name) {
				return true, nil
			}
		}
	}
	return false, nil
}

// VmisStatusStrip.InformConnectedState sets LockIcon's accessible name from these resource strings only while
// video is connected; it clears the name while disconnected/connecting. ToolStripItem.Name is not exposed as
// AutomationID by VMConnect's current accessibility provider, so match exact labels inside StatusBar instead.
func consoleVideoLabel(name string) bool {
	switch name {
	case "Connection unsecure", "Connection secured using certificate", "Connection secured using Kerberos", "Connection secured using Kerberos with certification",
		"连接不安全", "已使用证书保证连接安全", "已使用 Kerberos 保证连接安全", "已使用带证书的 Kerberos 保证连接安全":
		return true
	}
	return false
}

func reconnectPrompt(nodes []proto.ControlInfo) bool {
	reconnect, exit := false, false
	for _, n := range nodes {
		if n.ControlType != 50000 || !n.Enabled || n.Offscreen {
			continue
		}
		name := strings.ReplaceAll(n.Name, "&", "")
		reconnect = reconnect || strings.HasPrefix(name, "Reconnect") || strings.HasPrefix(name, "重新连接") || strings.HasPrefix(name, "Connect") || strings.HasPrefix(name, "连接")
		exit = exit || strings.HasPrefix(name, "Exit") || strings.HasPrefix(name, "退出")
	}
	return reconnect && exit
}

func disconnectedConsole(main windows.HWND, pid uint32) (bool, error) {
	// UIA can expose the modal content beneath its disabled owner rather than as an independent window root.
	tree, err := consoleControls(main, pid)
	if err != nil && !transientConsoleRead(main, pid, err) {
		return false, err
	}
	if err == nil && len(tree.Nodes) > 0 && !tree.Nodes[0].Enabled && reconnectPrompt(tree.Nodes) {
		return true, nil
	}
	for _, h := range consoleWindows(0) {
		var ownerPID uint32
		windows.GetWindowThreadProcessId(h, &ownerPID)
		if ownerPID != pid || !ownedConsoleWindow(h, main) || !windows.IsWindowVisible(h) {
			continue
		}
		tree, err := consoleControls(h, pid)
		if err != nil {
			if transientConsoleRead(h, pid, err) {
				continue
			}
			return false, err
		}
		if reconnectPrompt(tree.Nodes) {
			return true, nil
		}
	}
	return false, nil
}

func ownedConsoleWindow(h, main windows.HWND) bool {
	for i := 0; i < 16; i++ {
		owner, _, _ := pGetWindow.Call(uintptr(h), 4) // GW_OWNER
		if owner == uintptr(main) {
			return true
		}
		if owner == 0 || owner == uintptr(h) {
			return false
		}
		h = windows.HWND(owner)
	}
	return false
}

func closeConsoleProcess(vm string, h windows.HWND, pid uint32) error {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(p)
	var path [windows.MAX_PATH]uint16
	n := uint32(len(path))
	if err := windows.QueryFullProcessImageName(p, 0, &path[0], &n); err != nil {
		return err
	}
	var currentPID uint32
	windows.GetWindowThreadProcessId(h, &currentPID)
	name, ok := consoleTitleVM(windowText(h))
	if currentPID != pid || !ok || name != vm || !strings.EqualFold(windows.UTF16ToString(path[:n]), filepath.Join(system, "vmconnect.exe")) {
		return errors.New("VMConnect process identity changed")
	}
	if disconnected, err := disconnectedConsole(h, pid); err != nil {
		return err
	} else if !disconnected {
		return errors.New("VMConnect disconnect prompt changed before recovery")
	}
	pPostMessage.Call(uintptr(h), 0x10, 0, 0) // WM_CLOSE, only the verified viewer
	state, err := windows.WaitForSingleObject(p, 2000)
	if err != nil {
		return err
	}
	if state == windows.WAIT_OBJECT_0 {
		return nil
	}
	if err := windows.TerminateProcess(p, 0); err != nil {
		return err
	}
	state, err = windows.WaitForSingleObject(p, 3000)
	if err != nil {
		return err
	}
	if state != windows.WAIT_OBJECT_0 {
		return errors.New("old VMConnect did not exit")
	}
	return nil
}

func launchBasicConsole(vm string, previousPID uint32, requireVideo bool) error {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	basicSelected := !enhancedSessionAllowed() || basicConsolePreference()
	// /edit prevents saved enhanced settings from bypassing its options dialog. Closing that dialog returns Cancel;
	// VMConnect sets this process's enhanced preference false and connects in basic mode, without changing settings.
	cmd := exec.Command(filepath.Join(system, "vmconnect.exe"), "localhost", vm, "/edit")
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Release()
	pid := uint32(cmd.Process.Pid)
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		for _, h := range consoleWindows(0) {
			var windowPID uint32
			windows.GetWindowThreadProcessId(h, &windowPID)
			if windowPID != pid || !windows.IsWindowVisible(h) {
				continue
			}
			if name, ok := consoleTitleVM(windowText(h)); ok && name == vm {
				if enabled, _, _ := pConsoleEnabled.Call(uintptr(h)); enabled == 0 {
					continue
				}
				if !requireVideo {
					if err := raiseConsole(h); err != nil {
						return err
					}
					logConsoleResult(vm, "viewer_open", pid, h, previousPID)
					return nil
				}
				if !basicSelected {
					continue
				}
				connected, err := consoleVideoConnected(h, pid)
				if err != nil {
					if transientConsoleRead(h, pid, err) {
						lastErr = err
						continue
					}
					return err
				}
				if !connected {
					continue
				}
				for _, child := range consoleWindows(h) {
					if consoleVideoSurface(windowClass(child), windows.IsWindowVisible(child)) {
						if count := consoleCount(vm); count != 1 {
							return fmt.Errorf("VMConnect has %d visible viewers for %s, expected one", count, vm)
						}
						if err := raiseConsole(h); err != nil {
							return err
						}
						logConsoleResult(vm, "basic_viewer_connected", pid, h, previousPID)
						return nil
					}
				}
				continue
			}
			tree, err := consoleControls(h, pid)
			if err != nil {
				if transientConsoleRead(h, pid, err) {
					lastErr = err
					continue
				}
				return err
			}
			if len(tree.Nodes) > 0 && tree.Nodes[0].AutomationID == "EnhancedConnectionDialog" {
				if sent, _, err := pPostMessage.Call(uintptr(h), 0x10, 0, 0); sent == 0 {
					return fmt.Errorf("cancel VMConnect enhanced options: %w", err)
				}
				basicSelected = true
				continue
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("VMConnect did not become ready within 20 seconds; last read: %w", lastErr)
	}
	return errors.New("VMConnect basic viewer did not become visible within 20 seconds; inspect its window")
}

// VMConnect's basic RDP renderer exposes an OPWindowClass output surface; other RDP client versions expose the
// shell container. Neither class proves a connection by itself: callers also require the connected-video status.
func consoleVideoSurface(class string, visible bool) bool {
	return visible && (class == "OPWindowClass" || class == "TscShellContainerClass")
}

func transientConsoleRead(h windows.HWND, pid uint32, err error) bool {
	var current uint32
	_, queryErr := windows.GetWindowThreadProcessId(h, &current)
	return queryErr != nil || current != pid || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "0x80040201") // UIA_E_ELEMENTNOTAVAILABLE during window replacement
}

// Only an explicit false is trusted; missing/unknown settings require observing the options dialog's cancellation.
// This is the same known-folder path VMConnect uses, and is read without modifying the user's preferences.
func basicConsolePreference() bool {
	appData, err := windows.KnownFolderPath(windows.FOLDERID_RoamingAppData, 0)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(appData, "Microsoft", "Windows", "Hyper-V", "Client", "1.0", "clientsettings.config"))
	return err == nil && parseBasicConsolePreference(data)
}

func parseBasicConsolePreference(data []byte) bool {
	var config struct {
		Settings []struct {
			Name  string `xml:"name,attr"`
			Type  string `xml:"type,attr"`
			Value string `xml:"value"`
		} `xml:"Microsoft.Virtualization.Client.ClientVirtualizationSettings>setting"`
	}
	if xml.Unmarshal(data, &config) != nil {
		return false
	}
	count, basic := 0, false
	for _, setting := range config.Settings {
		if setting.Name == "VMConnectUseEnhancedMode" {
			count++
			basic = setting.Type == "System.Boolean" && strings.EqualFold(strings.TrimSpace(setting.Value), "false")
		}
	}
	return count == 1 && basic
}

// hyperhand-agent runs in the VM user's desktop session, shows a tray icon and serves
// requests from the host over a Hyper-V socket. "hyperhand-agent.exe install" installs it
// to %LOCALAPPDATA%\HyperHand and starts it at logon (HKCU Run key, no admin needed);
// "hyperhand-agent.exe uninstall" stops it, removes the Run value and deletes
// %LOCALAPPDATA%\HyperHand and C:\Users\Public\HyperHand (also no admin needed).
//
// Build: go build -ldflags "-H windowsgui" ./cmd/hyperhand-agent
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"hyperhand/internal/agent"
	"hyperhand/internal/hvsock"
	"hyperhand/internal/tray"
)

var mutex windows.Handle

func main() {
	if handled, err := agent.RunControlsHelper(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if handled, err := agent.RunAdminHelper(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "install" {
		id, err := parseInstallID(os.Args[2:])
		if err == nil {
			err = install(id)
		}
		if err != nil {
			windows.MessageBox(0, windows.StringToUTF16Ptr(err.Error()), windows.StringToUTF16Ptr("HyperHand"), 0x10)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "uninstall" {
		uninstall()
		return
	}
	id, err := parseInstallID(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	agent.InstallID = id
	h, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr("HyperHandAgent"))
	if err == windows.ERROR_ALREADY_EXISTS {
		return
	}
	mutex = h
	windows.NewLazySystemDLL("user32.dll").NewProc("SetProcessDPIAware").Call()
	if exe, err := os.Executable(); err == nil {
		go func() { // the previous version may still be exiting after update_agent
			for i := 0; i < 20; i++ {
				if err := os.Remove(exe + ".old"); err == nil || os.IsNotExist(err) {
					return
				}
				time.Sleep(500 * time.Millisecond)
			}
		}()
	}
	agent.AfterUpdate = restart
	const cmdQuit = 1
	items := func(status string) []tray.Item {
		return []tray.Item{{ID: 2, Text: status, Disabled: true}, {ID: cmdQuit, Text: "Exit"}}
	}
	tr := tray.New("HyperHand", items("Waiting for the host"))
	tr.OnCommand = func(id int) {
		if id == cmdQuit {
			tr.Quit()
		}
	}
	serve(func(status string) { tr.SetItems(items(status)) })
	icon, err := smallIcon()
	if err == nil {
		tr.Icon = icon
		err = tr.Run() // returns nil after Exit
		icon.DestroyIcon()
	}
	if err != nil {
		select {} // without a tray icon, keep serving the host
	}
}

// smallIcon loads the icon in the executable's resources (winres/winres.json) at the small icon size. The agent has
// no manifest selecting Common Controls 6, which LoadIconMetric needs, so it uses LoadImage.
func smallIcon() (win.HICON, error) {
	inst, err := win.GetModuleHandle("")
	if err != nil {
		return 0, err
	}
	h, err := inst.LoadImage(win.ResIdInt(1), co.IMAGE_ICON,
		int(win.GetSystemMetrics(co.SM_CXSMICON)), int(win.GetSystemMetrics(co.SM_CYSMICON)), co.LR_DEFAULTCOLOR)
	return win.HICON(h), err
}

// serve answers the host on the Hyper-V socket in the background and reports the connection state.
func serve(status func(string)) {
	go func() {
		for {
			l, err := hvsock.Listen()
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			for {
				c, err := l.Accept()
				if err != nil {
					break
				}
				status("Host connected")
				agent.Serve(c)
				c.Close()
				status("Waiting for the host")
			}
			l.Close()
			time.Sleep(time.Second)
		}
	}()
}

// restart starts the (already replaced) exe and exits.
func restart() {
	windows.CloseHandle(mutex)
	if exe, err := os.Executable(); err == nil {
		exec.Command(exe, os.Args[1:]...).Start()
	}
	os.Exit(0)
}

func hidden(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return c
}

func install(id string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "HyperHand")
	dst := filepath.Join(dir, "hyperhand-agent.exe")
	// Stop any running instance (other than this process).
	hidden("taskkill", "/F", "/IM", "hyperhand-agent.exe", "/FI", "PID ne "+strconv.Itoa(os.Getpid())).Run()
	time.Sleep(500 * time.Millisecond)
	if !samePath(self, dst) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// The killed instance may still hold its image for a moment.
		for i := 0; ; i++ {
			if err = copyFile(self, dst); err == nil || i == 20 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err != nil {
			return err
		}
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue("HyperHandAgent", `"`+dst+`"`); err != nil {
		return err
	}
	return exec.Command(dst, installLaunchArgs(id)...).Start()
}

// uninstall undoes install (and the C:\Users\Public staging copy); every step runs even if
// an earlier one fails, and the result is shown in a message box.
func uninstall() {
	var done, failed []string
	pid := strconv.Itoa(os.Getpid())
	hidden("taskkill", "/F", "/IM", "hyperhand-agent.exe", "/FI", "PID ne "+pid).Run()
	for i := 0; i < 20; i++ {
		out, _ := hidden("tasklist", "/NH", "/FI", "IMAGENAME eq hyperhand-agent.exe", "/FI", "PID ne "+pid).Output()
		if !strings.Contains(strings.ToLower(string(out)), "hyperhand-agent.exe") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	done = append(done, "Stopped other hyperhand-agent.exe processes")

	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE); err != nil {
		failed = append(failed, `HKCU\...\Run\HyperHandAgent: `+err.Error())
	} else {
		if err := k.DeleteValue("HyperHandAgent"); err != nil && err != registry.ErrNotExist {
			failed = append(failed, `HKCU\...\Run\HyperHandAgent: `+err.Error())
		} else {
			done = append(done, `HKCU\...\Run\HyperHandAgent`)
		}
		k.Close()
	}

	self, _ := os.Executable()
	d, f := removeFolders(self, os.TempDir(), filepath.Join(os.Getenv("LOCALAPPDATA"), "HyperHand"), `C:\Users\Public\HyperHand`)
	done, failed = append(done, d...), append(failed, f...)

	msg := "Removed:\n" + strings.Join(done, "\n")
	flags := uint32(0x40) // MB_ICONINFORMATION
	if len(failed) > 0 {
		msg += "\n\nFailed:\n" + strings.Join(failed, "\n")
		flags = 0x30 // MB_ICONWARNING
	}
	windows.MessageBox(0, windows.StringToUTF16Ptr(msg), windows.StringToUTF16Ptr("HyperHand uninstall"), flags)
}

// removeFolders deletes dirs; the running executable self, if it is in one of them, is moved to temp first, or to the
// parent of its folder when temp is on another volume. The working directory leaves the folders too.
func removeFolders(self, temp string, dirs ...string) (done, failed []string) {
	os.Chdir(temp)
	for _, dir := range dirs {
		if self != "" && strings.HasPrefix(strings.ToLower(self), strings.ToLower(dir)+`\`) {
			moved, err := moveOut(self, temp)
			if err != nil {
				moved, err = moveOut(self, filepath.Dir(dir))
			}
			if err != nil {
				failed = append(failed, self+": "+err.Error())
			} else {
				self = moved
				done = append(done, "This program was moved to "+moved+" (delete it any time)")
			}
		}
		if err := os.RemoveAll(dir); err != nil { // removes what it can even when some entries fail
			failed = append(failed, dir+": "+err.Error())
		} else {
			done = append(done, dir)
		}
	}
	return done, failed
}

// moveOut moves the running executable exe into dir, so that the folder it was in can be deleted now. Windows does
// not delete the file of a running program, but renames it on the same volume, as update_agent does.
func moveOut(exe, dir string) (string, error) {
	dst := filepath.Join(dir, fmt.Sprintf("hyperhand-agent-uninstalled-%d.exe", os.Getpid()))
	if err := os.Rename(exe, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func samePath(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

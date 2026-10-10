// Command hyperhand is the HyperHand host tray program: an MCP server (Streamable HTTP) that controls Hyper-V VMs.
//
//	hyperhand.exe [-port 8770]   run the tray and MCP server as the current user
//	hyperhand.exe install        install the dedicated Hyper-V service and user logon task
//	hyperhand.exe uninstall      remove the host service and user logon task
//	hyperhand.exe dev-install    development: build this checkout and install it through the preauthorized task (no UAC)
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"hyperhand/internal/broker"
	"hyperhand/internal/host"
	"hyperhand/internal/proto"
	"hyperhand/internal/tray"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "service" {
		if err := broker.Run(); err != nil {
			log.Print("service: ", err)
			os.Exit(1)
		}
		return
	}
	// A console command for development: progress on stdout, errors on stderr, no tray or message boxes.
	if len(os.Args) > 1 && os.Args[1] == "dev-install" {
		os.Exit(devInstallCommand(os.Args[2:]))
	}
	// Before the log file is opened, so uninstall can delete its folder.
	if len(os.Args) > 1 && os.Args[1] == "uninstall" {
		uninstall()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "uninstall-cleanup" {
		uninstallCleanup(os.Args[2:])
		return
	}
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "HyperHand")
	os.MkdirAll(dir, 0o755)
	if f, err := os.OpenFile(filepath.Join(dir, "hyperhand.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(f)
	}

	if len(os.Args) > 1 && os.Args[1] == "startup" {
		if err := startupCommand(os.Args[2:]); err != nil {
			log.Print("startup: ", err)
			msgBox("Changing Windows startup failed:\n"+err.Error(), windows.MB_ICONERROR)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "install" {
		if quiet, err := install(os.Args[2:]); err != nil {
			log.Print("install: ", err)
			if !quiet {
				msgBox("HyperHand install failed:\n"+err.Error(), windows.MB_ICONERROR)
			}
			os.Exit(1)
		}
		return
	}

	portFlag := flag.Int("port", 8770, "MCP HTTP port on 127.0.0.1; overrides the port saved in the settings window")
	restartParent := flag.Uint("restart-parent", 0, "internal: wait for the previous tray process")
	flag.Parse()
	explicitPort := false
	flag.Visit(func(f *flag.Flag) { explicitPort = explicitPort || f.Name == "port" })
	if *restartParent != 0 {
		if err := waitForPrevious(uint32(*restartParent)); err != nil {
			log.Print("restart: ", err)
			return
		}
	}
	name, _ := windows.UTF16PtrFromString(`Local\HyperHandTray`)
	mutex, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(mutex)
		return
	}
	if err != nil {
		log.Print("single instance: ", err)
		return
	}
	defer windows.CloseHandle(mutex)

	settings := loadHostSettings(filepath.Join(dir, "settings.json"))
	const cmdSettings, cmdRestart, cmdQuit = 1, 2, 3
	tr := tray.New("HyperHand", []tray.Item{{ID: cmdSettings, Text: "Settings..."}, {}, {ID: cmdRestart, Text: "Restart"}, {ID: cmdQuit, Text: "Quit"}})
	var restart atomic.Bool
	info := &trayInfo{
		port:     mcpPort(*portFlag, explicitPort, settings.port()),
		explicit: explicitPort,
		settings: settings,
		restart:  func() { restart.Store(true); tr.Quit() },
	}
	tr.OnSelect = func() { showSettings(info) }
	tr.OnCommand = func(id int) {
		switch id {
		case cmdSettings:
			showSettings(info)
		case cmdRestart:
			info.restart()
		case cmdQuit:
			tr.Quit()
		}
	}
	url := info.url()
	var httpServer *http.Server
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", info.port)); err != nil {
		log.Print(err)
		info.listenErr = err.Error()
		tr.SetTip("HyperHand: MCP server not running, see Settings")
	} else {
		host.MCPURL = url
		srv := host.NewServer(&host.Manager{AfterStart: func(vm string) {
			if settings.onStart(vm) {
				go func() {
					if err := openConsole(vm); err != nil {
						log.Print("open console: ", err)
					}
				}()
			}
		}})
		mux := http.NewServeMux()
		mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
		httpServer = &http.Server{Handler: mux}
		go func() { log.Print(httpServer.Serve(ln)) }()
		log.Print("listening on ", url)
		go func() {
			if _, err := (&broker.Client{}).ListVMs(); err != nil {
				log.Print("Hyper-V service unavailable: ", err)
				tr.SetTip("HyperHand: background service unavailable, see Settings")
			}
		}()
	}
	icon, err := appIcon(limSmall) // the notification area shows small icons
	if err != nil {
		log.Print("tray: ", err)
	} else {
		tr.Icon = icon
		if err := tr.Run(); err != nil {
			log.Print("tray: ", err)
		}
		icon.DestroyIcon()
	}
	if httpServer != nil {
		httpServer.Close()
	}
	if restart.Load() {
		if err := startReplacement(*portFlag, explicitPort); err != nil {
			log.Print("restart: ", err)
			msgBox("HyperHand restart failed:\n"+err.Error(), windows.MB_ICONERROR)
		}
	}
}

// The replacement waits for this process, including its mutex and pipe handles, to exit. It gets -port only if this
// process had it; otherwise it reads the port saved in the settings, which the settings window may have changed.
func startReplacement(port int, explicit bool) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"-restart-parent", strconv.Itoa(os.Getpid())}
	if explicit {
		args = append(args, "-port", strconv.Itoa(port))
	}
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func waitForPrevious(pid uint32) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err == windows.ERROR_INVALID_PARAMETER { // Already exited.
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 30000)
	if err != nil {
		return err
	}
	if state != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("previous HyperHand process did not exit")
	}
	return nil
}

// registerService registers proto.ServiceID as a Hyper-V socket guest communication service.
func registerService() error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\`+proto.ServiceID, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer k.Close()
	if v, _, err := k.GetStringValue("ElementName"); err == nil && v == "HyperHand" {
		return nil
	}
	return k.SetStringValue("ElementName", "HyperHand")
}

// runAs relaunches this executable elevated (UAC prompt) with args.
func runAs(args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	for i, a := range args {
		args[i] = syscall.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(self)
	params, _ := windows.UTF16PtrFromString(strings.Join(args, " "))
	return windows.ShellExecute(0, verb, file, params, nil, windows.SW_NORMAL)
}

func msgBox(text string, flags uint32) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("HyperHand")
	windows.MessageBox(0, t, c, flags)
}

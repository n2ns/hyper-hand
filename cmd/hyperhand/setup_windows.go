package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"

	"hyperhand/internal/broker"
	"hyperhand/internal/proto"
)

type setupIdentity struct{ SID, User string }

// Resolve the original caller before UAC. USERNAME/USERPROFILE after alternate
// administrator credentials are entered refer to a different person.
func setupOwner(args []string) (setupIdentity, error) {
	var owner setupIdentity
	f := flag.NewFlagSet("setup", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&owner.SID, "owner-sid", "", "original installing user SID")
	f.StringVar(&owner.User, "owner-user", "", "original installing account")
	if err := f.Parse(args); err != nil {
		return owner, err
	}
	if f.NArg() != 0 {
		return owner, errors.New("unexpected setup arguments")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return owner, err
	}
	if owner.SID == "" && owner.User == "" {
		owner.SID = user.User.Sid.String()
		name, domain, _, err := user.User.Sid.LookupAccount("")
		if err != nil {
			return owner, err
		}
		owner.User = domain + `\` + name
	}
	if owner.SID == "" || owner.User == "" {
		return owner, errors.New("owner SID and account must be supplied together")
	}
	sid, _, _, err := windows.LookupSID("", owner.User)
	if err != nil || sid.String() != owner.SID {
		return owner, errors.New("owner account does not match owner SID")
	}
	if !windows.GetCurrentProcessToken().IsElevated() && owner.SID != user.User.Sid.String() {
		return owner, errors.New("setup must be launched by the installing user")
	}
	return owner, nil
}

// Existing protected roots must already have an administrator/SYSTEM owner.
// Creating the new directory with its final descriptor avoids a writable gap.
func setupRoot(path, ownerSID string) error {
	if err := noSetupReparse(path); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return err
		}
		if owner.String() != "S-1-5-32-544" && owner.String() != "S-1-5-18" {
			return fmt.Errorf("untrusted installation directory owner: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		sd, err := windows.SecurityDescriptorFromString(setupRootSDDL(ownerSID))
		if err != nil {
			return err
		}
		p, _ := windows.UTF16PtrFromString(path)
		return windows.CreateDirectory(p, &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd})
	}
	sd, err := windows.SecurityDescriptorFromString(setupRootSDDL(ownerSID))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func setupRootSDDL(ownerSID string) string {
	return "O:BAG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;" + ownerSID + ")"
}

func noSetupReparse(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("setup path must be absolute")
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		p, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attrs, err := windows.GetFileAttributes(p)
		if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return err
		}
		if err == nil && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("refusing reparse point: %s", current)
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

const (
	serviceAccount = `NT SERVICE\` + broker.ServiceName
	logonTask      = "HyperHand"
	cleanupExe     = "uninstall-cleanup.exe"
	socketKey      = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\` + proto.ServiceID
)

// setupPaths are the installation's fixed locations. System paths come from known folders and GetSystemDirectory,
// never from environment variables, which in the elevated process can come from the user's registry.
type setupPaths struct {
	bin, data, hostExe, agentExe, config, serviceData, cleanup, vmconnect string
}

func installPaths() (setupPaths, error) {
	var p setupPaths
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return p, err
	}
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return p, err
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return p, err
	}
	p.bin, p.data = filepath.Join(programFiles, "HyperHand"), filepath.Join(programData, "HyperHand")
	p.hostExe, p.agentExe = filepath.Join(p.bin, "hyperhand.exe"), filepath.Join(p.bin, "hyperhand-agent.exe")
	p.config, p.serviceData, p.cleanup = filepath.Join(p.data, "config.json"), filepath.Join(p.data, "service-data"), filepath.Join(p.data, cleanupExe)
	p.vmconnect = filepath.Join(system, "vmconnect.exe")
	for _, path := range []string{p.bin, p.data, p.hostExe, p.agentExe, p.config, p.serviceData, p.cleanup} {
		if err := noSetupReparse(path); err != nil {
			return p, err
		}
	}
	return p, nil
}

// serviceCommand is the service's command line, as mgr.CreateService builds it.
func serviceCommand(hostExe string) string { return syscall.EscapeArg(hostExe) + " service" }

func runSetup(operation string, args []string) (bool, error) {
	owner, err := setupOwner(args)
	if err != nil {
		return false, err
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return false, runAs(operation, "--owner-sid", owner.SID, "--owner-user", owner.User)
	}
	p, err := installPaths()
	if err != nil {
		return false, err
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	if err := noSetupReparse(self); err != nil {
		return false, err
	}
	if err := removeStaleCleanup(p); err != nil {
		return false, err
	}
	if b, err := os.ReadFile(p.config); err == nil {
		var config struct {
			OwnerSID string `json:"owner_sid"`
		}
		if err := json.Unmarshal(b, &config); err != nil {
			return false, err
		}
		if config.OwnerSID != owner.SID {
			return false, errors.New("installed owner differs; run setup from the original installing user's session")
		}
	} else if !os.IsNotExist(err) {
		return false, err
	} else {
		if operation == "uninstall" {
			return false, errors.New("managed installation not found; no service or files were removed")
		}
		for _, path := range []string{p.data, p.bin} {
			entries, err := os.ReadDir(path)
			if err == nil && len(entries) != 0 {
				return false, fmt.Errorf("unrecognized non-empty installation directory: %s", path)
			}
			if err != nil && !os.IsNotExist(err) {
				return false, err
			}
		}
	}
	if err := setupRoot(p.data, owner.SID); err != nil {
		return false, err
	}
	if err := assertOwned(p.config); err != nil && !os.IsNotExist(err) {
		return false, err
	}

	runtime.LockOSThread() // COM (Task Scheduler) is used from this thread only
	defer runtime.UnlockOSThread()
	ts, err := connectTaskScheduler()
	if err != nil {
		return false, err
	}
	defer ts.close()
	m, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect()
	state, err := inspectInstall(p, owner, ts, m)
	if err != nil {
		return false, err
	}
	if operation == "install" {
		return true, setupInstall(p, owner, self, state, ts, m)
	}
	return true, setupUninstall(p, self, state, ts, m)
}

// installState is what an earlier installation left, checked to be HyperHand's own before it is changed.
type installState struct {
	logonTask, consoleTask, service bool
	startupDisabled                 bool
	oldExe                          string // the executable the logon task started, or ""
}

func inspectInstall(p setupPaths, owner setupIdentity, ts *taskScheduler, m *mgr.Mgr) (installState, error) {
	var st installState
	t, err := ts.task(logonTask)
	if err != nil {
		return st, err
	}
	if t != nil {
		if t.userSID != owner.SID || len(t.actions) != 1 {
			return st, errors.New("existing HyperHand task belongs to another user or has unexpected actions")
		}
		st.logonTask, st.oldExe = true, strings.Trim(t.actions[0].path, `"`)
		st.startupDisabled = !t.enabled
		if !strings.EqualFold(filepath.Base(st.oldExe), "hyperhand.exe") || t.actions[0].args != "" {
			return st, errors.New("existing task is not a recognized HyperHand tray task")
		}
		if err := noSetupReparse(st.oldExe); err != nil {
			return st, err
		}
	}
	if t, err = ts.task(consoleTask); err != nil {
		return st, err
	}
	if t != nil {
		if t.userSID != owner.SID || len(t.actions) != 1 || !strings.EqualFold(strings.Trim(t.actions[0].path, `"`), p.vmconnect) {
			return st, fmt.Errorf("existing %s task belongs to another user or has unexpected actions", consoleTask)
		}
		st.consoleTask = true
	}
	s, err := m.OpenService(broker.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return st, err
	}
	if !strings.EqualFold(c.BinaryPathName, serviceCommand(p.hostExe)) || !strings.EqualFold(c.ServiceStartName, serviceAccount) {
		return st, errors.New("existing service has unexpected binary or account")
	}
	st.service = true
	return st, nil
}

func setupInstall(p setupPaths, owner setupIdentity, self string, st installState, ts *taskScheduler, m *mgr.Mgr) (resultErr error) {
	sourceAgent := filepath.Join(filepath.Dir(self), "hyperhand-agent.exe")
	if err := noSetupReparse(sourceAgent); err != nil {
		return err
	}
	if fi, err := os.Stat(sourceAgent); err != nil || !fi.Mode().IsRegular() {
		return errors.New("place hyperhand-agent.exe beside hyperhand.exe before installing")
	}
	if err := ensureDir(p.bin); err != nil {
		return err
	}
	if err := setProtectedACL(p.bin, "(A;OICI;FRFX;;;BU)"); err != nil {
		return err
	}
	if err := setProtectedACL(p.data, "(A;OICI;FRFX;;;"+owner.SID+")"); err != nil {
		return err
	}
	config, err := json.Marshal(map[string]string{"owner_sid": owner.SID})
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.config, config, 0o644); err != nil {
		return err
	}
	if err := setProtectedACL(p.config, "(A;;FR;;;"+owner.SID+")"); err != nil {
		return err
	}

	if err := stopService(m); err != nil {
		return err
	}
	if st.logonTask {
		if err := ts.stop(logonTask); err != nil {
			return err
		}
	}
	if err := stopProcesses(p.hostExe, self, st.oldExe); err != nil {
		return err
	}
	for _, pair := range [][2]string{{self, p.hostExe}, {sourceAgent, p.agentExe}} {
		if !strings.EqualFold(pair[0], pair[1]) {
			if err := replaceFile(pair[0], pair[1]); err != nil {
				return err
			}
		}
	}

	if err := installService(m, p.hostExe, owner.SID, st.service); err != nil {
		return err
	}
	serviceSID, _, _, err := windows.LookupSID("", serviceAccount)
	if err != nil {
		return err
	}
	svcSID := serviceSID.String()
	if err := setProtectedACL(p.bin, "(A;OICI;FRFX;;;BU)(A;OICI;FRFX;;;"+svcSID+")"); err != nil {
		return err
	}
	for _, path := range []string{p.hostExe, p.agentExe} {
		if err := setProtectedACL(path, "(A;;FRFX;;;BU)(A;;FRFX;;;"+svcSID+")"); err != nil {
			return err
		}
	}
	if err := setProtectedACL(p.data, "(A;OICI;FRFX;;;"+owner.SID+")(A;OICI;FRFX;;;"+svcSID+")"); err != nil {
		return err
	}
	if err := setProtectedACL(p.config, "(A;;FR;;;"+owner.SID+")(A;;FR;;;"+svcSID+")"); err != nil {
		return err
	}
	if err := ensureDir(p.serviceData); err != nil {
		return err
	}
	// 0x1301bf: read, write, execute and delete, but not change permissions or take ownership.
	if err := setProtectedACL(p.serviceData, "(A;OICI;0x1301bf;;;"+svcSID+")"); err != nil {
		return err
	}
	if err := setGroupMember(serviceSID, true); err != nil {
		return err
	}
	if err := registerService(); err != nil { // the Hyper-V socket guest communication service
		return err
	}
	if err := startService(m); err != nil {
		return err
	}

	if err := ts.register(logonTask, logonTaskXML(p.hostExe, owner)); err != nil {
		return err
	}
	if st.startupDisabled {
		// Installation starts the tray once, but must preserve the disabled logon task even if a later step fails.
		defer func() { resultErr = errors.Join(resultErr, ts.setEnabled(logonTask, false)) }()
	}
	if t, err := ts.task(logonTask); err != nil {
		return err
	} else if t == nil || t.runLevel != taskRunLevelLUA {
		return errors.New("logon task is not limited")
	}
	// On demand only (no trigger): the tray runs it with a VM name to open VMConnect with the owner's full token,
	// since VMConnect needs Hyper-V rights that the owner's filtered token lacks. Its only action is vmconnect.exe.
	if err := ts.register(consoleTask, consoleTaskXML(p.vmconnect, owner)); err != nil {
		return err
	}
	if t, err := ts.task(consoleTask); err != nil {
		return err
	} else if t == nil || len(t.actions) != 1 || !strings.EqualFold(t.actions[0].path, p.vmconnect) || t.triggers != 0 {
		return errors.New("console task verification failed")
	}
	return ts.run(logonTask)
}

func setupUninstall(p setupPaths, self string, st installState, ts *taskScheduler, m *mgr.Mgr) error {
	hostHash, err := fileHash(p.hostExe)
	if err != nil {
		return err
	}
	agentHash, err := fileHash(p.agentExe)
	if err != nil {
		return err
	}
	if err := stopService(m); err != nil {
		return err
	}
	if st.logonTask {
		if err := ts.stop(logonTask); err != nil {
			return err
		}
		if err := ts.delete(logonTask); err != nil {
			return err
		}
	}
	if st.consoleTask {
		if err := ts.delete(consoleTask); err != nil {
			return err
		}
	}
	if err := stopProcesses(p.hostExe, st.oldExe); err != nil {
		return err
	}
	if st.service {
		serviceSID, _, _, err := windows.LookupSID("", serviceAccount)
		if err != nil {
			return err
		}
		if err := setGroupMember(serviceSID, false); err != nil {
			return err
		}
		if err := deleteService(m); err != nil {
			return err
		}
	}
	if err := registry.DeleteKey(registry.LOCAL_MACHINE, socketKey); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	if entries, err := os.ReadDir(p.serviceData); err == nil && len(entries) == 0 {
		if err := os.Remove(p.serviceData); err != nil {
			return err
		}
	}
	if !strings.EqualFold(self, p.hostExe) {
		return removeInstalledFiles(p, hostHash, agentHash, false)
	}
	// This process is the installed host and cannot delete its own file. A protected copy waits for it to exit and
	// removes only the named files with the original hashes.
	return startCleanup(p, self, hostHash, agentHash)
}

// removeInstalledFiles deletes the installed executables if they still have the hashes taken at uninstall ("" =
// absent), then the empty installation directory, and the owner marker and data directory unless other data
// remains. The cleanup copy cannot delete itself; it and the data directory are deleted at the next restart.
func removeInstalledFiles(p setupPaths, hostHash, agentHash string, fromCleanup bool) error {
	for _, path := range []string{p.bin, p.data} {
		if err := noSetupReparse(path); err != nil {
			return err
		}
	}
	for _, f := range [][2]string{{p.hostExe, hostHash}, {p.agentExe, agentHash}} {
		h, err := fileHash(f[0])
		if err != nil {
			return err
		}
		if h == "" {
			continue
		}
		if h != f[1] {
			return fmt.Errorf("%s changed; cleanup stopped", f[0])
		}
		if err := os.Remove(f[0]); err != nil {
			return err
		}
	}
	if entries, err := os.ReadDir(p.bin); err == nil && len(entries) == 0 {
		if err := os.Remove(p.bin); err != nil {
			return err
		}
	}
	// Keep the owner marker with retained service data, so reinstall can verify ownership without deleting those
	// files. Otherwise remove it last.
	entries, err := os.ReadDir(p.data)
	if err != nil {
		return err
	}
	remaining := 0
	for _, e := range entries {
		if !strings.EqualFold(e.Name(), "config.json") && !(fromCleanup && strings.EqualFold(e.Name(), cleanupExe)) {
			remaining++
		}
	}
	if fromCleanup {
		if err := deleteAtRestart(p.cleanup); err != nil {
			return err
		}
	}
	if remaining != 0 {
		return nil
	}
	if err := os.Remove(p.config); err != nil && !os.IsNotExist(err) {
		return err
	}
	if !fromCleanup {
		return os.Remove(p.data)
	}
	return deleteAtRestart(p.data) // a directory is deleted at restart only if it is empty
}

// deleteAtRestart registers path to be deleted when Windows restarts (administrators only).
var deleteAtRestart = func(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(name, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

// startCleanup copies this executable to an administrators-only file and runs it to finish uninstalling.
func startCleanup(p setupPaths, self, hostHash, agentHash string) error {
	if err := copyProtected(self, p.cleanup, "O:BAG:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)"); err != nil {
		return err
	}
	cmd := exec.Command(p.cleanup, "uninstall-cleanup", "--wait-pid", strconv.Itoa(os.Getpid()), "--host-hash", hostHash, "--agent-hash", agentHash)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// uninstallCleanup is the copy started by startCleanup. It finds the installation itself rather than trusting its
// arguments, waits for the uninstalling process to exit and records a failure in uninstall-error.log.
func uninstallCleanup(args []string) {
	p, err := installPaths()
	if err != nil {
		return
	}
	err = func() error {
		f := flag.NewFlagSet("uninstall-cleanup", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		pid := f.Uint("wait-pid", 0, "")
		hostHash := f.String("host-hash", "", "")
		agentHash := f.String("agent-hash", "", "")
		if err := f.Parse(args); err != nil {
			return err
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		if !strings.EqualFold(self, p.cleanup) || !windows.GetCurrentProcessToken().IsElevated() {
			return errors.New("uninstall cleanup must run elevated from its own location")
		}
		if h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(*pid)); err == nil {
			windows.WaitForSingleObject(h, windows.INFINITE)
			windows.CloseHandle(h)
		} else if err != windows.ERROR_INVALID_PARAMETER { // already exited
			return err
		}
		return removeInstalledFiles(p, *hostHash, *agentHash, true)
	}()
	if err != nil {
		os.WriteFile(filepath.Join(p.data, "uninstall-error.log"), []byte(err.Error()+"\n"), 0o644)
	}
}

// removeStaleCleanup deletes the cleanup copy an earlier uninstall left for deletion at restart, unless it is still
// running. Only inside a data directory that administrators own.
func removeStaleCleanup(p setupPaths) error {
	if _, err := os.Stat(p.cleanup); err != nil {
		return nil
	}
	if assertOwned(p.data) != nil {
		return nil // the ownership checks that follow report it
	}
	running, err := openProcessesWithPath(0, p.cleanup)
	if err != nil {
		return err
	}
	for _, h := range running {
		windows.CloseHandle(h)
	}
	if len(running) != 0 {
		return errors.New("an earlier uninstall is still finishing; try again shortly")
	}
	return os.Remove(p.cleanup)
}

// assertOwned requires path to be owned by SYSTEM or Administrators, which ordinary users cannot change.
func assertOwned(path string) error {
	if err := noSetupReparse(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if s := owner.String(); s != "S-1-5-18" && s != "S-1-5-32-544" {
		return fmt.Errorf("untrusted resource owner: %s", path)
	}
	return nil
}

// setProtectedACL makes Administrators the owner and replaces the DACL with SYSTEM and Administrators full control
// plus extra ACEs, not inherited from the parent.
func setProtectedACL(path, extra string) error {
	if err := noSetupReparse(path); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("O:BAG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" + extra)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	group, _, err := sd.Group()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, group, dacl, nil)
}

// ensureDir creates an administrators-only directory, or checks that an existing one is owned by administrators.
func ensureDir(path string) error {
	if err := noSetupReparse(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return assertOwned(path)
	} else if !os.IsNotExist(err) {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("O:BAG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	p, _ := windows.UTF16PtrFromString(path)
	return windows.CreateDirectory(p, &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd})
}

// replaceFile copies src beside dst under its final protected descriptor, checks the copy's hash and renames it
// over dst.
func replaceFile(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		if err := assertOwned(dst); err != nil {
			return err
		}
	}
	next := dst + ".installing"
	if err := noSetupReparse(next); err != nil {
		return err
	}
	if _, err := os.Lstat(next); err == nil {
		return fmt.Errorf("stale installer file: %s", next)
	}
	defer os.Remove(next)
	if err := copyProtected(src, next, "O:BAG:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)"); err != nil {
		return err
	}
	want, err := fileHash(src)
	if err != nil {
		return err
	}
	if got, err := fileHash(next); err != nil {
		return err
	} else if got != want {
		return errors.New("copied executable hash mismatch")
	}
	from, _ := windows.UTF16PtrFromString(next)
	to, _ := windows.UTF16PtrFromString(dst)
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// copyProtected copies src to a new file dst created with the security descriptor sddl, so dst is never accessible
// under another descriptor.
func copyProtected(src, dst, sddl string) error {
	if err := noSetupReparse(dst); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	name, _ := windows.UTF16PtrFromString(dst)
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0,
		&windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd},
		windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	out := os.NewFile(uintptr(h), dst)
	in, err := os.Open(src)
	if err != nil {
		out.Close()
		return err
	}
	defer in.Close()
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// fileHash returns the SHA-256 of path in hex, or "" if it does not exist.
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// openProcessesWithPath opens, with access, the processes other than this one whose executable is one of paths. Each
// path is checked on the returned handle itself, so a reused process ID cannot stand in for an exited process; a
// process that exits meanwhile is left out. A process with the same file name that cannot be checked is an error. The
// caller closes the handles.
func openProcessesWithPath(access uint32, paths ...string) ([]windows.Handle, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	names := map[string]bool{}
	for _, p := range paths {
		if p != "" {
			names[strings.ToLower(filepath.Base(p))] = true
		}
	}
	var hs []windows.Handle
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == uint32(os.Getpid()) || !names[strings.ToLower(windows.UTF16ToString(e.ExeFile[:]))] {
			continue
		}
		h, err := openProcessWithPath(e.ProcessID, access, paths...)
		if err != nil {
			for _, h := range hs {
				windows.CloseHandle(h)
			}
			return nil, fmt.Errorf("cannot verify HyperHand process %d: %w", e.ProcessID, err)
		}
		if h != 0 {
			hs = append(hs, h)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		for _, h := range hs {
			windows.CloseHandle(h)
		}
		return nil, err
	}
	return hs, nil
}

// openProcessWithPath opens process pid with access if its executable is one of paths; it returns 0 when it is not,
// or has exited. An exited process can still be opened while another process holds a handle to it, but its image name
// can then no longer be read.
func openProcessWithPath(pid, access uint32, paths ...string) (windows.Handle, error) {
	h, err := windows.OpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err == windows.ERROR_INVALID_PARAMETER {
		return 0, nil // no such process any more
	}
	if err != nil {
		return 0, err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		ev, werr := windows.WaitForSingleObject(h, 0)
		windows.CloseHandle(h)
		if werr == nil && ev == windows.WAIT_OBJECT_0 {
			return 0, nil
		}
		return 0, err
	}
	image := windows.UTF16ToString(buf[:n])
	for _, p := range paths {
		if p != "" && strings.EqualFold(image, p) {
			return h, nil
		}
	}
	windows.CloseHandle(h)
	return 0, nil
}

// stopProcesses ends the HyperHand processes started from paths (the trays; the service process has already exited)
// and waits for them to exit.
func stopProcesses(paths ...string) error {
	hs, err := openProcessesWithPath(windows.PROCESS_TERMINATE, paths...)
	if err != nil {
		return err
	}
	defer func() {
		for _, h := range hs {
			windows.CloseHandle(h)
		}
	}()
	for _, h := range hs {
		if err := stopProcess(h); err != nil {
			pid, _ := windows.GetProcessId(h)
			return fmt.Errorf("HyperHand process %d: %w", pid, err)
		}
	}
	return nil
}

// stopProcess ends the process h (PROCESS_TERMINATE and SYNCHRONIZE access) and waits up to 10 seconds for it to exit.
// TerminateProcess is asynchronous and fails with ERROR_ACCESS_DENIED for a process that has already terminated, as a
// tray just ended by its task's Stop may have, so its error counts only if the process does not exit.
func stopProcess(h windows.Handle) error {
	terr := windows.TerminateProcess(h, 1)
	ev, err := windows.WaitForSingleObject(h, 10000)
	switch {
	case err != nil:
		return err
	case ev == windows.WAIT_OBJECT_0:
		return nil
	case terr != nil:
		return terr
	}
	return errors.New("did not exit")
}

// waitProcessGone waits until no process with the ID and executable file name is listed. It needs no access to the
// process, which administrators may not have for one running as a service account.
func waitProcessGone(pid uint32, exe string, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); ; time.Sleep(200 * time.Millisecond) {
		listed, err := processListed(pid, exe)
		if err != nil || !listed {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %d did not exit", pid)
		}
	}
}

func processListed(pid uint32, exe string) (bool, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == pid && strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), exe) {
			return true, nil
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return false, err
	}
	return false, nil
}

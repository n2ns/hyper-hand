package main

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"

	"hyperhand/internal/broker"
)

func TestSetupOwnerRoundTrip(t *testing.T) {
	owner, err := setupOwner(nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := setupOwner([]string{"--owner-sid", owner.SID, "--owner-user", owner.User})
	if err != nil || got != owner {
		t.Fatalf("UAC identity round trip: %#v, %v", got, err)
	}
	for _, args := range [][]string{
		{"--owner-sid", owner.SID},
		{"--owner-user", owner.User},
		{"--owner-sid", "S-1-5-18", "--owner-user", owner.User},
		{"unexpected"},
	} {
		if _, err := setupOwner(args); err == nil {
			t.Fatalf("accepted ambiguous/mismatched identity: %v", args)
		}
	}
}

func TestSetupRejectsReparseAncestor(t *testing.T) {
	base := t.TempDir()
	if err := noSetupReparse(filepath.Join(base, "missing", "file")); err != nil {
		t.Fatal(err)
	}
	if err := noSetupReparse(`relative\file`); err == nil {
		t.Fatal("accepted relative path")
	}
	target, link := filepath.Join(base, "target"), filepath.Join(base, "link")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink privilege unavailable: %v", err)
	}
	if err := noSetupReparse(filepath.Join(link, "not-created")); err == nil {
		t.Fatal("accepted reparse ancestor")
	}
}

func TestSetupACLHasNoUserWriteGrant(t *testing.T) {
	owner, err := setupOwner(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Windows may serialize a RID-500 SID as LA instead of its numeric string.
	// Compare binary ACEs so both forms retain the same security assertions.
	adminSD, err := windows.SecurityDescriptorFromString("O:LA")
	if err != nil {
		t.Fatal(err)
	}
	adminSID, _, err := adminSD.Owner()
	if err != nil {
		t.Fatal(err)
	}
	for name, ownerSID := range map[string]string{"current-user": owner.SID, "local-administrator": adminSID.String()} {
		t.Run(name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(setupRootSDDL(ownerSID))
			if err != nil {
				t.Fatal(err)
			}
			control, _, err := sd.Control()
			if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
				t.Fatalf("missing protected DACL: control=%#x err=%v", control, err)
			}
			dacl, _, err := sd.DACL()
			if err != nil || dacl == nil {
				t.Fatalf("missing DACL: %v", err)
			}
			const fileAllAccess = 0x1f01ff // FILE_ALL_ACCESS from winnt.h
			want := []struct {
				sid  string
				mask windows.ACCESS_MASK
			}{
				{"S-1-5-18", fileAllAccess},
				{"S-1-5-32-544", fileAllAccess},
				{ownerSID, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE},
			}
			if int(dacl.AceCount) != len(want) {
				t.Fatalf("ACE count=%d, want %d", dacl.AceCount, len(want))
			}
			for i, entry := range want {
				var ace *windows.ACCESS_ALLOWED_ACE
				if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
					t.Fatal(err)
				}
				sid, err := windows.StringToSid(entry.sid)
				if err != nil {
					t.Fatal(err)
				}
				trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
				if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE || !windows.EqualSid(trustee, sid) || ace.Mask != entry.mask {
					t.Fatalf("ACE %d: type=%d flags=%#x mask=%#x; want allow, OICI, SID %s, mask=%#x", i, ace.Header.AceType, ace.Header.AceFlags, ace.Mask, entry.sid, entry.mask)
				}
			}
		})
	}
}

func TestInstallPathsAreSystemPaths(t *testing.T) {
	p, err := installPaths()
	if err != nil {
		t.Fatal(err)
	}
	system, _ := windows.GetSystemDirectory()
	if !strings.EqualFold(p.vmconnect, filepath.Join(system, "vmconnect.exe")) || !strings.EqualFold(filepath.Base(p.bin), "HyperHand") {
		t.Fatalf("paths: %+v", p)
	}
	if got := serviceCommand(p.hostExe); got != `"`+p.hostExe+`" service` {
		t.Fatalf("service command %q", got)
	}
}

// withTaskScheduler runs f with a Task Scheduler connection on a locked thread.
func withTaskScheduler(t *testing.T, f func(*taskScheduler)) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ts, err := connectTaskScheduler()
	if err != nil {
		t.Fatal(err)
	}
	defer ts.close()
	f(ts)
}

// Task Scheduler checks both definitions against its schema without registering them (TASK_VALIDATE_ONLY).
func TestTaskXMLValidates(t *testing.T) {
	owner, err := setupOwner(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := installPaths()
	if err != nil {
		t.Fatal(err)
	}
	withTaskScheduler(t, func(ts *taskScheduler) {
		const taskValidateOnly = 1
		for name, x := range map[string]string{
			"logon":   logonTaskXML(`C:\Program Files\HyperHand & <test>\hyperhand.exe`, owner),
			"console": consoleTaskXML(p.hostExe, owner),
		} {
			r, err := oleutil.CallMethod(ts.folder, "RegisterTask", "HyperHand validation", x, taskValidateOnly, nil, nil, taskLogonInteractiveToken, nil)
			if err != nil {
				t.Errorf("%s task XML: %v", name, err)
				continue
			}
			r.Clear()
		}
		if tk, err := ts.task("HyperHand validation"); err != nil || tk != nil {
			t.Fatalf("validation registered a task: %v %v", tk, err)
		}
	})
}

// Reads the installed tasks, if any, the way setup checks them; a missing task is nil without an error.
func TestTaskInspectionReadOnly(t *testing.T) {
	owner, err := setupOwner(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := installPaths()
	if err != nil {
		t.Fatal(err)
	}
	withTaskScheduler(t, func(ts *taskScheduler) {
		if tk, err := ts.task("HyperHand task that does not exist"); err != nil || tk != nil {
			t.Fatalf("missing task: %v %v", tk, err)
		}
		tk, err := ts.task(logonTask)
		if err != nil {
			t.Fatal(err)
		}
		if tk == nil {
			t.Skip("HyperHand is not installed")
		}
		if tk.userSID != owner.SID || len(tk.actions) != 1 || !strings.EqualFold(tk.actions[0].path, p.hostExe) ||
			tk.actions[0].args != "" || tk.runLevel != taskRunLevelLUA || tk.triggers != 1 {
			t.Errorf("logon task: %+v", tk)
		}
		c, err := ts.task(consoleTask)
		if err != nil {
			t.Fatal(err)
		}
		if !recognizedConsoleTask(c, p, owner.SID) || c.runLevel != 1 || c.triggers != 0 {
			t.Errorf("console task: %+v", c)
		}
	})
}

// Reads the installed service and its Hyper-V Administrators membership the way setup verifies them.
func TestServiceInspectionReadOnly(t *testing.T) {
	m, err := mgr.Connect()
	if err != nil {
		t.Skipf("service manager: %v", err)
	}
	defer m.Disconnect()
	h, err := windows.OpenService(m.Handle, windows.StringToUTF16Ptr(broker.ServiceName), windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		t.Skip("HyperHand is not installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	s := &mgr.Service{Name: broker.ServiceName, Handle: h}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := installPaths()
	if !strings.EqualFold(c.BinaryPathName, serviceCommand(p.hostExe)) || !strings.EqualFold(c.ServiceStartName, serviceAccount) ||
		c.StartType != mgr.StartAutomatic || c.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
		t.Errorf("service config: %+v", c)
	}
	sid, _, _, err := windows.LookupSID("", serviceAccount)
	if err != nil {
		t.Fatal(err)
	}
	group, _ := windows.StringToSid("S-1-5-32-578")
	name, _, _, err := group.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	in, err := groupHasMember(windows.StringToUTF16Ptr(name), sid)
	if err != nil || !in {
		t.Fatalf("service SID in %s: %v %v", name, in, err)
	}
	user, _ := windows.GetCurrentProcessToken().GetTokenUser()
	if in, err := groupHasMember(windows.StringToUTF16Ptr(name), user.User.Sid); err != nil || in {
		t.Fatalf("the user must not be a Hyper-V administrator through HyperHand: %v %v", in, err)
	}
}

// The service restarts 5, 30 and 60 seconds after its first, second and third failure; the count resets after a day.
func TestServiceRecoveryActions(t *testing.T) {
	want := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	got := serviceRecoveryActions()
	if len(got) != len(want) {
		t.Fatalf("recovery actions: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("recovery action %d: %+v, want %+v", i+1, got[i], want[i])
		}
	}
	if serviceRecoveryResetSeconds != 86400 {
		t.Errorf("reset period: %d", serviceRecoveryResetSeconds)
	}
}

// stopProcesses ends only processes started from the given executable, never this one, and waits for them.
func TestStopProcessesByPath(t *testing.T) {
	if os.Getenv("HYPERHAND_TEST_SLEEP") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestStopProcessesByPath$")
	cmd.Env = append(os.Environ(), "HYPERHAND_TEST_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pid := uint32(cmd.Process.Pid)
	if listed, err := processListed(pid, filepath.Base(self)); err != nil || !listed {
		t.Fatalf("child not listed: %v %v", listed, err)
	}
	if err := stopProcesses(filepath.Join(filepath.Dir(self), "other.exe"), self); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child still running")
	}
	if err := waitProcessGone(pid, filepath.Base(self), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveInstalledFilesChecksHashes(t *testing.T) {
	dir := t.TempDir()
	p := setupPaths{bin: filepath.Join(dir, "bin"), data: filepath.Join(dir, "data")}
	p.hostExe, p.agentExe = filepath.Join(p.bin, "hyperhand.exe"), filepath.Join(p.bin, "hyperhand-agent.exe")
	p.config, p.cleanup = filepath.Join(p.data, "config.json"), filepath.Join(p.data, cleanupExe)
	for _, d := range []string{p.bin, p.data} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(p.hostExe, "host")
	write(p.config, "{}")
	hostHash, _ := fileHash(p.hostExe)
	if h, err := fileHash(p.agentExe); err != nil || h != "" {
		t.Fatalf("missing file hash %q %v", h, err)
	}
	write(p.agentExe, "replaced after uninstall started")
	if err := removeInstalledFiles(p, hostHash, "", false); err == nil {
		t.Fatal("removed a file that changed")
	}
	os.Remove(p.agentExe)
	write(p.hostExe, "host")
	write(filepath.Join(p.data, "kept"), "service data")
	if err := removeInstalledFiles(p, hostHash, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.bin); !os.IsNotExist(err) {
		t.Fatal("empty installation directory kept")
	}
	if _, err := os.Stat(p.config); err != nil {
		t.Fatal("owner marker removed while other data remains")
	}
	os.Remove(filepath.Join(p.data, "kept"))
	if err := removeInstalledFiles(p, hostHash, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.data); !os.IsNotExist(err) {
		t.Fatal("empty data directory kept")
	}
}

func TestCopyProtectedCreatesNewFileOnly(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, _ := windows.GetCurrentProcessToken().GetTokenUser()
	sddl := "D:P(A;;FA;;;" + user.User.Sid.String() + ")"
	if err := copyProtected(src, dst, sddl); err != nil {
		t.Fatal(err)
	}
	a, _ := fileHash(src)
	b, _ := fileHash(dst)
	if a == "" || a != b {
		t.Fatalf("hashes %q %q", a, b)
	}
	if err := copyProtected(src, dst, sddl); err == nil {
		t.Fatal("overwrote an existing file")
	}
}

// The cleanup copy cannot delete itself, so it is always deleted at restart, also when service data is kept.
func TestCleanupCopyDeletedAtRestart(t *testing.T) {
	var scheduled []string
	defer func(f func(string) error) { deleteAtRestart = f }(deleteAtRestart)
	deleteAtRestart = func(path string) error { scheduled = append(scheduled, path); return nil }
	for _, keep := range []bool{true, false} {
		scheduled = nil
		dir := t.TempDir()
		p := setupPaths{bin: filepath.Join(dir, "bin"), data: filepath.Join(dir, "data")}
		p.hostExe, p.agentExe = filepath.Join(p.bin, "hyperhand.exe"), filepath.Join(p.bin, "hyperhand-agent.exe")
		p.config, p.cleanup = filepath.Join(p.data, "config.json"), filepath.Join(p.data, cleanupExe)
		os.Mkdir(p.data, 0o700)
		os.WriteFile(p.config, []byte("{}"), 0o600)
		os.WriteFile(p.cleanup, []byte("copy"), 0o600)
		if keep {
			os.Mkdir(filepath.Join(p.data, "service-data"), 0o700)
		}
		if err := removeInstalledFiles(p, "", "", true); err != nil {
			t.Fatal(err)
		}
		if len(scheduled) == 0 || scheduled[0] != p.cleanup {
			t.Errorf("service data kept %v: deleted at restart %v", keep, scheduled)
		}
		if _, err := os.Stat(p.config); keep != (err == nil) {
			t.Errorf("service data kept %v: owner marker present %v", keep, err == nil)
		}
	}
}

// TerminateProcess fails with ERROR_ACCESS_DENIED for a process that has already exited, for example a tray the
// logon task's Stop ended a moment before; that counts as stopped.
func TestStopProcessAlreadyExited(t *testing.T) {
	if os.Getenv("HYPERHAND_TEST_SLEEP") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestStopProcessAlreadyExited$")
	cmd.Env = append(os.Environ(), "HYPERHAND_TEST_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	cmd.Process.Kill()
	cmd.Wait()
	if err := stopProcess(h); err != nil {
		t.Fatal(err)
	}
}

// A process that has exited while another process still holds a handle to it can still be opened, but its image
// name cannot be read (ERROR_GEN_FAILURE); it counts as gone, not as a process setup cannot check.
func TestOpenProcessWithPathExited(t *testing.T) {
	if os.Getenv("HYPERHAND_TEST_SLEEP") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestOpenProcessWithPathExited$")
	cmd.Env = append(os.Environ(), "HYPERHAND_TEST_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := uint32(cmd.Process.Pid)
	held, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(held) // keeps the exited process object, as the task scheduler's handle may
	if h, err := openProcessWithPath(pid, windows.PROCESS_TERMINATE, self); err != nil || h == 0 {
		t.Fatalf("running child: %v %v", h, err)
	} else {
		windows.CloseHandle(h)
	}
	cmd.Process.Kill()
	cmd.Wait()
	h, err := openProcessWithPath(pid, windows.PROCESS_TERMINATE, self)
	if err != nil || h != 0 {
		t.Fatalf("exited child: %v %v", h, err)
	}
}

func TestWriteProtectedCreatesNewFileOnly(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst")
	user, _ := windows.GetCurrentProcessToken().GetTokenUser()
	sddl := "D:P(A;;FA;;;" + user.User.Sid.String() + ")"
	if err := writeProtected(dst, sddl, []byte("agent")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "agent" {
		t.Fatalf("content %q %v", b, err)
	}
	if err := writeProtected(dst, sddl, []byte("other")); err == nil {
		t.Fatal("overwrote an existing file")
	}
}

func TestUninstallEntry(t *testing.T) {
	p := setupPaths{bin: `C:\Program Files\HyperHand`, hostExe: `C:\Program Files\HyperHand\hyperhand.exe`}
	strs, dwords := uninstallEntry(p, "0.6.0", 15700)
	want := map[string]string{
		"DisplayName":          "HyperHand",
		"DisplayVersion":       "0.6.0",
		"Publisher":            "n2ns",
		"InstallLocation":      `C:\Program Files\HyperHand`,
		"DisplayIcon":          `C:\Program Files\HyperHand\hyperhand.exe`,
		"UninstallString":      `"C:\Program Files\HyperHand\hyperhand.exe" uninstall`,
		"QuietUninstallString": `"C:\Program Files\HyperHand\hyperhand.exe" uninstall --quiet`,
		"URLInfoAbout":         "https://github.com/n2ns/hyper-hand",
	}
	if !maps.Equal(strs, want) {
		t.Errorf("strings %v", strs)
	}
	if !maps.Equal(dwords, map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": 15700}) {
		t.Errorf("dwords %v", dwords)
	}
}

func TestParseUninstallArgs(t *testing.T) {
	quiet, rest := parseUninstallArgs([]string{"--quiet", "--owner-sid", "S-1-5-21-1", "--owner-user", `PC\me`})
	if !quiet || !slices.Equal(rest, []string{"--owner-sid", "S-1-5-21-1", "--owner-user", `PC\me`}) {
		t.Errorf("quiet %v rest %v", quiet, rest)
	}
	if quiet, rest := parseUninstallArgs(nil); quiet || len(rest) != 0 {
		t.Errorf("no args: %v %v", quiet, rest)
	}
}

// A plain go build (tests included) embeds only the directory's README, so install needs the agent beside it.
func TestPlainBuildEmbedsNoAgent(t *testing.T) {
	if data, err := embeddedAgent(); err != nil || data != nil {
		t.Errorf("embedded agent %d bytes, %v", len(data), err)
	}
}

package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"hyperhand/internal/broker"
)

// serviceRecoveryResetSeconds is how long the service must run without failing before the service control manager
// resets its failure count to zero.
const serviceRecoveryResetSeconds = 24 * 60 * 60

// serviceRecoveryActions restarts the service after its first, second and third (and every later) failure.
func serviceRecoveryActions() []mgr.RecoveryAction {
	return []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
}

// installService creates or updates the broker service: automatic start, the virtual account, an unrestricted
// service SID, restart on failure, and a DACL that lets the owner only query and start it.
func installService(m *mgr.Mgr, hostExe, ownerSID string, exists bool) error {
	c := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: serviceAccount, // a virtual account: no password (mgr passes NULL for "")
		DisplayName:      broker.ServiceName,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}
	var s *mgr.Service
	var err error
	if exists {
		if s, err = m.OpenService(broker.ServiceName); err != nil {
			return err
		}
		c.BinaryPathName = serviceCommand(hostExe)
		err = s.UpdateConfig(c)
	} else {
		s, err = m.CreateService(broker.ServiceName, hostExe, c, "service")
	}
	if err != nil {
		if s != nil {
			s.Close()
		}
		return err
	}
	defer s.Close()
	if err := s.SetRecoveryActions(serviceRecoveryActions(), serviceRecoveryResetSeconds); err != nil {
		return fmt.Errorf("set service recovery actions: %w", err)
	}
	// 0x20015: READ_CONTROL, SERVICE_QUERY_CONFIG, SERVICE_QUERY_STATUS and SERVICE_START.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x20015;;;" + ownerSID + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(s.Handle, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return err
	}
	got, err := s.Config()
	if err != nil {
		return err
	}
	if !strings.EqualFold(got.ServiceStartName, serviceAccount) || !strings.EqualFold(got.BinaryPathName, serviceCommand(hostExe)) ||
		got.StartType != mgr.StartAutomatic || got.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
		return errors.New("service account, executable or automatic startup verification failed")
	}
	return nil
}

// stopService stops the broker service if it exists and waits until its process has exited, so its executable can
// be replaced. The process runs as the service account; waiting for it to exit needs no access to it, unlike
// ending it, which administrators may be denied.
func stopService(m *mgr.Mgr) error {
	s, err := m.OpenService(broker.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	pid := st.ProcessId
	if st.State != svc.Stopped && st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
	}
	if err := waitServiceState(s, svc.Stopped, 60*time.Second); err != nil {
		return err
	}
	if pid == 0 {
		return nil
	}
	return waitProcessGone(pid, "hyperhand.exe", 30*time.Second)
}

func startService(m *mgr.Mgr) error {
	s, err := m.OpenService(broker.ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()
	// A recovery restart the service control manager scheduled after a failure can start the service first.
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return waitServiceState(s, svc.Running, 60*time.Second)
}

func waitServiceState(s *mgr.Service, want svc.State, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); ; time.Sleep(200 * time.Millisecond) {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if want == svc.Running && st.State == svc.Stopped {
			return fmt.Errorf("%s stopped while starting (exit code %d, service code %d)", broker.ServiceName, st.Win32ExitCode, st.ServiceSpecificExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not reach state %d", broker.ServiceName, want)
		}
	}
}

func deleteService(m *mgr.Mgr) error {
	s, err := m.OpenService(broker.ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Delete()
}

var (
	netapi32                    = windows.NewLazySystemDLL("netapi32.dll")
	procNetLocalGroupAddMembers = netapi32.NewProc("NetLocalGroupAddMembers")
	procNetLocalGroupDelMembers = netapi32.NewProc("NetLocalGroupDelMembers")
	procNetLocalGroupGetMembers = netapi32.NewProc("NetLocalGroupGetMembers")
)

// localGroupMembersInfo0 is LOCALGROUP_MEMBERS_INFO_0.
type localGroupMembersInfo0 struct{ sid *windows.SID }

// setGroupMember adds member to, or removes it from, the local Hyper-V Administrators group (S-1-5-32-578, whose
// name is localized), and checks the result.
func setGroupMember(member *windows.SID, present bool) error {
	groupSID, err := windows.StringToSid("S-1-5-32-578")
	if err != nil {
		return err
	}
	name, _, _, err := groupSID.LookupAccount("")
	if err != nil {
		return err
	}
	group, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	info := localGroupMembersInfo0{member}
	if present {
		r, _, _ := procNetLocalGroupAddMembers.Call(0, uintptr(unsafe.Pointer(group)), 0, uintptr(unsafe.Pointer(&info)), 1)
		if r != 0 && windows.Errno(r) != windows.ERROR_MEMBER_IN_ALIAS {
			return fmt.Errorf("add to %s: %w", name, windows.Errno(r))
		}
	} else {
		r, _, _ := procNetLocalGroupDelMembers.Call(0, uintptr(unsafe.Pointer(group)), 0, uintptr(unsafe.Pointer(&info)), 1)
		if r != 0 && windows.Errno(r) != windows.ERROR_MEMBER_NOT_IN_ALIAS {
			return fmt.Errorf("remove from %s: %w", name, windows.Errno(r))
		}
	}
	found, err := groupHasMember(group, member)
	if err != nil {
		return err
	}
	if found != present {
		return errors.New("Hyper-V group membership verification failed")
	}
	return nil
}

func groupHasMember(group *uint16, member *windows.SID) (bool, error) {
	const maxPreferredLength = 0xFFFFFFFF
	var resume uintptr
	found := false
	for {
		var buf *byte
		var read, total uint32
		r, _, _ := procNetLocalGroupGetMembers.Call(0, uintptr(unsafe.Pointer(group)), 0, uintptr(unsafe.Pointer(&buf)),
			maxPreferredLength, uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&resume)))
		if buf != nil { // freed even after ERROR_MORE_DATA
			for _, e := range unsafe.Slice((*localGroupMembersInfo0)(unsafe.Pointer(buf)), read) {
				found = found || e.sid.Equals(member)
			}
			windows.NetApiBufferFree(buf)
		}
		switch windows.Errno(r) {
		case 0:
			return found, nil
		case windows.ERROR_MORE_DATA:
			continue
		default:
			return false, windows.Errno(r)
		}
	}
}

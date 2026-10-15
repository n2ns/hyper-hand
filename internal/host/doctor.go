package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"hyperhand/internal/broker"
	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

// MCPURL is the URL this process serves MCP on, set by the tray before NewServer; vm_doctor reports it.
var MCPURL string

// doctorCheck is one line of vm_doctor's report. Status is ok, warn or fail; Suggestion is a concrete command or tool
// call that fixes a warn or fail.
type doctorCheck struct {
	Check      string `json:"check"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion,omitempty"`
}

const (
	doctorOK   = "ok"
	doctorWarn = "warn"
	doctorFail = "fail"
)

// doctor runs the host checks (each self-contained) and the guest checks for one VM. The fields are set by newDoctor
// and replaced by tests.
type doctor struct {
	host []func() doctorCheck
	find func(string) (hyperv.VM, error)
	call agentCall
}

func newDoctor(b Backend, call agentCall) doctor {
	return doctor{
		host: []func() doctorCheck{checkService, checkMCP, checkPipe, checkSocketRegistration, checkEnhancedSession},
		find: b.Find,
		call: call,
	}
}

// run performs every check; it never changes anything. Guest checks that need the agent are skipped (not reported)
// when the VM is not running, and the session and integrity checks when the agent does not answer.
func (dr doctor) run(ctx context.Context, vm string) []doctorCheck {
	out := make([]doctorCheck, 0, 9)
	for _, f := range dr.host {
		out = append(out, f())
	}
	v, err := dr.find(vm)
	if err != nil {
		return append(out, doctorCheck{Check: "vm.power", Status: doctorFail, Detail: err.Error(), Suggestion: "call vm_list and pass one of its names as vm"})
	}
	if v.State != "Running" {
		return append(out, doctorCheck{Check: "vm.power", Status: doctorFail, Detail: fmt.Sprintf("VM %s is %s", v.Name, powerState(v.State)), Suggestion: fmt.Sprintf("call vm_start with vm %q", v.Name)})
	}
	out = append(out, doctorCheck{Check: "vm.power", Status: doctorOK, Detail: fmt.Sprintf("VM %s is running", v.Name)})
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	var p proto.PingResult
	_, err = dr.call(pctx, v.Name, proto.OpPing, nil, nil, &p)
	cancel()
	if err != nil {
		return append(out, doctorCheck{Check: "guest.agent", Status: doctorFail, Detail: "the guest agent does not answer: " + err.Error(),
			Suggestion: "call vm_start (it waits for the agent and unlocks the session); if the agent is not installed, call vm_install_agent"})
	}
	agent := doctorCheck{Check: "guest.agent", Status: doctorOK, Detail: fmt.Sprintf("agent %s, protocol %d, on %s as %s", p.Version, p.Protocol, p.Hostname, p.User)}
	if p.Protocol < proto.Protocol {
		agent.Status, agent.Suggestion = doctorFail, "call vm_update_agent"
		agent.Detail = fmt.Sprintf("agent %s speaks protocol %d; this host needs %d", p.Version, p.Protocol, proto.Protocol)
	}
	out = append(out, agent)
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	var s proto.SessionStateResult
	_, err = dr.call(sctx, v.Name, proto.OpSessionState, nil, nil, &s)
	cancel()
	switch {
	case err != nil:
		out = append(out, doctorCheck{Check: "guest.session", Status: doctorFail, Detail: "session state unavailable: " + err.Error(), Suggestion: "call vm_update_agent"})
	case !s.Console:
		out = append(out, doctorCheck{Check: "guest.session", Status: doctorFail, Detail: "the agent's session is not the VM console session (an enhanced session, remote desktop or another user's session): host screenshots and input do not reach it",
			Suggestion: "call vm_take_console (it disconnects the enhanced or remote desktop viewer); to keep it from recurring, switch View > Enhanced Session off in VMConnect"})
	case s.Consent:
		out = append(out, doctorCheck{Check: "guest.session", Status: doctorWarn, Detail: "a UAC prompt is open; keyboard input goes to the secure desktop", Suggestion: "call vm_observe to see the prompt, then vm_key with alt+y or esc"})
	case s.Locked:
		out = append(out, doctorCheck{Check: "guest.session", Status: doctorWarn, Detail: "the session is locked", Suggestion: "call vm_unlock (needs the unlock password stored in the HyperHand tray)"})
	default:
		out = append(out, doctorCheck{Check: "guest.session", Status: doctorOK, Detail: "console session, unlocked"})
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var ws proto.WindowsResult
	_, err = dr.call(wctx, v.Name, proto.OpListWindows, nil, nil, &ws)
	cancel()
	switch {
	case err != nil:
		out = append(out, doctorCheck{Check: "guest.integrity", Status: doctorWarn, Detail: "window list unavailable: " + err.Error(), Suggestion: "call vm_update_agent"})
	case ws.AgentIntegrity == "":
		out = append(out, doctorCheck{Check: "guest.integrity", Status: doctorWarn, Detail: "the agent's integrity level is unknown", Suggestion: "call vm_update_agent"})
	default:
		out = append(out, doctorCheck{Check: "guest.integrity", Status: doctorOK, Detail: fmt.Sprintf("the agent runs at %s integrity; windows above it are refused with integrity_mismatch", ws.AgentIntegrity)})
	}
	return out
}

// checkService reports the state of HyperHandService from the service control manager.
func checkService() doctorCheck {
	c := doctorCheck{Check: "host.service"}
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return serviceError(c, err)
	}
	defer windows.CloseServiceHandle(h)
	name, _ := windows.UTF16PtrFromString(broker.ServiceName)
	sh, err := windows.OpenService(h, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			c.Status, c.Detail, c.Suggestion = doctorFail, broker.ServiceName+" is not installed", "run hyperhand.exe install (elevated)"
			return c
		}
		return serviceError(c, err)
	}
	s := &mgr.Service{Name: broker.ServiceName, Handle: sh}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return serviceError(c, err)
	}
	if st.State == svc.Running {
		c.Status, c.Detail = doctorOK, broker.ServiceName+" is running"
		return c
	}
	c.Status, c.Detail, c.Suggestion = doctorFail, fmt.Sprintf("%s is in state %d, not running", broker.ServiceName, st.State), "run Start-Service "+broker.ServiceName+" in an elevated PowerShell, or hyperhand.exe install"
	return c
}

func serviceError(c doctorCheck, err error) doctorCheck {
	c.Status, c.Detail = doctorFail, "cannot query "+broker.ServiceName+": "+err.Error()
	c.Suggestion = "run hyperhand.exe install (elevated)"
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		c.Status, c.Suggestion = doctorWarn, "run Get-Service "+broker.ServiceName+" in PowerShell to see its state"
	}
	return c
}

// checkMCP reports the MCP listener: this process answers the call, so it is up.
func checkMCP() doctorCheck {
	c := doctorCheck{Check: "host.mcp", Status: doctorOK, Detail: "the MCP server answered this call"}
	if MCPURL != "" {
		c.Detail = "serving " + MCPURL
	}
	return c
}

// checkPipe dials the service pipe (connectivity only, no request).
func checkPipe() doctorCheck {
	c := doctorCheck{Check: "host.pipe"}
	timeout := 2 * time.Second
	conn, err := winio.DialPipe(broker.PipePath, &timeout)
	if err != nil {
		c.Status, c.Detail = doctorFail, "the service pipe is not reachable: "+err.Error()
		c.Suggestion = "start " + broker.ServiceName + " (Start-Service " + broker.ServiceName + " elevated), or run hyperhand.exe install"
		return c
	}
	conn.Close()
	c.Status, c.Detail = doctorOK, broker.PipePath+" accepts connections"
	return c
}

// checkSocketRegistration reports whether the agent's Hyper-V socket service ID is registered on the host.
func checkSocketRegistration() doctorCheck {
	c := doctorCheck{Check: "host.hvsocket"}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\`+proto.ServiceID, registry.QUERY_VALUE)
	if err != nil {
		c.Status, c.Detail = doctorFail, "Hyper-V socket service "+proto.ServiceID+" is not registered: "+err.Error()
		c.Suggestion = "run hyperhand.exe install (elevated); the host cannot connect to the guest agent until then"
		return c
	}
	k.Close()
	c.Status, c.Detail = doctorOK, "Hyper-V socket service "+proto.ServiceID+" is registered"
	return c
}

// checkEnhancedSession warns when the host allows enhanced session mode: VMConnect may then move the guest session off
// the console that HyperHand's screenshots and input use.
func checkEnhancedSession() doctorCheck {
	c := doctorCheck{Check: "host.enhanced_session", Status: doctorOK, Detail: "enhanced session mode is not allowed on this host"}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization`, registry.QUERY_VALUE)
	if err != nil {
		return c
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("EnhancedMode"); err == nil && v != 0 {
		c.Status, c.Detail = doctorWarn, "this host allows enhanced session mode: if VMConnect opens an enhanced session, the guest session leaves the console and HyperHand sees the lock screen"
		c.Suggestion = "in VMConnect switch View > Enhanced Session off, or turn off \"Allow enhanced session mode\" in the Hyper-V host settings"
	}
	return c
}

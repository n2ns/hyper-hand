package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
)

// Task Scheduler constants (taskschd.h).
const (
	taskCreateOrUpdate        = 6 // TASK_CREATE_OR_UPDATE
	taskLogonInteractiveToken = 3 // TASK_LOGON_INTERACTIVE_TOKEN
	taskRunLevelLUA           = 0 // TASK_RUNLEVEL_LUA
)

// taskScheduler is a connection to the Task Scheduler service's root folder. COM must stay on the calling thread,
// which the caller locks.
type taskScheduler struct {
	service, folder *ole.IDispatch
	uninit          bool
}

func connectTaskScheduler() (*taskScheduler, error) {
	ts := &taskScheduler{}
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 { // S_FALSE = already initialized
			return nil, err
		}
	}
	ts.uninit = true
	unk, err := oleutil.CreateObject("Schedule.Service")
	if err != nil {
		ts.close()
		return nil, err
	}
	ts.service, err = unk.QueryInterface(ole.IID_IDispatch)
	unk.Release()
	if err != nil {
		ts.close()
		return nil, err
	}
	if _, err := oleutil.CallMethod(ts.service, "Connect"); err != nil {
		ts.close()
		return nil, err
	}
	folder, err := oleutil.CallMethod(ts.service, "GetFolder", `\`)
	if err != nil {
		ts.close()
		return nil, err
	}
	ts.folder = folder.ToIDispatch()
	return ts, nil
}

func (ts *taskScheduler) close() {
	if ts.folder != nil {
		ts.folder.Release()
	}
	if ts.service != nil {
		ts.service.Release()
	}
	if ts.uninit {
		ole.CoUninitialize()
	}
}

type taskAction struct{ path, args string }

// taskInfo is the part of a registered task's definition that setup checks.
type taskInfo struct {
	enabled  bool
	userSID  string
	runLevel int64
	actions  []taskAction
	triggers int64
}

// task reads the registered task name in the root folder, or returns nil if there is none.
func (ts *taskScheduler) task(name string) (*taskInfo, error) {
	t, err := oleutil.CallMethod(ts.folder, "GetTask", name)
	if err != nil {
		if oleCode(err) == 0x80070002 { // HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND)
			return nil, nil
		}
		return nil, err
	}
	defer t.Clear()
	var info taskInfo
	enabled, err := oleutil.GetProperty(t.ToIDispatch(), "Enabled")
	if err != nil {
		return nil, err
	}
	info.enabled = enabled.Val != 0
	enabled.Clear()
	get := func(obj *ole.IDispatch, prop string) (*ole.VARIANT, error) { return oleutil.GetProperty(obj, prop) }
	def, err := get(t.ToIDispatch(), "Definition")
	if err != nil {
		return nil, err
	}
	defer def.Clear()
	principal, err := get(def.ToIDispatch(), "Principal")
	if err != nil {
		return nil, err
	}
	defer principal.Clear()
	user, err := get(principal.ToIDispatch(), "UserId")
	if err != nil {
		return nil, err
	}
	info.userSID = user.ToString()
	user.Clear()
	if !strings.HasPrefix(info.userSID, "S-1-") {
		sid, _, _, err := windows.LookupSID("", info.userSID)
		if err != nil {
			return nil, err
		}
		info.userSID = sid.String()
	}
	level, err := get(principal.ToIDispatch(), "RunLevel")
	if err != nil {
		return nil, err
	}
	info.runLevel = level.Val
	level.Clear()
	triggers, err := get(def.ToIDispatch(), "Triggers")
	if err != nil {
		return nil, err
	}
	defer triggers.Clear()
	count, err := get(triggers.ToIDispatch(), "Count")
	if err != nil {
		return nil, err
	}
	info.triggers = count.Val
	count.Clear()
	actions, err := get(def.ToIDispatch(), "Actions")
	if err != nil {
		return nil, err
	}
	defer actions.Clear()
	if count, err = get(actions.ToIDispatch(), "Count"); err != nil {
		return nil, err
	}
	n := count.Val
	count.Clear()
	for i := int64(1); i <= n; i++ { // collections are 1-based
		item, err := oleutil.GetProperty(actions.ToIDispatch(), "Item", i)
		if err != nil {
			return nil, err
		}
		var a taskAction
		for _, p := range []struct {
			name string
			dst  *string
		}{{"Path", &a.path}, {"Arguments", &a.args}} {
			v, err := get(item.ToIDispatch(), p.name)
			if err != nil {
				item.Clear()
				return nil, fmt.Errorf("task %s action %d is not a program: %w", name, i, err)
			}
			*p.dst = v.ToString()
			v.Clear()
		}
		item.Clear()
		info.actions = append(info.actions, a)
	}
	return &info, nil
}

// register creates or replaces the task name from its XML definition. The principal is in the XML, so no user or
// password is passed (both must be null together).
func (ts *taskScheduler) register(name, xmlText string) error {
	t, err := oleutil.CallMethod(ts.folder, "RegisterTask", name, xmlText, taskCreateOrUpdate, nil, nil, taskLogonInteractiveToken, nil)
	if err != nil {
		return fmt.Errorf("register task %s: %w", name, err)
	}
	t.Clear()
	return nil
}

// stop stops all running instances of the task.
func (ts *taskScheduler) stop(name string) error {
	return ts.call(name, "Stop", 0)
}

// run starts the task without parameters.
func (ts *taskScheduler) run(name string) error {
	return ts.call(name, "Run", nil)
}

func (ts *taskScheduler) setEnabled(name string, enabled bool) error {
	t, err := oleutil.CallMethod(ts.folder, "GetTask", name)
	if err != nil {
		return err
	}
	defer t.Clear()
	r, err := oleutil.PutProperty(t.ToIDispatch(), "Enabled", enabled)
	if err != nil {
		return err
	}
	r.Clear()
	return nil
}

func (ts *taskScheduler) call(name, method string, arg any) error {
	t, err := oleutil.CallMethod(ts.folder, "GetTask", name)
	if err != nil {
		return err
	}
	defer t.Clear()
	r, err := oleutil.CallMethod(t.ToIDispatch(), method, arg)
	if err != nil {
		return fmt.Errorf("%s task %s: %w", method, name, err)
	}
	r.Clear()
	return nil
}

func (ts *taskScheduler) delete(name string) error {
	if _, err := oleutil.CallMethod(ts.folder, "DeleteTask", name, 0); err != nil {
		return fmt.Errorf("delete task %s: %w", name, err)
	}
	return nil
}

// logonTaskXML defines the tray task: hyperhand.exe at the owner's logon, with the owner's limited token.
func logonTaskXML(hostExe string, owner setupIdentity) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlEscape(owner.SID) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
  </Settings>
  <Triggers>
    <LogonTrigger>
      <UserId>` + xmlEscape(owner.User) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(hostExe) + `</Command>
    </Exec>
  </Actions>
</Task>`
}

// consoleTaskXML defines consoleTask: no trigger, run on demand with mode:VM-GUID as $(Arg0), with the owner's full
// token, in parallel for several VMs.
const consoleTaskArgs = `console "$(Arg0)"`

func consoleTaskXML(hostExe string, owner setupIdentity) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlEscape(owner.SID) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <MultipleInstancesPolicy>Parallel</MultipleInstancesPolicy>
  </Settings>
  <Triggers />
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(hostExe) + `</Command>
      <Arguments>` + xmlEscape(consoleTaskArgs) + `</Arguments>
    </Exec>
  </Actions>
</Task>`
}

// oleCode is the HRESULT of a failed IDispatch call: an object's own error comes back as DISP_E_EXCEPTION, with
// the HRESULT in the EXCEPINFO that go-ole keeps as the sub-error.
func oleCode(err error) uint32 {
	const dispEException = 0x80020009
	var oe *ole.OleError
	if !errors.As(err, &oe) {
		return 0
	}
	if ei, ok := oe.SubError().(ole.EXCEPINFO); ok && oe.Code() == dispEException {
		return ei.SCODE()
	}
	return uint32(oe.Code())
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

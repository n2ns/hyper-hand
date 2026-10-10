// Package hyperv controls a Hyper-V VM from the host through WMI (root\virtualization\v2): screen, mouse, keyboard,
// state, and copying a file into the guest. Needs Hyper-V management rights. vm is the VM name; "" means the only running VM
// (or the only VM if none is running).
package hyperv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
)

// inputMu serializes mouse and keyboard input so concurrent tool calls do not interleave.
var inputMu sync.Mutex

type VM struct {
	Name  string `json:"name"`
	ID    string `json:"id"` // Msvm_ComputerSystem.Name, used for the Hyper-V socket
	State string `json:"state"`
}

func ListVMs() ([]VM, error) {
	var vms []VM
	err := withWMI(func(s *session) error {
		objs, err := s.vmObjects()
		for _, o := range objs {
			vms = append(vms, s.vmInfo(o))
		}
		return err
	})
	return vms, err
}

// Find resolves a VM name ("" = the only running VM, or the only VM if none is running).
func Find(vm string) (VM, error) {
	var v VM
	err := withWMI(func(s *session) error {
		o, err := s.find(vm)
		if err == nil {
			v = s.vmInfo(o)
		}
		return err
	})
	return v, err
}

// Screenshot returns the VM screen as PNG at its current resolution (pixel coordinates = mouse coordinates).
func Screenshot(vm string) (pngData []byte, width, height int, err error) {
	err = withWMI(func(s *session) error {
		o, err := s.find(vm)
		if err != nil {
			return err
		}
		heads, err := s.assoc(o, "Msvm_VideoHead")
		if err != nil {
			return err
		}
		if len(heads) == 0 {
			return errors.New("no video head (VM not running?)")
		}
		width, height = toInt(s.get(heads[0], "CurrentHorizontalResolution")), toInt(s.get(heads[0], "CurrentVerticalResolution"))
		settings, err := s.assoc(o, "Msvm_VirtualSystemSettingData")
		if err != nil {
			return err
		}
		var target *ole.IDispatch
		for _, sd := range settings {
			if fmt.Sprint(s.get(sd, "VirtualSystemType")) == "Microsoft:Hyper-V:System:Realized" {
				target = sd
			}
		}
		if target == nil {
			return errors.New("realized system settings not found")
		}
		svc, err := s.one("SELECT * FROM Msvm_VirtualSystemManagementService")
		if err != nil {
			return err
		}
		out, err := s.call(svc, "GetVirtualSystemThumbnailImage", "TargetSystem", s.path(target), "WidthPixels", int32(width), "HeightPixels", int32(height))
		if err != nil {
			return err
		}
		v, err := oleutil.GetProperty(out, "ImageData")
		if err != nil {
			return err
		}
		defer v.Clear()
		arr := v.ToArray()
		if arr == nil {
			return errors.New("no image data")
		}
		data, err := safeArrayBytes(arr)
		if err != nil {
			return err
		}
		img, err := rgb565ToRGBA(data, width, height)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return err
		}
		pngData = buf.Bytes()
		return nil
	})
	return
}

// Modifiers are the key names accepted in Click and Drag modifiers.
var Modifiers = []string{"ctrl", "shift", "alt"}

// ValidateModifiers checks a modifiers list: each must be one of Modifiers, without repeats.
func ValidateModifiers(mods []string) error {
	seen := map[string]bool{}
	for _, m := range mods {
		if !slices.Contains(Modifiers, m) {
			return fmt.Errorf("unknown modifier %q; allowed: %s", m, strings.Join(Modifiers, ", "))
		}
		if seen[m] {
			return fmt.Errorf("modifier %q repeated", m)
		}
		seen[m] = true
	}
	return nil
}

// withModifiers holds the modifier keys on the VM's keyboard while f runs with the mouse device, then releases them
// in reverse order even if f fails.
func withModifiers(vm string, mods []string, f func(*session, *ole.IDispatch) error) error {
	if err := ValidateModifiers(mods); err != nil {
		return err
	}
	return withDevice(vm, "Msvm_Keyboard", func(s *session, k *ole.IDispatch) (err error) {
		o, err := s.find(vm)
		if err != nil {
			return err
		}
		mice, err := s.assoc(o, "Msvm_SyntheticMouse")
		if err != nil {
			return err
		}
		if len(mice) == 0 {
			return fmt.Errorf("Msvm_SyntheticMouse not found (VM not running?)")
		}
		pressed := 0
		defer func() {
			for i := pressed - 1; i >= 0; i-- {
				if _, rerr := s.call(k, "ReleaseKey", "KeyCode", int32(keyNames[mods[i]])); rerr != nil && err == nil {
					err = rerr
				}
			}
		}()
		for _, m := range mods {
			if _, err := s.call(k, "PressKey", "KeyCode", int32(keyNames[m])); err != nil {
				return err
			}
			pressed++
		}
		return f(s, mice[0])
	})
}

// Click: button 1 left, 2 right, 3 middle; count clicks (1 to 3) while the modifiers (ctrl, shift, alt) are held.
func Click(vm string, x, y, button, count int, modifiers []string) error {
	if count < 1 || count > 3 {
		return fmt.Errorf("click count must be 1 to 3")
	}
	inputMu.Lock()
	defer inputMu.Unlock()
	return withModifiers(vm, modifiers, func(s *session, m *ole.IDispatch) error {
		if err := s.move(m, x, y); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		for i := 0; i < count; i++ {
			if _, err := s.call(m, "ClickButton", "ButtonIndex", int32(button)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Drag drags with the left button from (x1, y1) to (x2, y2) while the modifiers are held.
func Drag(vm string, x1, y1, x2, y2 int, modifiers []string) error {
	inputMu.Lock()
	defer inputMu.Unlock()
	return withModifiers(vm, modifiers, func(s *session, m *ole.IDispatch) (err error) {
		if err := s.move(m, x1, y1); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		if _, err := s.call(m, "SetButtonState", "ButtonIndex", int32(1), "IsDown", true); err != nil {
			return err
		}
		defer func() { // always release the button, even if a move failed
			if _, uerr := s.call(m, "SetButtonState", "ButtonIndex", int32(1), "IsDown", false); err == nil {
				err = uerr
			}
		}()
		const steps = 8
		for i := 1; i <= steps; i++ {
			time.Sleep(50 * time.Millisecond)
			if err := s.move(m, x1+(x2-x1)*i/steps, y1+(y2-y1)*i/steps); err != nil {
				return err
			}
		}
		time.Sleep(100 * time.Millisecond)
		return nil
	})
}

// Scroll: positive delta scrolls up.
func Scroll(vm string, x, y, delta int) error {
	inputMu.Lock()
	defer inputMu.Unlock()
	return withDevice(vm, "Msvm_SyntheticMouse", func(s *session, m *ole.IDispatch) error {
		if err := s.move(m, x, y); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		// delta is in wheel notches; Windows uses 120 units per notch.
		_, err := s.call(m, "SetScrollPosition", "ScrollPositionDelta", int32(delta*120))
		return err
	})
}

// TypeText types ASCII text.
func TypeText(vm, text string) error {
	for _, r := range text {
		if r > 127 {
			return fmt.Errorf("TypeText supports ASCII only (got %q)", r)
		}
	}
	inputMu.Lock()
	defer inputMu.Unlock()
	return withDevice(vm, "Msvm_Keyboard", func(s *session, k *ole.IDispatch) error {
		_, err := s.call(k, "TypeText", "AsciiText", text)
		return err
	})
}

// ValidateKeys checks a combination without sending input.
func ValidateKeys(keys string) error {
	_, err := parseKeys(keys)
	return err
}

// PressKeys presses a key or a combination such as "enter", "esc", "f2", "ctrl+v", "win+r", "alt+f4".
func PressKeys(vm, keys string) error {
	codes, err := parseKeys(keys)
	if err != nil {
		return err
	}
	inputMu.Lock()
	defer inputMu.Unlock()
	return withDevice(vm, "Msvm_Keyboard", func(s *session, k *ole.IDispatch) error {
		if len(codes) == 1 {
			_, err := s.call(k, "TypeKey", "KeyCode", int32(codes[0]))
			return err
		}
		var firstErr error
		pressed := 0
		for _, c := range codes {
			if _, err := s.call(k, "PressKey", "KeyCode", int32(c)); err != nil {
				firstErr = err
				break
			}
			pressed++
		}
		for i := pressed - 1; i >= 0; i-- {
			if _, err := s.call(k, "ReleaseKey", "KeyCode", int32(codes[i])); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	})
}

func Start(vm string) error { return requestState(vm, 2) }

func Stop(vm string) error { return requestState(vm, 3) }

// Save saves the VM's memory and device state to disk and stops it (RequestStateChange 6, Saved). Writing the memory
// takes time in proportion to it, so the job is awaited longer than other state changes.
func Save(vm string) error { return requestStateTimeout(vm, 6, saveTimeout) }

// Pause freezes the VM in memory (RequestStateChange 9, Paused).
func Pause(vm string) error { return requestState(vm, 9) }

// saveTimeout bounds the wait for a save job.
const saveTimeout = 5 * time.Minute

// Shutdown asks the guest to shut down through the Hyper-V shutdown integration service and returns once the request
// is accepted, not when the VM is off. It is not forced: a program with unsaved work can keep Windows from shutting
// down. https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/initiateshutdown-msvm-shutdowncomponent
func Shutdown(vm string) error {
	return withDevice(vm, "Msvm_ShutdownComponent", func(s *session, c *ole.IDispatch) error {
		// call fails on any ReturnValue other than 0 (done) and 4096 (accepted).
		if _, err := s.call(c, "InitiateShutdown", "Force", false, "Reason", "HyperHand vm_shutdown"); err != nil {
			return fmt.Errorf("the guest shutdown integration service did not accept the request; is the guest running Windows with the Shutdown integration service enabled? %w", err)
		}
		return nil
	})
}

// CopyToGuest copies a host file into the guest (enables the Guest Service Interface if needed; no guest password).
func CopyToGuest(vm, hostPath, guestPath string) error {
	_, err := vmScript(vm, `$gs=Get-VMIntegrationService -VM $vm | Where-Object { $_.Id -like '*6C09BB55-D683-4DA0-8931-C9BF705F6480' }
if ($gs -and -not $gs.Enabled) { Enable-VMIntegrationService -VMIntegrationService $gs; Start-Sleep -Seconds 3 }
Copy-VMFile -VM $vm -SourcePath `+psq(hostPath)+` -DestinationPath `+psq(guestPath)+` -CreateFullPath -FileSource Host -Force`)
	return err
}

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// vmScript runs a PowerShell script with $vm set to the VM (looked up by Id) and returns its UTF-8 output.
func vmScript(vm, script string) ([]byte, error) {
	v, err := Find(vm)
	if err != nil {
		return nil, err
	}
	script = "[Console]::OutputEncoding=[Text.Encoding]::UTF8; $ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; $vm=Get-VM -Id " + psq(v.ID) + "\n" + script
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW} // no console flash (the tray is a GUI exe)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("powershell: %v: %s", err, strings.TrimSpace(stderr.String()+"\n"+string(out)))
	}
	return out, nil
}

func requestState(vm string, state int32) error {
	return requestStateTimeout(vm, state, 45*time.Second)
}

func requestStateTimeout(vm string, state int32, timeout time.Duration) error {
	return withWMI(func(s *session) error {
		o, err := s.find(vm)
		if err != nil {
			return err
		}
		out, err := s.call(o, "RequestStateChange", "RequestedState", state)
		if err != nil {
			return err
		}
		return s.awaitJob(out, "RequestStateChange", timeout)
	})
}

// awaitJob finishes a WMI method call from its out-parameters: ReturnValue 0 is done; 4096 means a job started, which
// is polled until it completes (or timeout passes, in which case the operation may still be running).
func (s *session) awaitJob(out *ole.IDispatch, method string, timeout time.Duration) error {
	if toInt(s.get(out, "ReturnValue")) != 4096 {
		return nil
	}
	path, _ := s.get(out, "Job").(string)
	if path == "" {
		return fmt.Errorf("%s started asynchronously without a job reference", method)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return waitStateJob(ctx, 500*time.Millisecond, func() (stateJob, error) {
		return s.readStateJob(path)
	})
}

type stateJob struct {
	State       int
	ErrorCode   int
	Description string
}

// A method's ReturnValue 4096 means only that an operation started. Never replay the
// request: poll its Msvm_ConcreteJob and require Completed with ErrorCode zero.
// https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/msvm-concretejob
func waitStateJob(ctx context.Context, interval time.Duration, poll func() (stateJob, error)) error {
	var last stateJob
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("Hyper-V job wait ended (JobState=%d, ErrorCode=%d, ErrorDescription=%q); operation may still be running: %w", last.State, last.ErrorCode, last.Description, err)
		}
		job, err := poll()
		if err != nil {
			return fmt.Errorf("read Hyper-V job: %w", err)
		}
		last = job
		switch job.State {
		case 7: // Completed normally; ErrorCode still determines success.
			if job.ErrorCode == 0 {
				return nil
			}
			fallthrough
		case 8, 9, 10: // Terminated, Killed, Exception.
			return fmt.Errorf("Hyper-V job failed (JobState=%d, ErrorCode=%d, ErrorDescription=%q)", job.State, job.ErrorCode, job.Description)
		case 2, 3, 4, 5, 6, 11: // New, Starting, Running, Suspended, Shutting Down, Service.
		default:
			return fmt.Errorf("Hyper-V job has unexpected JobState=%d (ErrorCode=%d, ErrorDescription=%q)", job.State, job.ErrorCode, job.Description)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *session) readStateJob(path string) (stateJob, error) {
	// Get a fresh WMI object each time; release it before the next poll.
	v, err := oleutil.CallMethod(s.svc, "Get", path)
	if err != nil {
		return stateJob{}, err
	}
	defer v.Clear()
	o := v.ToIDispatch()
	if o == nil {
		return stateJob{}, errors.New("job object is unavailable")
	}
	readNumber := func(name string) (int, error) {
		p, err := oleutil.GetProperty(o, name)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		defer p.Clear()
		if p.Value() == nil {
			return 0, fmt.Errorf("job %s is missing", name)
		}
		return toInt(p.Value()), nil
	}
	var job stateJob
	if job.State, err = readNumber("JobState"); err != nil {
		return job, err
	}
	if job.ErrorCode, err = readNumber("ErrorCode"); err != nil {
		if job.State >= 7 && job.State <= 10 {
			return job, err
		}
		// The provider need not have a final error code while the job is pending.
		job.ErrorCode = -1
	}
	job.Description, _ = s.get(o, "ErrorDescription").(string)
	if job.Description == "" {
		job.Description, _ = s.get(o, "ErrorSummaryDescription").(string)
	}
	return job, nil
}

// ---- WMI plumbing ----

type session struct {
	svc  *ole.IDispatch
	objs []*ole.IDispatch // released when the session ends
}

// withWMI runs f with a fresh WMI connection on a locked OS thread.
func withWMI(f func(*session) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 { // S_FALSE = already initialized
			return err
		}
	}
	defer ole.CoUninitialize()
	s := &session{}
	defer func() {
		for i := len(s.objs) - 1; i >= 0; i-- {
			s.objs[i].Release()
		}
	}()
	unk, err := oleutil.CreateObject("WbemScripting.SWbemLocator")
	if err != nil {
		return err
	}
	defer unk.Release()
	loc, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	s.keep(loc)
	if s.svc, err = s.dispatch(oleutil.CallMethod(loc, "ConnectServer", ".", `root\virtualization\v2`)); err != nil {
		return fmt.Errorf("connect root\\virtualization\\v2: %w", err)
	}
	return f(s)
}

func withDevice(vm, class string, f func(*session, *ole.IDispatch) error) error {
	return withWMI(func(s *session) error {
		o, err := s.find(vm)
		if err != nil {
			return err
		}
		devs, err := s.assoc(o, class)
		if err != nil {
			return err
		}
		if len(devs) == 0 {
			return fmt.Errorf("%s not found (VM not running?)", class)
		}
		return f(s, devs[0])
	})
}

func (s *session) keep(d *ole.IDispatch) *ole.IDispatch { s.objs = append(s.objs, d); return d }

func (s *session) dispatch(v *ole.VARIANT, err error) (*ole.IDispatch, error) {
	if err != nil {
		return nil, err
	}
	return s.keep(v.ToIDispatch()), nil
}

func (s *session) get(o *ole.IDispatch, name string) interface{} {
	v, err := oleutil.GetProperty(o, name)
	if err != nil {
		return nil
	}
	defer v.Clear()
	return v.Value()
}

func (s *session) path(o *ole.IDispatch) string {
	p, err := s.dispatch(oleutil.GetProperty(o, "Path_"))
	if err != nil {
		return ""
	}
	return fmt.Sprint(s.get(p, "Path"))
}

// list collects the items of an SWbemObjectSet.
func (s *session) list(set *ole.IDispatch, err error) ([]*ole.IDispatch, error) {
	if err != nil {
		return nil, err
	}
	var out []*ole.IDispatch
	err = oleutil.ForEach(set, func(v *ole.VARIANT) error {
		out = append(out, s.keep(v.ToIDispatch())) // ForEach does not clear v, so we own the reference
		return nil
	})
	return out, err
}

func (s *session) query(wql string) ([]*ole.IDispatch, error) {
	return s.list(s.dispatch(oleutil.CallMethod(s.svc, "ExecQuery", wql)))
}

func (s *session) one(wql string) (*ole.IDispatch, error) {
	r, err := s.query(wql)
	if err == nil && len(r) == 0 {
		err = fmt.Errorf("no result for %s", wql)
	}
	if err != nil {
		return nil, err
	}
	return r[0], nil
}

func (s *session) assoc(o *ole.IDispatch, resultClass string) ([]*ole.IDispatch, error) {
	return s.list(s.dispatch(oleutil.CallMethod(o, "Associators_", "", resultClass)))
}

// call invokes a WMI method with name/value argument pairs and returns the out-parameters object.
// ReturnValue 0 (done) and 4096 (job started) are success.
func (s *session) call(o *ole.IDispatch, method string, args ...interface{}) (*ole.IDispatch, error) {
	var in *ole.IDispatch
	if len(args) > 0 {
		methods, err := s.dispatch(oleutil.GetProperty(o, "Methods_"))
		if err != nil {
			return nil, err
		}
		m, err := s.dispatch(oleutil.CallMethod(methods, "Item", method))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		params, err := s.dispatch(oleutil.GetProperty(m, "InParameters"))
		if err != nil {
			return nil, err
		}
		if in, err = s.dispatch(oleutil.CallMethod(params, "SpawnInstance_")); err != nil {
			return nil, err
		}
		for i := 0; i+1 < len(args); i += 2 {
			if _, err := oleutil.PutProperty(in, args[i].(string), args[i+1]); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", method, args[i], err)
			}
		}
	}
	var callArgs []interface{}
	callArgs = append(callArgs, method)
	if in != nil {
		callArgs = append(callArgs, in)
	}
	out, err := s.dispatch(oleutil.CallMethod(o, "ExecMethod_", callArgs...))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if rv := toInt(s.get(out, "ReturnValue")); rv != 0 && rv != 4096 {
		return nil, fmt.Errorf("%s returned %d", method, rv)
	}
	return out, nil
}

// vmObjects returns the Msvm_ComputerSystem objects that are VMs (GUID Name), not the host.
func (s *session) vmObjects() ([]*ole.IDispatch, error) {
	all, err := s.query("SELECT * FROM Msvm_ComputerSystem")
	var vms []*ole.IDispatch
	for _, o := range all {
		if name := fmt.Sprint(s.get(o, "Name")); len(name) == 36 && strings.Count(name, "-") == 4 {
			vms = append(vms, o)
		}
	}
	return vms, err
}

func (s *session) vmInfo(o *ole.IDispatch) VM {
	return VM{Name: fmt.Sprint(s.get(o, "ElementName")), ID: fmt.Sprint(s.get(o, "Name")), State: stateName(toInt(s.get(o, "EnabledState")))}
}

func (s *session) find(vm string) (*ole.IDispatch, error) {
	objs, err := s.vmObjects()
	if err != nil {
		return nil, err
	}
	var hits []*ole.IDispatch
	for _, o := range objs {
		if vm == "" && toInt(s.get(o, "EnabledState")) == 2 || vm != "" && strings.EqualFold(fmt.Sprint(s.get(o, "ElementName")), vm) {
			hits = append(hits, o)
		}
	}
	if vm == "" && len(hits) == 0 && len(objs) == 1 { // nothing running but only one VM: use it (e.g. to start it)
		return objs[0], nil
	}
	switch {
	case len(hits) == 1:
		return hits[0], nil
	case vm == "" && len(hits) == 0:
		return nil, errors.New("no running VM")
	case vm == "":
		return nil, errors.New("several VMs are running; specify one by name")
	case len(hits) == 0:
		return nil, fmt.Errorf("VM %q not found", vm)
	default:
		return nil, fmt.Errorf("several VMs are named %q", vm)
	}
}

func (s *session) move(m *ole.IDispatch, x, y int) error {
	_, err := s.call(m, "SetAbsolutePosition", "HorizontalPosition", int32(x), "VerticalPosition", int32(y))
	return err
}

// ---- pure helpers ----

func stateName(st int) string {
	switch st {
	case 2:
		return "Running"
	case 3:
		return "Off"
	case 6, 32769: // Enabled but Offline; the older Suspended value
		return "Saved"
	case 9, 32768: // Quiesce; the older Paused value
		return "Paused"
	}
	return strconv.Itoa(st)
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int8, int16, int32, int64, int, uint8, uint16, uint32, uint64, uint:
		i, _ := strconv.Atoi(fmt.Sprint(n))
		return i
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

var (
	oleaut32                  = windows.NewLazySystemDLL("oleaut32.dll")
	procSafeArrayAccessData   = oleaut32.NewProc("SafeArrayAccessData")
	procSafeArrayUnaccessData = oleaut32.NewProc("SafeArrayUnaccessData")
	procSafeArrayGetElemsize  = oleaut32.NewProc("SafeArrayGetElemsize")
)

// safeArrayBytes reads WMI's uint8[] SAFEARRAY. VT_UI1 and VT_VARIANT arrays are read straight from the array data
// (ToValueArray builds millions of interface values); anything else falls back to ToValueArray.
// ToByteArray misreads WMI's uint8 array (every byte came back as 0x11, the VT_UI1 tag).
func safeArrayBytes(arr *ole.SafeArrayConversion) ([]byte, error) {
	vt, err := arr.GetType()
	if err != nil {
		return nil, err
	}
	n, err := arr.TotalElements(0)
	if err != nil {
		return nil, err
	}
	if (ole.VT(vt) == ole.VT_UI1 || ole.VT(vt) == ole.VT_VARIANT) && n > 0 {
		// Element size: 1, or 16/24 for a VARIANT (32/64-bit). Called directly: go-ole's GetSize returns the size cast
		// to a pointer (dereferencing it crashes).
		es, _, _ := procSafeArrayGetElemsize.Call(uintptr(unsafe.Pointer(arr.Array)))
		if es == 0 {
			return nil, errors.New("SafeArrayGetElemsize returned 0")
		}
		size := &[]uint32{uint32(es)}[0]
		var p unsafe.Pointer
		if hr, _, _ := procSafeArrayAccessData.Call(uintptr(unsafe.Pointer(arr.Array)), uintptr(unsafe.Pointer(&p))); hr != 0 {
			return nil, fmt.Errorf("SafeArrayAccessData: 0x%08x", uint32(hr))
		}
		defer procSafeArrayUnaccessData.Call(uintptr(unsafe.Pointer(arr.Array)))
		raw := unsafe.Slice((*byte)(p), int(n)*int(*size))
		if ole.VT(vt) == ole.VT_UI1 {
			return bytes.Clone(raw), nil
		}
		return variantBytes(raw, int(*size)), nil
	}
	vals := arr.ToValueArray()
	data := make([]byte, len(vals))
	for i, x := range vals {
		if b, ok := x.(uint8); ok {
			data[i] = b
		} else {
			data[i] = byte(toInt(x))
		}
	}
	return data, nil
}

// variantBytes returns the value byte (offset 8) of each elemSize-byte VARIANT in a raw VARIANT array.
func variantBytes(raw []byte, elemSize int) []byte {
	out := make([]byte, len(raw)/elemSize)
	for i := range out {
		out[i] = raw[i*elemSize+8]
	}
	return out
}

// rgb565ToRGBA converts row-major little-endian RGB565 pixels to an RGBA image.
func rgb565ToRGBA(data []byte, w, h int) (*image.RGBA, error) {
	if w <= 0 || h <= 0 || len(data) < w*h*2 {
		return nil, fmt.Errorf("image data has %d bytes, want %d for %dx%d", len(data), w*h*2, w, h)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		p := uint16(data[2*i]) | uint16(data[2*i+1])<<8
		r, g, b := uint8(p>>11), uint8(p>>5&0x3f), uint8(p&0x1f)
		img.SetRGBA(i%w, i/w, color.RGBA{r<<3 | r>>2, g<<2 | g>>4, b<<3 | b>>2, 255})
	}
	return img, nil
}

// keyNames maps key names (lowercase; parseKeys lowercases its input) to Windows virtual-key codes. Besides the plain
// names it accepts X11/Codex-style aliases (Return, Escape, Control_L, KP_0, period...). The Hyper-V keyboard's TypeKey
// takes a virtual-key code without an extended flag, so numenter/KP_Enter is the same key as enter, and the left/right
// modifier aliases are the plain modifier.
var keyNames = map[string]int{
	"ctrl": 0x11, "control": 0x11, "control_l": 0x11, "control_r": 0x11,
	"shift": 0x10, "shift_l": 0x10, "shift_r": 0x10,
	"alt": 0x12, "alt_l": 0x12, "alt_r": 0x12,
	"win": 0x5B, "super_l": 0x5B,
	"enter": 0x0D, "return": 0x0D, "esc": 0x1B, "escape": 0x1B, "tab": 0x09, "space": 0x20,
	"backspace": 0x08, "delete": 0x2E, "del": 0x2E, "insert": 0x2D, "home": 0x24, "end": 0x23,
	"pageup": 0x21, "prior": 0x21, "pagedown": 0x22, "next": 0x22, "up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27,
	"printscreen": 0x2C, "scrolllock": 0x91, "pause": 0x13, "apps": 0x5D, "capslock": 0x14, "numlock": 0x90,
	"num0": 0x60, "num1": 0x61, "num2": 0x62, "num3": 0x63, "num4": 0x64, "num5": 0x65, "num6": 0x66, "num7": 0x67, "num8": 0x68, "num9": 0x69,
	"kp_0": 0x60, "kp_1": 0x61, "kp_2": 0x62, "kp_3": 0x63, "kp_4": 0x64, "kp_5": 0x65, "kp_6": 0x66, "kp_7": 0x67, "kp_8": 0x68, "kp_9": 0x69,
	"numenter": 0x0D, "kp_enter": 0x0D, "numdot": 0x6E, "kp_decimal": 0x6E, "numplus": 0x6B, "kp_add": 0x6B,
	"numminus": 0x6D, "kp_subtract": 0x6D, "nummul": 0x6A, "kp_multiply": 0x6A, "numdiv": 0x6F, "kp_divide": 0x6F,
	";": 0xBA, "=": 0xBB, "plus": 0xBB, "equal": 0xBB, ",": 0xBC, "comma": 0xBC, "-": 0xBD, "minus": 0xBD,
	".": 0xBE, "period": 0xBE, "/": 0xBF, "slash": 0xBF, "`": 0xC0,
	"[": 0xDB, "\\": 0xDC, "]": 0xDD, "'": 0xDE,
}

// parseKeys turns "ctrl+shift+esc" into Windows virtual-key codes (case-insensitive; f1 to f20).
func parseKeys(keys string) ([]int, error) {
	var codes []int
	for _, k := range strings.Split(strings.ToLower(strings.TrimSpace(keys)), "+") {
		k = strings.TrimSpace(k)
		code, ok := keyNames[k]
		switch {
		case ok:
		case len(k) == 1 && (k[0] >= 'a' && k[0] <= 'z' || k[0] >= '0' && k[0] <= '9'):
			code = int(strings.ToUpper(k)[0])
		case len(k) >= 2 && k[0] == 'f':
			if n, err := strconv.Atoi(k[1:]); err == nil && n >= 1 && n <= 20 {
				code = 0x6F + n
			}
		}
		if code == 0 {
			return nil, fmt.Errorf("unknown key %q in %q", k, keys)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

package agent

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"syscall"
	"unicode/utf16"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
	"hyperhand/internal/proto"
)

// Slots follow Microsoft's UIAutomationClient.h (IUIAutomation, IUIAutomationElement, IUIAutomationTreeWalker and
// the pattern interfaces). Properties beyond the basic ones are read through GetCurrentPropertyValue.
const (
	uiaElementFromHandle   = 6
	uiaGetFocusedElement   = 8
	uiaControlViewWalker   = 14
	uiaGetParent           = 3
	uiaFirstChild          = 4
	uiaNextSibling         = 6
	uiaGetRuntimeID        = 4
	uiaGetPropertyValue    = 10
	uiaGetPropertyValueEx  = 11
	uiaGetCurrentPattern   = 16
	uiaProcessID           = 20
	uiaControlType         = 21
	uiaName                = 23
	uiaEnabled             = 28
	uiaAutomationID        = 29
	uiaClassName           = 30
	uiaPassword            = 35
	uiaOffscreen           = 38
	uiaBounds              = 43
	uiaValueSetValue       = 3 // IUIAutomationValuePattern
	uiaInvokeInvoke        = 3 // IUIAutomationInvokePattern
	uiaToggleToggle        = 3 // IUIAutomationTogglePattern
	uiaExpand              = 3 // IUIAutomationExpandCollapsePattern
	uiaCollapse            = 4
	uiaSelectionItemSelect = 3 // IUIAutomationSelectionItemPattern
	uiaScrollIntoView      = 3 // IUIAutomationScrollItemPattern
	uiaScroll              = 3 // IUIAutomationScrollPattern
	uiaTextGetSelection    = 5 // IUIAutomationTextPattern
	uiaTextRangeGetText    = 12
)

// UIA property IDs (UIAutomationClient.h) and pattern IDs.
const (
	propHasKeyboardFocus        = 30008
	propNativeWindowHandle      = 30020
	propIsOffscreen             = 30022
	propValueValue              = 30045
	propValueReadOnly           = 30046
	propHorizontalScrollPercent = 30053
	propVerticalScrollPercent   = 30055
	propHorizontallyScrollable  = 30057
	propVerticallyScrollable    = 30058
	propExpandState             = 30070
	propSelectionSelected       = 30079
	propToggleState             = 30086
	patternText                 = 10014
	uiaTextLimit                = 512
)

var (
	oleaut32          = windows.NewLazySystemDLL("oleaut32.dll")
	pSafeArrayDestroy = oleaut32.NewProc("SafeArrayDestroy")
	pGetDesktopWindow = user32.NewProc("GetDesktopWindow")
	pGetShellWindow   = user32.NewProc("GetShellWindow")
)

//go:uintptrescapes
func uiaCall(obj *ole.IUnknown, slot int, args ...uintptr) error {
	method := *(*uintptr)(unsafe.Add(unsafe.Pointer(obj.RawVTable), uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	params := append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)
	hr, _, _ := syscall.SyscallN(method, params...)
	runtime.KeepAlive(obj)
	if int32(hr) < 0 {
		return fmt.Errorf("UI Automation method %d: HRESULT 0x%08x", slot, uint32(hr))
	}
	return nil
}

func validateControlsWindow(a proto.ControlsArgs) error {
	if uint64(uintptr(a.Handle)) != a.Handle {
		return errors.New("window handle exceeds native range")
	}
	h := windows.HWND(a.Handle)
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(h, &pid); err != nil {
		return fmt.Errorf("controls window: %w", err)
	}
	desktop, _, _ := pGetDesktopWindow.Call()
	shell, _, _ := pGetShellWindow.Call()
	root, _, _ := pGetAncestor.Call(uintptr(h), gaRoot)
	if pid != a.PID || root != uintptr(h) || uintptr(h) == desktop || uintptr(h) == shell || !windows.IsWindowVisible(h) || cloaked(h) {
		return errors.New("controls require the specified visible top-level window and matching PID; desktop roots are not allowed")
	}
	return nil
}

// uiaSession holds the UIA objects of one helper run. The helper owns no UI, and every COM call, including final
// Release and CoUninitialize, runs in the same MTA apartment on the thread that opened the session, which the caller
// has locked.
type uiaSession struct {
	automation, root, walker *ole.IUnknown
}

func openUIA() (*uiaSession, error) {
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		var oe *ole.OleError
		if !errors.As(err, &oe) || oe.Code() != 1 {
			return nil, err
		}
	}
	automation, err := ole.CreateInstance(ole.NewGUID("{FF48DBA4-60EF-4201-AA87-54103EEF594E}"), ole.NewGUID("{30CBE57D-D9D0-452A-AB13-7AC5AC4825EE}"))
	if err != nil {
		ole.CoUninitialize()
		return nil, err
	}
	s := &uiaSession{automation: automation}
	if err := uiaCall(automation, uiaControlViewWalker, uintptr(unsafe.Pointer(&s.walker))); err != nil {
		s.close()
		return nil, err
	}
	if s.walker == nil {
		s.close()
		return nil, errors.New("UI Automation returned no control-view walker")
	}
	return s, nil
}

func (s *uiaSession) close() {
	for _, o := range []*ole.IUnknown{s.walker, s.root, s.automation} {
		if o != nil {
			o.Release()
		}
	}
	ole.CoUninitialize()
}

// openWindow sets root to the element of the validated window and checks that UIA attributes it to the same process.
func (s *uiaSession) openWindow(a proto.ControlsArgs) (*nativeControl, error) {
	if err := validateControlsWindow(a); err != nil {
		return nil, err
	}
	if err := uiaCall(s.automation, uiaElementFromHandle, uintptr(a.Handle), uintptr(unsafe.Pointer(&s.root))); err != nil {
		return nil, err
	}
	if s.root == nil {
		return nil, errors.New("UI Automation returned no window element")
	}
	e := &nativeControl{element: s.root, walker: s.walker}
	pid, err := e.integer(uiaProcessID)
	if err != nil {
		return nil, err
	}
	if uint32(pid) != a.PID {
		return nil, errors.New("UI Automation root PID does not match the requested window")
	}
	return e, nil
}

// focused returns the element with keyboard focus, or nil; the caller releases it.
func (s *uiaSession) focused() (*nativeControl, error) {
	var el *ole.IUnknown
	if err := uiaCall(s.automation, uiaGetFocusedElement, uintptr(unsafe.Pointer(&el))); err != nil {
		return nil, err
	}
	if el == nil {
		return nil, nil
	}
	return &nativeControl{element: el, walker: s.walker}, nil
}

// UIA reports physical screen coordinates.
func collectControls(a proto.ControlsArgs) (proto.ControlsResult, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := validateControlsWindow(a); err != nil {
		return proto.ControlsResult{}, err
	}
	s, err := openUIA()
	if err != nil {
		return proto.ControlsResult{}, err
	}
	defer s.close()
	e, err := s.openWindow(a)
	if err != nil {
		return proto.ControlsResult{}, err
	}
	if a.RootRuntimeID != "" {
		if hinted, ok := s.findHintedRuntimeID(e, a, a.RootRuntimeID, a.HintRect); ok {
			e = hinted
		} else {
			e, err = e.findScopedRuntimeID(a.RootRuntimeID)
		}
		if err != nil {
			return proto.ControlsResult{}, err
		}
		defer e.release()
	}
	r, err := walkControls(e, a)
	if err != nil {
		return proto.ControlsResult{}, err
	}
	if a.RootRuntimeID == "" {
		r.SelectedText = selectedText(s, e, a.PID)
	} else if password, err := e.integer(uiaPassword); err == nil && password == 0 {
		r.SelectedText, _ = e.selection()
	}
	if err := validateControlsWindow(a); err != nil {
		return proto.ControlsResult{}, err
	}
	return r, nil
}

// selectedText is best effort: the first selected TextPattern range of the window, else of the focused element when
// it belongs to the same process. Any failure yields "".
func selectedText(s *uiaSession, root *nativeControl, pid uint32) string {
	if text, ok := root.selection(); ok {
		return text
	}
	f, err := s.focused()
	if err != nil || f == nil {
		return ""
	}
	defer f.release()
	if p, err := f.integer(uiaProcessID); err != nil || uint32(p) != pid {
		return ""
	}
	text, _ := f.selection()
	return text
}

// collectFocused summarizes the UIA focused element for list_windows; nil when nothing has focus.
func collectFocused() (*proto.FocusedControl, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	s, err := openUIA()
	if err != nil {
		return nil, err
	}
	defer s.close()
	e, err := s.focused()
	if err != nil || e == nil {
		return nil, err
	}
	defer e.release()
	n, password, _, err := e.info()
	if err != nil {
		return nil, err
	}
	f := &proto.FocusedControl{
		Name:         n.Name,
		ControlType:  proto.ControlTypeName(n.ControlType),
		AutomationID: n.AutomationID,
		ClassName:    n.ClassName,
		RuntimeID:    n.RuntimeID,
		Rect:         n.Rect,
	}
	if password {
		f.Name = ""
	}
	f.Window = uint64(e.topWindow())
	return f, nil
}

// topWindow is the top-level window containing the element: GetAncestor(GA_ROOT) of the nearest native window handle
// up the control view, else of the window under the element's center.
func (e *nativeControl) topWindow() windows.HWND {
	var hwnd uintptr
	cur := e
	for i := 0; i < 32 && cur != nil; i++ {
		if v, err := cur.property(propNativeWindowHandle); err == nil {
			if v.VT == ole.VT_I4 {
				hwnd = uintptr(uint32(v.Val))
			}
			v.Clear()
		}
		if hwnd != 0 {
			break
		}
		parent, err := cur.relative(uiaGetParent)
		if cur != e {
			cur.release()
		}
		if err != nil || parent == nil {
			cur = nil
			break
		}
		cur = parent.(*nativeControl)
	}
	if cur != nil && cur != e {
		cur.release()
	}
	if hwnd == 0 {
		var r proto.Rect
		if err := uiaCall(e.element, uiaBounds, uintptr(unsafe.Pointer(&r))); err != nil {
			return 0
		}
		x, y := (r.Left+r.Right)/2, (r.Top+r.Bottom)/2
		hwnd, _, _ = pWindowFromPoint.Call(uintptr(uint32(x)) | uintptr(uint32(y))<<32)
		if hwnd == 0 {
			return 0
		}
	}
	root, _, _ := pGetAncestor.Call(hwnd, gaRoot)
	return windows.HWND(root)
}

// performControlAction runs one pattern action on the element with the given runtime ID.
func performControlAction(a proto.ControlActionArgs) (proto.ControlActionResult, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	action, err := parseAction(a.Action)
	if err != nil {
		return proto.ControlActionResult{}, err
	}
	ca := proto.ControlsArgs{Handle: a.Handle, PID: a.PID}
	if err := validateControlsWindow(ca); err != nil {
		return proto.ControlActionResult{}, err
	}
	s, err := openUIA()
	if err != nil {
		return proto.ControlActionResult{}, err
	}
	defer s.close()
	root, err := s.openWindow(ca)
	if err != nil {
		return proto.ControlActionResult{}, err
	}
	e, hinted := s.findHintedRuntimeID(root, ca, a.RuntimeID, a.HintRect)
	if !hinted {
		e, err = root.findRuntimeID(a.RuntimeID)
	}
	if err != nil {
		return proto.ControlActionResult{}, err
	}
	if e == nil {
		return proto.ControlActionResult{}, errElementNotFound
	}
	defer e.release()
	available, err := e.patterns()
	if err != nil {
		return proto.ControlActionResult{}, err
	}
	if action == "Locate" {
		return readControlActionResult(e, available, action, a.Value), nil
	}
	var before *proto.ControlState
	switch action {
	case "Toggle", "ScrollUp", "ScrollDown", "ScrollLeft", "ScrollRight":
		before = e.state(available)
	}
	idx := patternFor(action)
	if !available[idx] || !scrollAxisSupported(action, before) {
		if before == nil {
			before = e.state(available)
		}
		return proto.ControlActionResult{}, unsupportedPattern(action, actionNamesForState(available, before))
	}
	var pattern *ole.IUnknown
	if err := uiaCall(e.element, uiaGetCurrentPattern, uintptr(uiaPatterns[idx].id), uintptr(unsafe.Pointer(&pattern))); err != nil {
		return proto.ControlActionResult{}, err
	}
	if pattern == nil {
		if before == nil {
			before = e.state(available)
		}
		return proto.ControlActionResult{}, unsupportedPattern(action, actionNamesForState(available, before))
	}
	defer pattern.Release()
	switch action {
	case "SetValue":
		value := ole.SysAllocString(a.Value)
		err = uiaCall(pattern, uiaValueSetValue, uintptr(unsafe.Pointer(value)))
		ole.SysFreeString(value)
	case "Invoke":
		err = uiaCall(pattern, uiaInvokeInvoke)
	case "Toggle":
		err = uiaCall(pattern, uiaToggleToggle)
	case "Expand":
		err = uiaCall(pattern, uiaExpand)
	case "Collapse":
		err = uiaCall(pattern, uiaCollapse)
	case "Select":
		err = uiaCall(pattern, uiaSelectionItemSelect)
	case "ScrollIntoView":
		err = uiaCall(pattern, uiaScrollIntoView)
	case "ScrollUp":
		err = uiaCall(pattern, uiaScroll, 2, 1) // NoAmount, SmallDecrement
	case "ScrollDown":
		err = uiaCall(pattern, uiaScroll, 2, 4) // NoAmount, SmallIncrement
	case "ScrollLeft":
		err = uiaCall(pattern, uiaScroll, 1, 2)
	case "ScrollRight":
		err = uiaCall(pattern, uiaScroll, 4, 2)
	}
	if err != nil {
		return proto.ControlActionResult{}, fmt.Errorf("%s failed: %w", action, err)
	}
	return readVerifiedControlAction(action, before, func() proto.ControlActionResult {
		return readControlActionResult(e, available, action, a.Value)
	}), nil
}

// readControlActionResult reads the element's state after an action (or for Locate): its current rectangle and, when it
// has a ValuePattern and is not a password control, its value; SetValue compares the value with the requested one.
func readControlActionResult(e *nativeControl, available []bool, action, requested string) proto.ControlActionResult {
	r := proto.ControlActionResult{State: e.state(available)}
	var rect proto.Rect
	if err := uiaCall(e.element, uiaBounds, uintptr(unsafe.Pointer(&rect))); err == nil {
		r.Rect = &rect
	}
	if !available[valuePatternIndex] {
		return r
	}
	password, err := e.integer(uiaPassword)
	if err != nil || password != 0 {
		return r
	}
	v, err := e.stateProperty(propValueValue)
	defer v.Clear()
	if err != nil || v.VT != ole.VT_BSTR {
		return r
	}
	if value, cut, err := bstrText(*(**int16)(unsafe.Pointer(&v.Val))); err == nil {
		r.Value, r.HasValue = value, true
		if action == "SetValue" && !cut { // a truncated read-back cannot be compared
			verified := value == requested
			r.Verified = &verified
		}
	}
	return r
}

// valuePatternIndex is the uiaPatterns entry of ValuePattern.
var valuePatternIndex = patternFor("SetValue")

type nativeControl struct {
	element, walker *ole.IUnknown
}

func (e *nativeControl) release() { e.element.Release() }
func (e *nativeControl) relative(slot int) (controlElement, error) {
	var out *ole.IUnknown
	if err := uiaCall(e.walker, slot, uintptr(unsafe.Pointer(e.element)), uintptr(unsafe.Pointer(&out))); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	return &nativeControl{element: out, walker: e.walker}, nil
}
func (e *nativeControl) first() (controlElement, error) { return e.relative(uiaFirstChild) }
func (e *nativeControl) next() (controlElement, error)  { return e.relative(uiaNextSibling) }
func (e *nativeControl) integer(slot int) (int32, error) {
	var out int32
	err := uiaCall(e.element, slot, uintptr(unsafe.Pointer(&out)))
	return out, err
}
func (e *nativeControl) text(slot int) (string, bool, error) {
	var out *int16
	err := uiaCall(e.element, slot, uintptr(unsafe.Pointer(&out)))
	if out != nil {
		defer ole.SysFreeString(out)
	}
	if err != nil {
		return "", false, err
	}
	return bstrText(out)
}

// bstrText decodes a BSTR, capped at uiaTextLimit UTF-16 units; the flag says it was cut.
func bstrText(s *int16) (string, bool, error) {
	if s == nil {
		return "", false, nil
	}
	n := ole.SysStringLen(s)
	cut := n > uiaTextLimit
	if cut {
		n = uiaTextLimit
	}
	return string(utf16.Decode(unsafe.Slice((*uint16)(unsafe.Pointer(s)), int(n)))), cut, nil
}

// property reads a UIA property through GetCurrentPropertyValue; the caller clears the VARIANT.
func (e *nativeControl) property(id int32) (ole.VARIANT, error) {
	var v ole.VARIANT
	err := uiaCall(e.element, uiaGetPropertyValue, uintptr(id), uintptr(unsafe.Pointer(&v)))
	return v, err
}

// stateProperty suppresses UIA default values for properties the provider does
// not implement; those are returned as a reserved VT_UNKNOWN value instead.
func (e *nativeControl) stateProperty(id int32) (ole.VARIANT, error) {
	var v ole.VARIANT
	err := uiaCall(e.element, uiaGetPropertyValueEx, uintptr(id), 1, uintptr(unsafe.Pointer(&v)))
	return v, err
}

// propertyBool reads a VT_BOOL property; an unsupported property reads false.
func (e *nativeControl) propertyBool(id int32) (bool, error) {
	v, err := e.property(id)
	if err != nil {
		return false, err
	}
	defer v.Clear()
	return v.VT == ole.VT_BOOL && int16(v.Val) != 0, nil
}

// state reads only properties of supported patterns. A failed or unsupported read
// remains nil so callers do not mistake an unknown state for false or zero.
func (e *nativeControl) state(available []bool) *proto.ControlState {
	s := &proto.ControlState{}
	for _, p := range []struct {
		action string
		id     int32
		target **bool
	}{
		{"", propIsOffscreen, &s.Offscreen},
		{"SetValue", propValueReadOnly, &s.ReadOnly},
		{"Select", propSelectionSelected, &s.Selected},
		{"ScrollUp", propHorizontallyScrollable, &s.HorizontallyScrollable},
		{"ScrollUp", propVerticallyScrollable, &s.VerticallyScrollable},
	} {
		if p.action != "" && !available[patternFor(p.action)] {
			continue
		}
		v, err := e.stateProperty(p.id)
		if err == nil && v.VT == ole.VT_BOOL {
			b := int16(v.Val) != 0
			*p.target = &b
		}
		v.Clear()
	}
	for _, p := range []struct {
		action string
		id     int32
		names  []string
		target **string
	}{
		{"Toggle", propToggleState, []string{"off", "on", "indeterminate"}, &s.Toggle},
		{"Expand", propExpandState, []string{"collapsed", "expanded", "partially_expanded", "leaf"}, &s.ExpandCollapse},
	} {
		if !available[patternFor(p.action)] {
			continue
		}
		v, err := e.stateProperty(p.id)
		if err == nil && v.VT == ole.VT_I4 && v.Val >= 0 && v.Val < int64(len(p.names)) {
			name := p.names[v.Val]
			*p.target = &name
		}
		v.Clear()
	}
	if available[patternFor("ScrollUp")] {
		for _, p := range []struct {
			id         int32
			target     **float64
			scrollable *bool
		}{
			{propHorizontalScrollPercent, &s.HorizontalScrollPercent, s.HorizontallyScrollable},
			{propVerticalScrollPercent, &s.VerticalScrollPercent, s.VerticallyScrollable},
		} {
			v, err := e.stateProperty(p.id)
			if err == nil && v.VT == ole.VT_R8 {
				*p.target = readableScrollPercent(math.Float64frombits(uint64(v.Val)), p.scrollable)
			}
			v.Clear()
		}
	}
	if s.Toggle == nil && s.ExpandCollapse == nil && s.Selected == nil && s.ReadOnly == nil && s.Offscreen == nil && s.HorizontallyScrollable == nil && s.VerticallyScrollable == nil && s.HorizontalScrollPercent == nil && s.VerticalScrollPercent == nil {
		return nil
	}
	return s
}

// propertyText reads a VT_BSTR property with the text cap; an unsupported property reads "".
func (e *nativeControl) propertyText(id int32) (string, bool, error) {
	v, err := e.property(id)
	if err != nil {
		return "", false, err
	}
	defer v.Clear()
	if v.VT != ole.VT_BSTR {
		return "", false, nil
	}
	return bstrText(*(**int16)(unsafe.Pointer(&v.Val)))
}

// safeArray is the 64-bit SAFEARRAY layout (one dimension read).
type safeArray struct {
	dims     uint16
	features uint16
	elemSize uint32
	locks    uint32
	_        uint32
	data     unsafe.Pointer
	bounds   [1]struct {
		count uint32
		lower int32
	}
}

func destroySafeArray(a *safeArray) { pSafeArrayDestroy.Call(uintptr(unsafe.Pointer(a))) }

// runtimeID reads GetRuntimeId (a SAFEARRAY of int32).
func (e *nativeControl) runtimeID() ([]int32, error) {
	var arr *safeArray
	if err := uiaCall(e.element, uiaGetRuntimeID, uintptr(unsafe.Pointer(&arr))); err != nil {
		return nil, err
	}
	if arr == nil {
		return nil, nil
	}
	defer destroySafeArray(arr)
	if arr.dims != 1 || arr.elemSize != 4 || arr.data == nil {
		return nil, nil
	}
	ids := make([]int32, arr.bounds[0].count)
	copy(ids, unsafe.Slice((*int32)(arr.data), len(ids)))
	return ids, nil
}

// patterns reports which of uiaPatterns the element supports.
func (e *nativeControl) patterns() ([]bool, error) {
	available := make([]bool, len(uiaPatterns))
	for i, p := range uiaPatterns {
		on, err := e.propertyBool(p.available)
		if err != nil {
			return nil, err
		}
		available[i] = on
	}
	return available, nil
}

// selection returns the text of the element's first selected TextPattern range; ok is false when it has none.
func (e *nativeControl) selection() (string, bool) {
	var pattern *ole.IUnknown
	if err := uiaCall(e.element, uiaGetCurrentPattern, uintptr(patternText), uintptr(unsafe.Pointer(&pattern))); err != nil || pattern == nil {
		return "", false
	}
	defer pattern.Release()
	var arr *safeArray
	if err := uiaCall(pattern, uiaTextGetSelection, uintptr(unsafe.Pointer(&arr))); err != nil || arr == nil {
		return "", false
	}
	defer destroySafeArray(arr) // releases the ranges it holds
	if arr.dims != 1 || arr.bounds[0].count == 0 || arr.data == nil {
		return "", false
	}
	rng := *(**ole.IUnknown)(arr.data)
	if rng == nil {
		return "", false
	}
	var out *int16
	if err := uiaCall(rng, uiaTextRangeGetText, uintptr(uiaTextLimit), uintptr(unsafe.Pointer(&out))); err != nil {
		return "", false
	}
	if out != nil {
		defer ole.SysFreeString(out)
	}
	text, _, _ := bstrText(out)
	return text, true
}

// findRuntimeID walks the control view under e for the element whose runtime ID formats as want; nil when none. The
// caller releases the result, which is e itself (with an extra reference) when it matches.
func (e *nativeControl) findRuntimeID(want string) (*nativeControl, error) {
	id, err := e.runtimeID()
	if err != nil {
		return nil, err
	}
	if formatRuntimeID(id) == want {
		e.element.AddRef()
		return e, nil
	}
	child, err := e.first()
	if err != nil {
		return nil, err
	}
	for child != nil {
		c := child.(*nativeControl)
		found, err := c.findRuntimeID(want)
		if err != nil {
			c.release()
			return nil, err
		}
		if found != nil {
			c.release()
			return found, nil
		}
		next, err := c.next()
		c.release()
		if err != nil {
			return nil, err
		}
		child = next
	}
	return nil, nil
}

func (e *nativeControl) info() (proto.ControlInfo, bool, bool, error) {
	var n proto.ControlInfo
	password, err := e.integer(uiaPassword)
	if err != nil {
		return n, false, false, err
	}
	pid, err := e.integer(uiaProcessID)
	if err != nil {
		return n, false, false, err
	}
	n.PID = uint32(pid)
	n.ControlType, err = e.integer(uiaControlType)
	if err != nil {
		return n, false, false, err
	}
	on, err := e.integer(uiaEnabled)
	if err != nil {
		return n, false, false, err
	}
	n.Enabled = on != 0
	off, err := e.integer(uiaOffscreen)
	if err != nil {
		return n, false, false, err
	}
	n.Offscreen = off != 0
	if err := uiaCall(e.element, uiaBounds, uintptr(unsafe.Pointer(&n.Rect))); err != nil {
		return n, false, false, err
	}
	cut := false
	fields := []struct {
		slot   int
		target *string
	}{{uiaAutomationID, &n.AutomationID}, {uiaClassName, &n.ClassName}}
	if password == 0 {
		fields = append(fields, struct {
			slot   int
			target *string
		}{uiaName, &n.Name})
	}
	for _, f := range fields {
		value, shortened, err := e.text(f.slot)
		if err != nil {
			return n, false, false, err
		}
		*f.target, cut = value, cut || shortened
	}
	id, err := e.runtimeID()
	if err != nil {
		return n, false, false, err
	}
	n.RuntimeID = formatRuntimeID(id)
	available, err := e.patterns()
	if err != nil {
		return n, false, false, err
	}
	n.Patterns = patternNames(available)
	n.State = e.state(available)
	n.Actions = actionNamesForState(available, n.State)
	if available[valuePatternIndex] && password == 0 {
		value, shortened, err := e.propertyText(propValueValue)
		if err != nil {
			return n, false, false, err
		}
		n.Value, n.HasValue, cut = value, true, cut || shortened
	}
	n.Focused, err = e.propertyBool(propHasKeyboardFocus)
	if err != nil {
		return n, false, false, err
	}
	return n, password != 0, cut, nil
}

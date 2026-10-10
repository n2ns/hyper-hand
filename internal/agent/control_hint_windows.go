package agent

import (
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"hyperhand/internal/proto"
)

const uiaElementFromPoint = 7
const uiaNormalizeElement = 8

// A rectangle is only a lookup hint. A hit is accepted only after exact runtime
// identity and its control-view ancestry to this request's window are verified.
// Every failure falls back to the ordinary traversal; it never establishes absence.
func (s *uiaSession) findHintedRuntimeID(root *nativeControl, window proto.ControlsArgs, want string, rect *proto.Rect) (*nativeControl, bool) {
	point, ok := hintPoint(rect)
	if !ok || want == "" {
		return nil, false
	}
	// Reject covered or offscreen hints before asking UIA. No mouse input occurs.
	hwnd, _, _ := pWindowFromPoint.Call(point)
	if hwnd == 0 {
		return nil, false
	}
	top, _, _ := pGetAncestor.Call(hwnd, gaRoot)
	if top != uintptr(window.Handle) {
		return nil, false
	}
	rootIDs, err := root.runtimeID()
	if err != nil || len(rootIDs) == 0 {
		return nil, false
	}
	rootID := formatRuntimeID(rootIDs)
	var hit *ole.IUnknown
	if err := uiaCall(s.automation, uiaElementFromPoint, point, uintptr(unsafe.Pointer(&hit))); err != nil || hit == nil {
		if hit != nil {
			hit.Release()
		}
		return nil, false
	}
	var normalized *ole.IUnknown
	err = uiaCall(s.walker, uiaNormalizeElement, uintptr(unsafe.Pointer(hit)), uintptr(unsafe.Pointer(&normalized)))
	hit.Release()
	if err != nil || normalized == nil {
		if normalized != nil {
			normalized.Release()
		}
		return nil, false
	}
	cur := &nativeControl{element: normalized, walker: s.walker}
	var target *nativeControl
	defer func() {
		if cur != nil {
			cur.release()
		}
		if target != nil {
			target.release()
		}
	}()
	for depth := 0; depth <= 64; depth++ {
		ids, err := cur.runtimeID()
		if err != nil || len(ids) == 0 {
			return nil, false
		}
		id := formatRuntimeID(ids)
		if target == nil && id == want {
			cur.element.AddRef()
			target = &nativeControl{element: cur.element, walker: s.walker}
		} else if target != nil {
			password, err := cur.integer(uiaPassword)
			if err != nil || password != 0 {
				return nil, false
			}
		}
		if id == rootID {
			if target == nil || validateControlsWindow(window) != nil {
				return nil, false
			}
			result := target
			target = nil // transfer the retained reference to the caller
			return result, true
		}
		if depth == 64 {
			return nil, false
		}
		parent, err := cur.relative(uiaGetParent)
		if err != nil || parent == nil {
			return nil, false
		}
		cur.release()
		cur = parent.(*nativeControl)
	}
	return nil, false
}

func hintPoint(rect *proto.Rect) (uintptr, bool) {
	if rect == nil || rect.Right <= rect.Left || rect.Bottom <= rect.Top {
		return 0, false
	}
	// Widen before adding so even an extreme stale rectangle cannot overflow.
	x := int32((int64(rect.Left) + int64(rect.Right)) / 2)
	y := int32((int64(rect.Top) + int64(rect.Bottom)) / 2)
	return uintptr(uint32(x)) | uintptr(uint32(y))<<32, true
}

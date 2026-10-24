package agent

import (
	"errors"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"hyperhand/internal/proto"
)

const (
	uiaCreateCacheRequest  = 20 // IUIAutomation
	uiaCacheAddProperty    = 3  // IUIAutomationCacheRequest
	uiaCachePutTreeScope   = 7
	uiaBuildUpdatedCache   = 9  // IUIAutomationElement
	uiaGetCachedProperty   = 12 // GetCachedPropertyValue
	uiaGetCachedPropertyEx = 13
	uiaCachedOffset        = 32 // get_CachedX = get_CurrentX + 32
	uiaWalkerCacheOffset   = 6  // GetXElementBuildCache follows the six ordinary walker methods
	treeScopeElement       = 1
	propRuntimeID          = 30000
	vtArrayI4              = ole.VT_ARRAY | ole.VT_I4
)

var uiaCacheProperties = []int32{
	propRuntimeID, 30001, 30002, 30003, propHasKeyboardFocus, 30010, 30011, 30012, 30019, propIsOffscreen,
	propValueReadOnly, propHorizontalScrollPercent, propVerticalScrollPercent, propHorizontallyScrollable,
	propVerticallyScrollable, propExpandState, propSelectionSelected, propToggleState,
}

// Names and values are deliberately absent: password controls must not have either read,
// even into an internal cache. Names and values are read live after the password check.
func searchCacheProperties(a proto.FindControlsArgs) []int32 {
	ids := []int32{30019} // IsPassword is needed even when no selectors match: it prunes descendants.
	if a.ControlType != 0 {
		ids = append(ids, 30003)
	}
	if a.AutomationID != "" {
		ids = append(ids, 30011)
	}
	return ids
}

// cacheElement caches just e, then reuses the request when navigating to each next
// element. The existing bounded walk still decides which children to visit; unlike a
// subtree cache, this never traverses password descendants or fetches a whole tree
// before applying the limits. Boundary probes fetch only one element's metadata.
// The session owns the cache request, and the caller releases the returned element.
func (s *uiaSession) cacheElement(e *nativeControl, search *proto.FindControlsArgs) (*nativeControl, error) {
	var req *ole.IUnknown
	if err := uiaCall(s.automation, uiaCreateCacheRequest, uintptr(unsafe.Pointer(&req))); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("UI Automation returned no cache request")
	}
	s.cache = req
	ids := append([]int32{}, uiaCacheProperties...)
	for _, p := range uiaPatterns {
		ids = append(ids, p.available)
	}
	if search != nil {
		ids = searchCacheProperties(*search)
	}
	for _, id := range ids {
		if err := uiaCall(req, uiaCacheAddProperty, uintptr(id)); err != nil {
			return nil, err
		}
	}
	if err := uiaCall(req, uiaCachePutTreeScope, treeScopeElement); err != nil {
		return nil, err
	}
	var out *ole.IUnknown
	if err := uiaCall(e.element, uiaBuildUpdatedCache, uintptr(unsafe.Pointer(req)), uintptr(unsafe.Pointer(&out))); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("UI Automation returned no cached element")
	}
	return &nativeControl{element: out, walker: e.walker, cache: req, partial: search != nil}, nil
}

// cachedRuntimeID reads the cached RuntimeId property (VT_ARRAY|VT_I4).
func (e *nativeControl) cachedRuntimeID() ([]int32, error) {
	v, err := e.property(propRuntimeID)
	if err != nil {
		return nil, err
	}
	defer v.Clear()
	if v.VT != vtArrayI4 || v.Val == 0 {
		return nil, nil
	}
	arr := *(**safeArray)(unsafe.Pointer(&v.Val))
	if arr.dims != 1 || arr.elemSize != 4 || arr.data == nil {
		return nil, nil
	}
	ids := make([]int32, arr.bounds[0].count)
	copy(ids, unsafe.Slice((*int32)(arr.data), len(ids)))
	return ids, nil
}

const (
	uiaControlViewCondition = 18 // IUIAutomation get_ControlViewCondition
	uiaFindAllBuildCache    = 8  // IUIAutomationElement
	uiaArrayLength          = 3  // IUIAutomationElementArray
	uiaArrayGetElement      = 4
	treeScopeChildren       = 2
	propBoundingRectangle   = 30001
)

// childScan lists an element's children for the per-parent child limit: one FindAllBuildCache over the immediate
// children that satisfy the control-view condition, caching only the rectangle and the offscreen flag. full is the
// walk's cache request (nil for the uncached walk), applied to the children that are then visited. The session owns
// cond and light.
type childScan struct {
	cond, light, full *ole.IUnknown
}

func (s *uiaSession) openChildScan(full *ole.IUnknown) (*childScan, error) {
	c := &childScan{full: full}
	s.scan = c
	if err := uiaCall(s.automation, uiaControlViewCondition, uintptr(unsafe.Pointer(&c.cond))); err != nil {
		return nil, err
	}
	if c.cond == nil {
		return nil, errors.New("UI Automation returned no control-view condition")
	}
	if err := uiaCall(s.automation, uiaCreateCacheRequest, uintptr(unsafe.Pointer(&c.light))); err != nil {
		return nil, err
	}
	if c.light == nil {
		return nil, errors.New("UI Automation returned no cache request")
	}
	for _, id := range []int32{propBoundingRectangle, propIsOffscreen} {
		if err := uiaCall(c.light, uiaCacheAddProperty, uintptr(id)); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// children lists e's children in one provider call. When the provider fails the call it returns nil, and the walk
// visits the children through the control-view walker as before, without the child limit.
func (e *nativeControl) children() (childSet, error) {
	if e.scan == nil {
		return nil, nil
	}
	var arr *ole.IUnknown
	if err := uiaCall(e.element, uiaFindAllBuildCache, treeScopeChildren, uintptr(unsafe.Pointer(e.scan.cond)), uintptr(unsafe.Pointer(e.scan.light)), uintptr(unsafe.Pointer(&arr))); err != nil || arr == nil {
		return nil, nil
	}
	var n int32
	if err := uiaCall(arr, uiaArrayLength, uintptr(unsafe.Pointer(&n))); err != nil {
		arr.Release()
		return nil, nil
	}
	return &nativeChildren{array: arr, n: int(n), parent: e}, nil
}

type nativeChildren struct {
	array  *ole.IUnknown
	n      int
	parent *nativeControl
}

func (c *nativeChildren) len() int { return c.n }
func (c *nativeChildren) release() { c.array.Release() }

func (c *nativeChildren) element(i int) (*ole.IUnknown, error) {
	var el *ole.IUnknown
	if err := uiaCall(c.array, uiaArrayGetElement, uintptr(i), uintptr(unsafe.Pointer(&el))); err != nil {
		return nil, err
	}
	if el == nil {
		return nil, errors.New("UI Automation returned no child element")
	}
	return el, nil
}

// bounds reads the cached rectangle and offscreen flag of child i.
func (c *nativeChildren) bounds(i int) (proto.Rect, bool, error) {
	var r proto.Rect
	el, err := c.element(i)
	if err != nil {
		return r, false, err
	}
	defer el.Release()
	e := nativeControl{element: el, cache: c.parent.scan.light}
	if err := uiaCall(el, e.slot(uiaBounds), uintptr(unsafe.Pointer(&r))); err != nil {
		return r, false, err
	}
	off, err := e.integer(uiaOffscreen)
	return r, off != 0, err
}

// load returns child i with the walk's cache request applied, like the walker's navigation does.
func (c *nativeChildren) load(i int) (controlElement, error) {
	el, err := c.element(i)
	if err != nil {
		return nil, err
	}
	p := c.parent
	if p.scan.full == nil {
		return &nativeControl{element: el, walker: p.walker, scan: p.scan}, nil
	}
	var out *ole.IUnknown
	err = uiaCall(el, uiaBuildUpdatedCache, uintptr(unsafe.Pointer(p.scan.full)), uintptr(unsafe.Pointer(&out)))
	el.Release()
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("UI Automation returned no cached element")
	}
	return &nativeControl{element: out, walker: p.walker, cache: p.scan.full, scan: p.scan}, nil
}

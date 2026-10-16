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

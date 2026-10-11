package agent

import (
	"errors"
	"unsafe"

	ole "github.com/go-ole/go-ole"
)

// PROTOTYPE: read a whole control-view subtree in one cross-process BuildUpdatedCache and then read every node from
// the cache in process. uiaCacheTree switches collectControls and collectFindControls to it.
var uiaCacheTree bool

const (
	uiaCreateCacheRequest  = 20 // IUIAutomation
	uiaCacheAddProperty    = 3  // IUIAutomationCacheRequest
	uiaCachePutTreeScope   = 7
	uiaBuildUpdatedCache   = 9  // IUIAutomationElement
	uiaGetCachedProperty   = 12 // GetCachedPropertyValue
	uiaGetCachedPropertyEx = 13
	uiaGetCachedChildren   = 19
	uiaCachedOffset        = 32 // get_CachedX = get_CurrentX + 32
	uiaArrayLength         = 3  // IUIAutomationElementArray
	uiaArrayElement        = 4
	treeScopeSubtree       = 7
	propRuntimeID          = 30000
	vtArrayI4              = ole.VT_ARRAY | ole.VT_I4
)

var uiaCacheProperties = []int32{
	propRuntimeID, 30001, 30002, 30003, 30005, propHasKeyboardFocus, 30010, 30011, 30012, 30019, propIsOffscreen,
	propValueValue, propValueReadOnly, propHorizontalScrollPercent, propVerticalScrollPercent, propHorizontallyScrollable,
	propVerticallyScrollable, propExpandState, propSelectionSelected, propToggleState,
}

// cachedSiblings is the cached children array a cached element came from.
type cachedSiblings struct {
	array *ole.IUnknown
	index int
	count int32
}

// uiaSearchProperties are the only properties searchMatch reads; matches read info live.
var uiaSearchProperties = []int32{30003, 30005, 30011, 30019}

// cacheSubtree returns a cached copy of e holding its whole control-view subtree; the caller releases it. With
// search, only the selector properties are cached.
func (s *uiaSession) cacheSubtree(e *nativeControl, search bool) (*nativeControl, error) {
	var req *ole.IUnknown
	if err := uiaCall(s.automation, uiaCreateCacheRequest, uintptr(unsafe.Pointer(&req))); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("UI Automation returned no cache request")
	}
	defer req.Release()
	ids := append([]int32{}, uiaCacheProperties...)
	for _, p := range uiaPatterns {
		ids = append(ids, p.available)
	}
	if search {
		ids = uiaSearchProperties
	}
	for _, id := range ids {
		if err := uiaCall(req, uiaCacheAddProperty, uintptr(id)); err != nil {
			return nil, err
		}
	}
	if err := uiaCall(req, uiaCachePutTreeScope, treeScopeSubtree); err != nil {
		return nil, err
	}
	var out *ole.IUnknown
	if err := uiaCall(e.element, uiaBuildUpdatedCache, uintptr(unsafe.Pointer(req)), uintptr(unsafe.Pointer(&out))); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("UI Automation returned no cached element")
	}
	return &nativeControl{element: out, walker: e.walker, cached: true, partial: search}, nil
}

func (e *nativeControl) cachedChild(siblings cachedSiblings) (controlElement, error) {
	if siblings.index >= int(siblings.count) {
		siblings.array.Release()
		return nil, nil
	}
	var out *ole.IUnknown
	if err := uiaCall(siblings.array, uiaArrayElement, uintptr(siblings.index), uintptr(unsafe.Pointer(&out))); err != nil {
		siblings.array.Release()
		return nil, err
	}
	return &nativeControl{element: out, walker: e.walker, cached: true, partial: e.partial, siblings: &siblings}, nil
}

func (e *nativeControl) cachedFirst() (controlElement, error) {
	var arr *ole.IUnknown
	if err := uiaCall(e.element, uiaGetCachedChildren, uintptr(unsafe.Pointer(&arr))); err != nil {
		return nil, err
	}
	if arr == nil {
		return nil, nil
	}
	var n int32
	if err := uiaCall(arr, uiaArrayLength, uintptr(unsafe.Pointer(&n))); err != nil {
		arr.Release()
		return nil, err
	}
	return e.cachedChild(cachedSiblings{array: arr, count: n})
}

func (e *nativeControl) cachedNext() (controlElement, error) {
	if e.siblings == nil {
		return nil, nil
	}
	e.siblings.array.AddRef()
	return e.cachedChild(cachedSiblings{array: e.siblings.array, index: e.siblings.index + 1, count: e.siblings.count})
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

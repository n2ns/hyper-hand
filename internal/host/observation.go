package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"hyperhand/internal/proto"
)

// observeIn is vm_observe's input, also used by actions for their after-action observation (observe_after).
type observeIn struct {
	VM            string `json:"vm,omitempty" jsonschema:"VM name from vm_list (required)"`
	Handle        uint64 `json:"handle,omitempty" jsonschema:"observe this window (handle from vm_windows or a previous observation): the screenshot is cropped to it and the control tree rooted at it; omit for the whole screen"`
	PID           uint32 `json:"pid,omitempty" jsonschema:"restrict handle to this process, or select the process's only visible window"`
	ObservationID string `json:"observation_id,omitempty" jsonschema:"with index, observe this control's subtree; alternative to handle/pid; implies controls; screenshot remains cropped to its window"`
	Index         *int   `json:"index,omitempty" jsonschema:"subtree root index from vm_find_controls or vm_observe; requires observation_id"`
	Screenshot    *bool  `json:"screenshot,omitempty" jsonschema:"include a screenshot; default true"`
	Controls      bool   `json:"controls,omitempty" jsonschema:"include the UI Automation control tree as indexed text; default false"`
	MaxDepth      int    `json:"max_depth,omitempty" jsonschema:"control tree depth; default 4, maximum 10"`
	MaxNodes      int    `json:"max_nodes,omitempty" jsonschema:"control tree size; default 200, maximum 1000"`
	MaxSize       int    `json:"max_size,omitempty" jsonschema:"longest side of the output image in pixels; 0 keeps the original size, never upscales"`
	DiffFrom      string `json:"diff_from,omitempty" jsonschema:"a previous observation_id of the same window: controls then lists only added, removed and changed nodes"`
}

// observeScreenshot describes the returned image: (OriginX, OriginY) is its top-left in guest screen pixels and Scale
// is output pixels per screen pixel. Callers never need them: actions take observation_id and image pixels.
type observeScreenshot struct {
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	OriginX int     `json:"origin_x"`
	OriginY int     `json:"origin_y"`
	Scale   float64 `json:"scale"`
}

// observeWindow is the observed window in an observation result.
type observeWindow struct {
	Handle     uint64     `json:"handle"`
	PID        uint32     `json:"pid"`
	Process    string     `json:"process"`
	Title      string     `json:"title"`
	Class      string     `json:"class"`
	Rect       proto.Rect `json:"rect"`
	Foreground bool       `json:"foreground"`
	Enabled    bool       `json:"enabled"`
	GroupRoot  uint64     `json:"group_root"`
	Integrity  string     `json:"integrity,omitempty"`
}

// observeFocused is the focused control in an observation result; Index is its index in the control tree when the
// tree was captured and contains it, else -1.
type observeFocused struct {
	Index       int        `json:"index"`
	Name        string     `json:"name"`
	ControlType string     `json:"control_type"`
	ClassName   string     `json:"class_name,omitempty"`
	Rect        proto.Rect `json:"rect"`
}

// controlsDiff lists the changes between two control trees; Removed holds indexes of the earlier observation.
type controlsDiff struct {
	Added   []string `json:"added"`
	Removed []int    `json:"removed"`
	Changed []string `json:"changed"`
}

// observeOut is the JSON text item of vm_observe and of an action's "after" field. Controls is the indexed text tree
// ("[12] Edit \"\" (0,980 1920x60) focused"), nil when not requested or replaced by ControlsDiff. StaleRisk explains
// data that may be out of date (target not responding, diff_from ignored); Agent is "offline" when the agent could not
// be reached and only the screenshot is present.
type observeOut struct {
	ObservationID     string             `json:"observation_id"`
	VM                string             `json:"vm"`
	CapturedAt        string             `json:"captured_at"`
	Window            *observeWindow     `json:"window,omitempty"`
	Screenshot        *observeScreenshot `json:"screenshot,omitempty"`
	Focused           *observeFocused    `json:"focused,omitempty"`
	SelectedText      string             `json:"selected_text,omitempty"`
	Controls          *string            `json:"controls,omitempty"`
	ControlsTruncated bool               `json:"controls_truncated,omitempty"`
	ControlsDiff      *controlsDiff      `json:"controls_diff,omitempty"`
	StaleRisk         string             `json:"stale_risk,omitempty"`
	Agent             string             `json:"agent,omitempty"`
}

// observeFunc performs an observation and returns its JSON item and PNG (nil without a screenshot). It is implemented
// in tools_observe.go and used by actions for observe_after.
type observeFunc func(ctx context.Context, in observeIn) (*observeOut, []byte, error)

// observation is what the host remembers about one vm_observe result so that later actions can be checked against it
// and mapped back to screen pixels.
type observation struct {
	ID        string
	VM        string
	At        time.Time
	Revision  uint64             // guest mutations since capture invalidate coordinate-based targets
	Epoch     uint64             // VM lifecycle changes invalidate both coordinates and control identities
	SourcePNG []byte             // original screen image, before cropping/scaling, for local visual checks
	Window    *proto.WindowInfo  // the observed window; nil for a whole-screen observation
	Windows   []proto.WindowInfo // the window list at capture time
	// Output image geometry: Crop is the captured region in guest screen pixels (host screenshots are at the guest's
	// screen resolution, so screenshot pixels are screen pixels) and Scale/ScaleY are output pixels per screen pixel
	// on each axis (ScaleY 0 means Scale).
	Crop                   screenshotRegion
	Scale, ScaleY          float64
	OutputWidth, OutputHgt int
	HasImage               bool
	Nodes                  []proto.ControlInfo // the control tree, nil when not captured
	TreeWindow             *proto.WindowInfo   // the window Nodes belong to: Window, or the foreground window of a whole-screen observation
	TreeRootRuntimeID      string              // empty for a whole-window tree
	SearchResults          bool                // a result set is not a tree suitable for tree diffs
}

// treeWindow returns the window the control tree belongs to, nil when there is none.
func (o *observation) treeWindow() *proto.WindowInfo {
	if o.TreeWindow != nil {
		return o.TreeWindow
	}
	return o.Window
}

// toScreen maps a pixel of the output image to guest screen pixels (the coordinates of Backend.Click).
func (o *observation) toScreen(u, v int) (int, int, error) {
	if !o.HasImage {
		return 0, 0, refuse(codeInvalidArgument, "call vm_observe with screenshot: true, or pass index", nil, "observation %s has no screenshot to take coordinates from", o.ID)
	}
	if u < 0 || v < 0 || u >= o.OutputWidth || v >= o.OutputHgt {
		return 0, 0, refuse(codeInvalidArgument, "use coordinates inside the observation image", nil, "(%d, %d) is outside the %dx%d image of observation %s", u, v, o.OutputWidth, o.OutputHgt, o.ID)
	}
	sy := o.ScaleY
	if sy == 0 {
		sy = o.Scale
	}
	x := o.Crop.X + min(o.Crop.Width-1, int((float64(u)+0.5)/o.Scale))
	y := o.Crop.Y + min(o.Crop.Height-1, int((float64(v)+0.5)/sy))
	return x, y, nil
}

// node returns the control at index in the observation's tree.
func (o *observation) node(index int) (proto.ControlInfo, error) {
	if o.Nodes == nil {
		return proto.ControlInfo{}, refuse(codeInvalidArgument, "call vm_observe with controls: true", nil, "observation %s has no control tree", o.ID)
	}
	if index < 0 || index >= len(o.Nodes) {
		return proto.ControlInfo{}, refuse(codeStaleElement, "call vm_observe with controls: true and use an index from its tree", nil, "index %d is not in the %d-node tree of observation %s", index, len(o.Nodes), o.ID)
	}
	return o.Nodes[index], nil
}

// observationStore keeps the most recent observations per VM.
type observationStore struct {
	mu        sync.Mutex
	byID      map[string]*observation
	order     map[string][]string // VM -> IDs, oldest first
	limit     int
	revisions *observationRevisions
}

func newObservationStore() *observationStore {
	return newObservationStoreWithRevisions(&observationRevisions{byVM: map[string]observationVersion{}})
}

func newObservationStoreWithRevisions(revisions *observationRevisions) *observationStore {
	return &observationStore{byID: map[string]*observation{}, order: map[string][]string{}, limit: 8, revisions: revisions}
}

// newID returns a fresh observation ID ("o-" and 4 hex bytes).
func newID() string {
	var b [4]byte
	rand.Read(b[:])
	return "o-" + hex.EncodeToString(b[:])
}

// put stores o (assigning its ID and time if unset) and evicts the VM's oldest observations beyond the limit.
func (s *observationStore) put(o *observation) {
	if o.ID == "" {
		o.ID = newID()
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[o.ID] = o
	ids := append(s.order[o.VM], o.ID)
	for len(ids) > s.limit {
		delete(s.byID, ids[0])
		ids = ids[1:]
	}
	s.order[o.VM] = ids
}

// get returns the observation for id, or a stale_observation error when it is unknown or evicted.
func (s *observationStore) get(id string) (*observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.byID[id]; ok {
		return o, nil
	}
	return nil, refuse(codeStaleObservation, "call vm_observe again and use its observation_id", nil, "unknown observation_id %q (expired or never issued)", id)
}

// clear releases only this task's observations; shared VM revisions stay intact.
func (s *observationStore) clear(vm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vm == "" {
		s.byID = map[string]*observation{}
		s.order = map[string][]string{}
		return
	}
	for _, id := range s.order[vm] {
		delete(s.byID, id)
	}
	delete(s.order, vm)
}

// runID names one AI task's run; checkpoints and results carry it.
func newRunID() string {
	var b [8]byte
	rand.Read(b[:])
	return fmt.Sprintf("run-%s-%s", time.Now().Format("20060102-1504"), hex.EncodeToString(b[:]))
}

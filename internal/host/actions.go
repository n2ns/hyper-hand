package host

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

// observe_after modes (design 4.5).
const (
	afterNone       = "none"
	afterScreenshot = "screenshot"
	afterControls   = "controls"
	afterBoth       = "both"
)

const (
	defaultSettle = 300 * time.Millisecond
	maxSettleMs   = 5000
)

// controlActionTimeout bounds one UI Automation action: a hung target must not hold the connection (a variable so
// that tests can shorten it).
var controlActionTimeout = 12 * time.Second

// afterIn is the observe_after part of every action's input.
type afterIn struct {
	ObserveAfter string `json:"observe_after,omitempty" jsonschema:"what to observe after the action succeeded: none, screenshot, controls or both; the result then carries after, a vm_observe result (new observation_id, image first) taken settle_ms later. A failed action never observes"`
	SettleMs     int    `json:"settle_ms,omitempty" jsonschema:"milliseconds to wait before observing; default 300, maximum 5000"`
}

// validate resolves the mode (def when unset) and the settle delay.
func (in afterIn) validate(def string) (string, time.Duration, error) {
	mode := in.ObserveAfter
	if mode == "" {
		mode = def
	}
	switch mode {
	case afterNone, afterScreenshot, afterControls, afterBoth:
	default:
		return "", 0, refuse(codeInvalidArgument, "use observe_after none, screenshot, controls or both", nil, "unknown observe_after %q", in.ObserveAfter)
	}
	if in.SettleMs < 0 || in.SettleMs > maxSettleMs {
		return "", 0, refuse(codeInvalidArgument, fmt.Sprintf("use settle_ms between 0 and %d", maxSettleMs), nil, "settle_ms %d is out of range", in.SettleMs)
	}
	settle := defaultSettle
	if in.SettleMs != 0 {
		settle = time.Duration(in.SettleMs) * time.Millisecond
	}
	return mode, settle, nil
}

// actionOut is what an action's body returns: tool-specific result fields, the window that received the action (nil
// when untargeted) and the window to observe afterwards (0 for the whole screen).
type actionOut struct {
	fields  map[string]any
	window  *proto.WindowInfo
	observe uint64
}

// action is the state of one input action while it runs under d.input: the pinned VM, the latest window list and the
// observation it is checked against.
type action struct {
	d       *deps
	ctx     context.Context
	vm      string
	ws      *proto.WindowsResult // nil until listed, and when the agent did not answer (see listErr)
	listErr error
	obs     *observation
	mutated bool // a mutating call was dispatched, even if its result is an error
}

// run pins the VM, holds d.input while body checks and performs the action, then releases it and adds the
// observe_after observation to the result.
func (d *deps) run(ctx context.Context, vm string, after afterIn, defaultAfter string, body func(a *action) (*actionOut, error)) (*mcp.CallToolResult, error) {
	mode, settle, err := after.validate(defaultAfter)
	if err != nil {
		return nil, err
	}
	v, err := d.raw.Find(vm)
	if err != nil {
		return nil, err
	}
	out, err := func() (*actionOut, error) {
		d.input.Lock()
		defer d.input.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a := &action{d: d, ctx: ctx, vm: v.Name}
		defer func() {
			if a.mutated {
				d.taskObs(ctx).revise(v.Name)
			}
		}()
		return body(a)
	}()
	if err != nil {
		return nil, err
	}
	obj := out.fields
	if obj == nil {
		obj = map[string]any{"ok": true}
	}
	var window *windowRef
	if out.window != nil {
		window = refOf(*out.window)
	}
	obj["window"] = window
	if mode == afterNone {
		return jsonResult(obj)
	}
	select {
	case <-time.After(settle):
	case <-ctx.Done():
		obj["after_error"] = ctx.Err().Error()
		return jsonResult(obj)
	}
	shot := mode == afterScreenshot || mode == afterBoth
	o, png, err := d.observe(ctx, observeIn{VM: v.Name, Handle: out.observe, Screenshot: &shot, Controls: mode == afterControls || mode == afterBoth})
	if err != nil {
		// The action was performed: report it, and say why the observation is missing instead of failing the call.
		obj["after_error"] = asToolError(err).Error()
		return jsonResult(obj)
	}
	obj["after"] = o
	return jsonImageResult(obj, png)
}

// list returns the window list, fetched once; a failure is remembered (see windows and answered).
func (a *action) list() (*proto.WindowsResult, error) {
	if a.ws == nil && a.listErr == nil {
		a.ws, a.listErr = listWindowsResult(a.ctx, a.d.call, a.vm)
	}
	return a.ws, a.listErr
}

// relist fetches a fresh window list.
func (a *action) relist() error {
	a.ws, a.listErr = nil, nil
	_, err := a.list()
	return err
}

// offline reports whether the agent is unreachable (untargeted input then takes the console path). Any other failure
// of the window list, such as agent_outdated, is returned as the error.
func (a *action) offline() (bool, error) {
	if _, err := a.list(); err != nil {
		te := asToolError(agentRequired(err))
		if te.Code == codeAgentRequired {
			return true, nil
		}
		return false, te
	}
	return false, nil
}

// windows returns the window list for an action that cannot proceed without the agent.
func (a *action) windows() (*proto.WindowsResult, error) {
	ws, err := a.list()
	if err != nil {
		return nil, agentRequired(err)
	}
	return ws, nil
}

// agentRequired turns a transport failure into agent_required; errors returned
// by a responding agent (including UI Automation provider failures) pass through.
func agentRequired(err error) error {
	te := asToolError(err)
	// listWindowsResult may already have rendered the underlying error as a
	// toolError; inspect its reason as well as a still-wrapped transport error.
	if te.Code != codeFailed || (!agentUnreachable(err) && !agentUnreachable(errors.New(te.Reason))) {
		return te
	}
	return refuse(codeAgentRequired, "call vm_status; if the agent is not running, vm_install_agent, or act without observation_id and handle (raw screen input)", nil, "the guest agent did not answer: %v", err)
}

// checkSession is step 1 of the check chain: input must reach the agent's session and its programs. An untargeted
// action (raw screen input) may answer a UAC prompt: that is what the secure desktop then holds.
func (a *action) checkSession(targeted bool) error {
	if a.ws == nil || a.ws.Session == nil {
		return nil
	}
	s := a.ws.Session
	fields := map[string]any{"session": s}
	switch {
	case s.Locked:
		return refuse(codeSessionUnusable, "call vm_unlock", fields, "the guest session is locked")
	case !s.Console:
		return refuse(codeSessionUnusable, "call vm_doctor; the agent must run in the VM console session (sign in on the console, not an enhanced or remote desktop session)", fields, "the agent's session is not the VM console session, so Hyper-V input would not reach its windows")
	case s.SecureDesktop:
		if !targeted && s.Consent {
			return nil
		}
		return refuse(codeSessionUnusable, "answer the prompt first with vm_key or vm_click without observation_id (raw screen input), then retry", fields, "keyboard and mouse input goes to the secure desktop (a UAC prompt or the sign-in screen), not to the user's windows")
	}
	return nil
}

// observation is step 2: it loads id, checks that it belongs to this VM and that its window is unchanged (fresh).
func (a *action) observation(id string) (*observation, error) {
	o, err := a.d.taskObs(a.ctx).get(id)
	if err != nil {
		return nil, err
	}
	if o.VM != a.vm {
		return nil, refuse(codeStaleObservation, "call vm_observe again on this VM and use its observation_id", nil, "observation %s is of VM %q, not %q", o.ID, o.VM, a.vm)
	}
	a.obs = o
	if err := a.fresh(); err != nil {
		return nil, err
	}
	return o, nil
}

// fresh validates the VM lifecycle and the observed window's identity and geometry.
func (a *action) fresh() error {
	o := a.obs
	if o == nil {
		return nil
	}
	const next = "call vm_observe again and use its observation_id"
	version := a.d.taskObs(a.ctx).version(a.vm)
	if version.Busy != 0 {
		return refuse(codeStaleObservation, "wait for the running VM operation to finish, then call vm_observe again", nil, "VM %q has a mutating operation in progress", a.vm)
	}
	if o.Epoch != version.Epoch {
		return refuse(codeStaleObservation, next, nil, "VM %q restarted, was restored or changed agent since observation %s", a.vm, o.ID)
	}
	if o.Window == nil {
		return nil
	}
	cur, ok := findWindow(a.ws.Windows, o.Window.Handle)
	switch {
	case !ok:
		return refuse(codeStaleObservation, next, nil, "window %s of observation %s no longer exists", describe(*o.Window), o.ID)
	case !sameWindowIdentity(cur, *o.Window):
		return refuse(codeStaleObservation, next, nil, "window handle %d now identifies a different window than observation %s", cur.Handle, o.ID)
	case cur.Minimized:
		return refuse(codeStaleObservation, next, nil, "window %s of observation %s is minimized", describe(*o.Window), o.ID)
	case cur.Rect != o.Window.Rect:
		return refuse(codeStaleObservation, next, map[string]any{"rect": cur.Rect}, "window %s of observation %s moved or resized since it was observed", describe(*o.Window), o.ID)
	}
	return nil
}

// current returns the listed window with handle h.
func (a *action) current(h uint64) (proto.WindowInfo, error) {
	w, ok := findWindow(a.ws.Windows, h)
	if !ok {
		return w, refuse(codeNoWindow, "call vm_windows and use a listed handle", nil, "no visible window has handle %d", h)
	}
	return w, nil
}

// activate is step 3: when activate is set and neither target nor a window of its group is in the foreground, it asks
// the agent to focus target, re-lists the windows and re-checks the observation. It returns target's current entry.
// With activate unset the strict rule applies and checkUsable reports a background target.
func (a *action) activate(target proto.WindowInfo, activate bool) (proto.WindowInfo, error) {
	if fg, ok := foreground(a.ws.Windows); ok && inGroup(a.ws.Windows, target, fg.Handle) {
		return target, nil
	}
	if !activate {
		return target, nil
	}
	var r proto.FocusResult
	a.mutated = true
	if _, err := a.d.call(a.ctx, a.vm, proto.OpFocusWindow, proto.TitleArgs{Handle: target.Handle}, nil, &r); err != nil {
		if te := asToolError(err); te.Code != codeFailed {
			return target, te
		}
		return target, a.activateFailed(target, err.Error())
	}
	if err := a.relist(); err != nil {
		return target, agentRequired(err)
	}
	if err := a.fresh(); err != nil {
		return target, err
	}
	cur, err := a.current(target.Handle)
	if err != nil {
		return target, err
	}
	if fg, ok := foreground(a.ws.Windows); !ok || !inGroup(a.ws.Windows, cur, fg.Handle) {
		return cur, a.activateFailed(cur, "another window kept the foreground")
	}
	return cur, nil
}

// activateFailed names the window that holds the foreground and what to do about it.
func (a *action) activateFailed(target proto.WindowInfo, cause string) error {
	fg, ok := foreground(a.ws.Windows)
	if !ok {
		return refuse(codeActivateFailed, "call vm_observe (whole screen) to see what is on the screen", map[string]any{"foreground": nil}, "window %s could not be activated: %s; no window is in the foreground", describe(target), cause)
	}
	next := fmt.Sprintf("act on handle %d first", fg.Handle)
	if fg.PID != target.PID {
		next = fmt.Sprintf("act on handle %d first: it belongs to %s and probably is a dialog that blocks the target", fg.Handle, fg.Process)
	}
	return refuse(codeActivateFailed, next, map[string]any{"foreground": refOf(fg)}, "window %s could not be activated: %s; the foreground window is %s", describe(target), cause, describe(fg))
}

// hit is step 5: the window a click at screen point (x, y) reaches, refusing when another window covers it.
func (a *action) hit(target proto.WindowInfo, x, y int) (proto.WindowInfo, error) {
	var at proto.HandleResult
	if _, err := a.d.call(a.ctx, a.vm, proto.OpWindowAt, proto.PointArgs{X: x, Y: y}, nil, &at); err != nil {
		return proto.WindowInfo{}, agentRequired(err)
	}
	return checkHit(a.ws.Windows, target, x, y, at)
}

var integrityRank = map[string]int{"low": 1, "medium": 2, "high": 3, "system": 4}

// checkIntegrity is step 6: input to a window of higher integrity than the agent is dropped by Windows (UIPI).
// An unknown level ("") on either side never refuses.
func checkIntegrity(ws *proto.WindowsResult, w proto.WindowInfo) error {
	t, ag := integrityRank[w.Integrity], integrityRank[ws.AgentIntegrity]
	if t == 0 || ag == 0 || t <= ag {
		return nil
	}
	return refuse(codeIntegrityMismatch, "launch the application without admin, or use vm_launch with admin:true", map[string]any{"integrity": w.Integrity, "agent_integrity": ws.AgentIntegrity}, "window %s runs at %s integrity, above the agent's %s", describe(w), w.Integrity, ws.AgentIntegrity)
}

// windowTarget runs steps 3 and 4 for the group root w (an observation's window): activation, then enabled and
// foreground (checkUsable), on w's current entry, which it returns. Callers run step 5 (hit) and 6 (integrity).
func (a *action) windowTarget(w proto.WindowInfo, activate bool) (proto.WindowInfo, error) {
	w, err := a.current(w.Handle)
	if err != nil {
		return w, err
	}
	if w, err = a.activate(w, activate); err != nil {
		return w, err
	}
	return w, checkUsable(a.ws.Windows, w)
}

// treeTarget returns the window the observation's control tree belongs to, for index actions.
func (a *action) treeTarget(o *observation) (proto.WindowInfo, error) {
	tw := o.treeWindow()
	if tw == nil {
		return proto.WindowInfo{}, refuse(codeInvalidArgument, "call vm_observe with controls: true and use its observation_id and an index from its tree", nil, "observation %s has no control tree", o.ID)
	}
	cur, ok := findWindow(a.ws.Windows, tw.Handle)
	if !ok || !sameWindowIdentity(cur, *tw) {
		return proto.WindowInfo{}, refuse(codeStaleObservation, "call vm_observe again and use its observation_id", nil, "the control tree window of observation %s no longer has the same identity", o.ID)
	}
	return *tw, nil
}

// locate re-finds node by its runtime ID in its window and returns its current rectangle, so that a control that moved
// (scrolled list, re-laid-out dialog) is clicked where it is now; a vanished control is stale_element. Nodes without a
// runtime ID keep the rectangle of the observation.
func (a *action) locate(o *observation, w proto.WindowInfo, node proto.ControlInfo) (proto.Rect, error) {
	if node.RuntimeID == "" {
		if err := a.freshCoordinates(o, int(node.Rect.Left+node.Rect.Right)/2, int(node.Rect.Top+node.Rect.Bottom)/2); err != nil {
			return proto.Rect{}, err
		}
		return node.Rect, nil
	}
	r, err := a.controlAction(o, w, node, "Locate", "")
	if err != nil {
		return proto.Rect{}, err
	}
	if r.Rect == nil {
		return node.Rect, nil
	}
	return *r.Rect, nil
}

// pointTarget runs steps 1 to 6 for a pointer action at image pixel (u, v) of observation id, or at the centre of node
// index when index is set (the node is re-located first). It returns the screen point, the window the pointer reaches
// (zero when a whole-screen observation is clicked by coordinates, which has no window checks) and the observation.
func (a *action) pointTarget(id string, u, v int, index *int, activate bool) (x, y int, hit proto.WindowInfo, o *observation, err error) {
	if _, err = a.windows(); err != nil {
		return
	}
	if err = a.checkSession(true); err != nil {
		return
	}
	if o, err = a.observation(id); err != nil {
		return
	}
	var window proto.WindowInfo
	if index != nil {
		n, e := o.node(*index)
		if e != nil {
			return 0, 0, hit, o, e
		}
		if window, err = a.treeTarget(o); err != nil {
			return
		}
		if window, err = a.windowTarget(window, activate); err != nil {
			return 0, 0, hit, o, err
		}
		r, e := a.locate(o, window, n)
		if e != nil {
			return 0, 0, hit, o, e
		}
		x, y = int(r.Left+r.Right)/2, int(r.Top+r.Bottom)/2
	} else {
		if x, y, err = o.toScreen(u, v); err != nil {
			return
		}
		if o.Window == nil {
			return x, y, hit, o, a.freshCoordinates(o, x, y)
		}
		if window, err = a.windowTarget(*o.Window, activate); err != nil {
			return 0, 0, hit, o, err
		}
		if err = a.freshCoordinates(o, x, y); err != nil {
			return 0, 0, hit, o, err
		}
	}
	if hit, err = a.hit(window, x, y); err != nil {
		return 0, 0, hit, o, err
	}
	return x, y, hit, o, checkIntegrity(a.ws, hit)
}

// inputTarget runs steps 1 to 6 for keyboard input to the window sel selects: the input goes to the window of its
// group that is in the foreground (inputWindow). It returns the root and the receiving window.
func (a *action) inputTarget(sel windowSelector, activate bool) (root, target proto.WindowInfo, err error) {
	if _, err = a.windows(); err != nil {
		return
	}
	if err = a.checkSession(true); err != nil {
		return
	}
	if root, err = resolveWindow(a.ws.Windows, sel); err != nil {
		return
	}
	if root, err = a.activate(root, activate); err != nil {
		return
	}
	if root, target, err = inputWindow(a.ws.Windows, windowSelector{Handle: root.Handle}); err != nil {
		return
	}
	return root, target, checkIntegrity(a.ws, target)
}

// typeKeys sends text to target through the agent (Unicode SendInput). A failure after some events were injected is
// partial_input with applied_chars and total_chars.
func (a *action) typeKeys(text string, target proto.WindowInfo) (applied, total int, err error) {
	text = strings.ReplaceAll(text, "\r\n", "\n") // as the agent counts it
	total = len([]rune(text))
	var r proto.TypeKeysResult
	a.mutated = true
	if _, err := a.d.call(a.ctx, a.vm, proto.OpTypeKeys, proto.TypeKeysArgs{Text: text, Handle: target.Handle, PID: target.PID}, nil, &r); err != nil {
		if te := asToolError(err); te.Code != codeFailed {
			return 0, total, te
		}
		applied = appliedChars(text, injectedEvents(err))
		fields := map[string]any{"applied_chars": applied, "total_chars": total}
		if applied == 0 {
			return 0, total, refuse(codeFailed, "call vm_observe with controls: true to confirm the focused control, then retry", fields, "nothing was typed: %v", err)
		}
		return applied, total, refuse(codePartialInput, "call vm_observe with controls: true to see what arrived; do not retype blindly", fields, "typing stopped after %d of %d characters: %v", applied, total, err)
	}
	return total, total, nil
}

var stoppedAfter = regexp.MustCompile(`stopped after (\d+) injected events`)

// injectedEvents parses the event count from the agent's type_keys error, 0 when it reports none.
func injectedEvents(err error) int {
	m := stoppedAfter.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// appliedChars counts the leading characters of text whose input events (a key down and up per UTF-16 unit) fit in
// events.
func appliedChars(text string, events int) int {
	n := 0
	for _, r := range text {
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		if events < 2*units {
			break
		}
		events -= 2 * units
		n++
	}
	return n
}

// controlAction performs action on node (of observation o, in window w) through the agent with a hard timeout, mapping
// the agent's errors to stale_element, unsupported_pattern and target_not_responding (the agent's own UI Automation
// timeout included).
func (a *action) controlAction(o *observation, w proto.WindowInfo, node proto.ControlInfo, action, value string) (proto.ControlActionResult, error) {
	var r proto.ControlActionResult
	if node.RuntimeID == "" && o.Revision != a.d.taskObs(a.ctx).version(a.vm).Revision {
		return r, refuse(codeStaleObservation, "call vm_observe again and use its observation_id", nil, "control [%d] has no stable runtime ID and observation %s predates a VM mutation", node.Index, o.ID)
	}
	ctx, cancel := context.WithTimeout(a.ctx, controlActionTimeout)
	defer cancel()
	if action != "Locate" {
		a.mutated = true
	}
	pid := node.PID
	if pid == 0 {
		pid = w.PID
	}
	_, err := a.d.call(ctx, a.vm, proto.OpControlAction, proto.ControlActionArgs{Handle: w.Handle, PID: pid, RuntimeID: node.RuntimeID, Action: action, Value: value, HintRect: &node.Rect}, nil, &r)
	if err == nil {
		return r, nil
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (ctx.Err() != nil && a.ctx.Err() == nil) ||
		strings.Contains(msg, "UI Automation interrupted") || strings.Contains(msg, "timed out"):
		return r, refuse(codeTargetNotResponding, "call vm_observe on the window to see whether it answers; close it with vm_key alt+f4 or vm_exec if it hangs", nil, "window %s did not perform %s within %v", describe(w), action, controlActionTimeout)
	case strings.HasPrefix(msg, "element not found"):
		return r, refuse(codeStaleElement, "call vm_observe with controls: true and use an index from its tree", nil, "control [%d] %s %q of observation %s no longer exists: %v", node.Index, proto.ControlTypeName(node.ControlType), node.Name, o.ID, err)
	case strings.HasPrefix(msg, "unsupported pattern"):
		return r, refuse(codeUnsupportedPattern, "use vm_set_value for SetValue, vm_invoke for another supported action, or vm_click with this index", map[string]any{"supported": supportedControlActions(msg, node)}, "control [%d] %s %q does not support %s", node.Index, proto.ControlTypeName(node.ControlType), node.Name, action)
	}
	return r, agentRequired(err)
}

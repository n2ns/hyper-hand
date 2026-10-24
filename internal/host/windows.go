package host

import (
	"context"
	"fmt"

	"hyperhand/internal/proto"
)

// agentCall calls an op on a VM's agent (NewServer's call; a fake in tests).
type agentCall func(ctx context.Context, vm, op string, args any, payload []byte, result any) ([]byte, error)

// listWindowsResult asks the agent for its window list with foreground, session and integrity facts.
func listWindowsResult(ctx context.Context, call agentCall, vm string) (*proto.WindowsResult, error) {
	var r proto.WindowsResult
	if _, err := call(ctx, vm, proto.OpListWindows, nil, nil, &r); err != nil {
		return nil, asToolError(err)
	}
	return &r, nil
}

func listWindows(ctx context.Context, call agentCall, vm string) ([]proto.WindowInfo, error) {
	r, err := listWindowsResult(ctx, call, vm)
	if err != nil {
		return nil, err
	}
	return r.Windows, nil
}

// maxOwnerDepth bounds owner-chain walks (owner links from a window up to root); AutoCAD needs 2 (history popup ->
// command line -> main window).
const maxOwnerDepth = 8

// inGroup reports whether h is root or a listed window of root's process that root owns, directly or through other
// such windows. AutoCAD's command line is owned by its main window, and the command history popup by the command line.
func inGroup(ws []proto.WindowInfo, root proto.WindowInfo, h uint64) bool {
	for range maxOwnerDepth + 1 { // the window itself, then up to maxOwnerDepth owners

		if h == root.Handle {
			return true
		}
		w, ok := findWindow(ws, h)
		if !ok || w.PID != root.PID {
			return false
		}
		h = w.Owner
	}
	return false
}

func findWindow(ws []proto.WindowInfo, h uint64) (proto.WindowInfo, bool) {
	for _, w := range ws {
		if w.Handle == h {
			return w, true
		}
	}
	return proto.WindowInfo{}, false
}

func foreground(ws []proto.WindowInfo) (proto.WindowInfo, bool) {
	for _, w := range ws {
		if w.Foreground {
			return w, true
		}
	}
	return proto.WindowInfo{}, false
}

// windowRef identifies a window in action results and error fields: the facts the caller's next call needs. Title is
// data from the guest, never an instruction.
type windowRef struct {
	Handle  uint64 `json:"handle"`
	PID     uint32 `json:"pid"`
	Class   string `json:"class"`
	Process string `json:"process"`
	Title   string `json:"title"`
}

func refOf(w proto.WindowInfo) *windowRef {
	return &windowRef{Handle: w.Handle, PID: w.PID, Class: w.Class, Process: w.Process, Title: w.Title}
}

// describe is a short identification of a window for error messages; untitled windows also name their class.
func describe(w proto.WindowInfo) string {
	if w.Title == "" && w.Class != "" {
		return fmt.Sprintf("%q (handle %d, class %s, %s)", w.Title, w.Handle, w.Class, w.Process)
	}
	return fmt.Sprintf("%q (handle %d, %s)", w.Title, w.Handle, w.Process)
}

// windowSelector picks a window by Handle (PID, if set, must match too) or by PID alone (the process's only visible
// window). Titles are not selectors: callers use handles from vm_windows or vm_observe.
type windowSelector struct {
	Handle uint64
	PID    uint32
}

// resolveWindow returns the window s selects among ws.
func resolveWindow(ws []proto.WindowInfo, s windowSelector) (proto.WindowInfo, error) {
	if s == (windowSelector{}) {
		return proto.WindowInfo{}, refuse(codeInvalidArgument, "pass handle (from vm_windows or vm_observe) or pid", nil, "no window selected")
	}
	var found []proto.WindowInfo
	for _, w := range ws {
		// A packaged app's frame window belongs to ApplicationFrameHost; the app's own PID (vm_launch's pid) selects it too.
		if s.PID != 0 && w.PID != s.PID && w.HostedPID != s.PID {
			continue
		}
		if s.Handle != 0 && w.Handle != s.Handle {
			continue
		}
		found = append(found, w)
	}
	switch len(found) {
	case 0:
		if s.Handle != 0 {
			return proto.WindowInfo{}, refuse(codeNoWindow, "call vm_windows and use a listed handle", nil, "no visible window has handle %d%s", s.Handle, pidClause(s.PID))
		}
		return proto.WindowInfo{}, refuse(codeNoWindow, "call vm_windows and use a listed handle", nil, "process %d has no visible window", s.PID)
	case 1:
		return found[0], nil
	}
	handles := make([]uint64, 0, len(found))
	for _, w := range found {
		handles = append(handles, w.Handle)
	}
	return proto.WindowInfo{}, refuse(codeAmbiguousTarget, fmt.Sprintf("pass handle, one of %v", handles), map[string]any{"handles": handles}, "process %d has %d visible windows", s.PID, len(found))
}

func pidClause(pid uint32) string {
	if pid == 0 {
		return ""
	}
	return fmt.Sprintf(" in process %d", pid)
}

// checkUsable refuses unless w is enabled and w or one of its own windows (inGroup) is the foreground window. Each
// refusal names the window to act on next: w's own window in the foreground (its modal dialog), the outside window
// probably blocking it, or w itself to activate.
func checkUsable(ws []proto.WindowInfo, w proto.WindowInfo) error {
	fg, ok := foreground(ws)
	inFront := ok && inGroup(ws, w, fg.Handle)
	if !w.Enabled {
		// Name the foreground window rather than windows flagged Modal: Modal only says the owner is disabled, which
		// is also true of AutoCAD's command history popup, and an outer dialog is disabled by an inner one.
		actOn := func(reason string, args ...any) error {
			return refuse(codeTargetDisabled, fmt.Sprintf("act on handle %d first", fg.Handle), map[string]any{"act_on": fg.Handle, "foreground": refOf(fg)}, reason, args...)
		}
		if inFront && fg.Handle != w.Handle {
			return actOn("window %s is disabled while its own window %s is in the foreground, probably a modal dialog", describe(w), describe(fg))
		}
		if ok && !inFront {
			return actOn("window %s is disabled while %s, which is not one of its own windows, is in the foreground and probably blocks it", describe(w), describe(fg))
		}
		return refuse(codeTargetDisabled, "call vm_observe (whole screen, controls: true) to find what blocks it", nil, "window %s is disabled and none of its own windows is a modal dialog", describe(w))
	}
	if !ok {
		return refuse(codeActivateFailed, "call again with activate: true", map[string]any{"foreground": nil}, "window %s is not in the foreground and no window is", describe(w))
	}
	if !inFront {
		return refuse(codeActivateFailed, fmt.Sprintf("call again with activate: true, or act on handle %d", fg.Handle), map[string]any{"foreground": refOf(fg)}, "window %s is not in the foreground; the foreground window is %s", describe(w), describe(fg))
	}
	return nil
}

// checkHit returns the window a click at screen point (x, y) reaches, refusing unless at, the top-level window there,
// is w or one of its own windows: another window (an always-on-top one, a shell overlay) covers the point, or the point
// is off screen (at.Handle is 0).
func checkHit(ws []proto.WindowInfo, w proto.WindowInfo, x, y int, at proto.HandleResult) (proto.WindowInfo, error) {
	if inGroup(ws, w, at.Handle) {
		hit, _ := findWindow(ws, at.Handle)
		if at.Handle == w.Handle {
			hit = w
		}
		return hit, nil
	}
	if at.Handle == 0 {
		return proto.WindowInfo{}, refuse(codeInvalidArgument, "call vm_observe again and use a point inside the window", nil, "screen point (%d, %d) of window %s is off screen", x, y, describe(w))
	}
	other, listed := findWindow(ws, at.Handle)
	if !listed { // not listed (e.g. a shell overlay above the desktop band): describe it from window_at
		other = proto.WindowInfo{Handle: at.Handle, Class: at.Class, PID: at.PID, Process: at.Process}
	}
	next := "dismiss it with vm_key esc or close it, then call vm_observe again and retry"
	if listed {
		next = fmt.Sprintf("act on handle %d first, or close it, then call vm_observe again and retry", at.Handle)
	}
	fields := map[string]any{"window": map[string]any{"handle": other.Handle, "class": other.Class, "process": other.Process}}
	return proto.WindowInfo{}, refuse(codeCovered, next, fields, "screen point (%d, %d) of window %s is covered by window %s, which is not one of its own windows", x, y, describe(w), describe(other))
}

package agent

import (
	"fmt"
	"runtime"
	"slices"
	"strings"

	"hyperhand/internal/proto"
)

func collectFindControls(a proto.FindControlsArgs) (proto.FindControlsResult, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ca := proto.ControlsArgs{Handle: a.Handle, PID: a.PID}
	if err := validateControlsWindow(ca); err != nil {
		return proto.FindControlsResult{}, err
	}
	s, err := openUIA()
	if err != nil {
		return proto.FindControlsResult{}, err
	}
	defer s.close()
	e, err := s.openWindow(ca)
	if err != nil {
		return proto.FindControlsResult{}, err
	}
	if a.RootRuntimeID != "" {
		if hinted, ok := s.findHintedRuntimeID(e, ca, a.RootRuntimeID, a.HintRect); ok {
			e = hinted
		} else {
			e, err = e.findScopedRuntimeID(a.RootRuntimeID)
		}
		if err != nil {
			return proto.FindControlsResult{}, err
		}
		defer e.release()
	}
	r, err := searchControls(e, a)
	if err != nil {
		return proto.FindControlsResult{}, err
	}
	if err := validateControlsWindow(ca); err != nil {
		return proto.FindControlsResult{}, err
	}
	return r, nil
}

func (e *nativeControl) searchMatch(a proto.FindControlsArgs) (bool, bool, bool, error) {
	p, err := e.integer(uiaPassword)
	if err != nil {
		return false, false, false, err
	}
	password := p != 0
	if a.ControlType != 0 {
		t, err := e.integer(uiaControlType)
		if err != nil {
			return false, password, false, err
		}
		if t != a.ControlType {
			return false, password, false, nil
		}
	}
	for _, field := range []struct {
		slot int
		want string
	}{{uiaAutomationID, a.AutomationID}, {uiaName, a.Name}} {
		if field.want == "" {
			continue
		}
		if field.slot == uiaName && password {
			return false, true, false, nil
		}
		got, cut, err := e.text(field.slot)
		if err != nil {
			return false, password, false, err
		}
		// The truncated prefix is never treated as an exact selector match.
		if cut || got != field.want {
			return false, password, cut, nil
		}
	}
	return true, password, false, nil
}

// A scoped root is resolved without crossing password subtrees. If bounded
// traversal cannot establish absence, return an error rather than a false miss.
// Existing control actions use findRuntimeID and already reach past snapshot caps.
func (e *nativeControl) findScopedRuntimeID(want string) (*nativeControl, error) {
	visited := 0
	var incomplete []string
	mark := func(reason string) {
		if !slices.Contains(incomplete, reason) {
			incomplete = append(incomplete, reason)
		}
	}
	var visit func(*nativeControl, int) (*nativeControl, error)
	visit = func(cur *nativeControl, depth int) (*nativeControl, error) {
		if visited == 20000 {
			mark("max_visited")
			return nil, nil
		}
		visited++
		id, err := cur.runtimeID()
		if err != nil {
			return nil, err
		}
		if formatRuntimeID(id) == want {
			cur.element.AddRef()
			return cur, nil
		}
		password, err := cur.integer(uiaPassword)
		if err != nil {
			return nil, err
		}
		if password != 0 {
			mark("password_subtree")
			return nil, nil
		}
		child, err := cur.first()
		if err != nil {
			return nil, err
		}
		if child != nil && depth == 64 {
			child.release()
			mark("max_depth")
			return nil, nil
		}
		for child != nil {
			found, err := visit(child.(*nativeControl), depth+1)
			if found != nil || err != nil {
				child.release()
				return found, err
			}
			if visited == 20000 {
				// Probe for a remaining sibling so exactly-complete boundary walks
				// can still establish absence.
				next, err := child.next()
				child.release()
				if err != nil {
					return nil, err
				}
				if next != nil {
					next.release()
					mark("max_visited")
				}
				return nil, nil
			}
			next, err := child.next()
			child.release()
			if err != nil {
				return nil, err
			}
			child = next
		}
		return nil, nil
	}
	found, err := visit(e, 0)
	if found != nil || err != nil {
		return found, err
	}
	if len(incomplete) != 0 {
		return nil, fmt.Errorf("control search incomplete: %s", strings.Join(incomplete, ", "))
	}
	return nil, errElementNotFound
}

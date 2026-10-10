package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"hyperhand/internal/proto"
)

func findControlsArgs(a proto.FindControlsArgs) (proto.FindControlsArgs, error) {
	if _, err := controlsArgs(proto.ControlsArgs{Handle: a.Handle, PID: a.PID}); err != nil {
		return a, err
	}
	if a.AutomationID == "" && a.Name == "" && a.ControlType == 0 {
		return a, errors.New("find_controls requires automation_id, name, or control_type")
	}
	if a.ControlType != 0 && (a.ControlType < 50000 || a.ControlType > 50040) {
		return a, errors.New("control_type must be a UIA control type ID between 50000 and 50040")
	}
	if strings.ContainsRune(a.AutomationID+a.Name+a.RootRuntimeID, 0) {
		return a, errors.New("control selectors must not contain NUL")
	}
	if a.MaxDepth == 0 {
		a.MaxDepth = 32
	}
	if a.MaxVisited == 0 {
		a.MaxVisited = 5000
	}
	if a.MaxMatches == 0 {
		a.MaxMatches = 20
	}
	if a.MaxDepth < 1 || a.MaxDepth > 64 {
		return a, errors.New("max_depth must be between 1 and 64")
	}
	if a.MaxVisited < 1 || a.MaxVisited > 20000 {
		return a, errors.New("max_visited must be between 1 and 20000")
	}
	if a.MaxMatches < 1 || a.MaxMatches > 100 {
		return a, errors.New("max_matches must be between 1 and 100")
	}
	return a, nil
}

func listControlSubtree(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.ControlsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, nil, err
	}
	if a.RootRuntimeID == "" || strings.ContainsRune(a.RootRuntimeID, 0) {
		return nil, nil, errors.New("list_control_subtree requires a nonempty root_runtime_id without NUL")
	}
	return listControlsArgs(ctx, a)
}

func findControls(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.FindControlsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, nil, err
	}
	a, err := findControlsArgs(a)
	if err != nil {
		return nil, nil, err
	}
	opctx, cancel := context.WithTimeout(ctx, uiaTimeout)
	defer cancel()
	cmd, err := helperCommand(opctx)
	if err != nil {
		return nil, nil, err
	}
	reply, err := runHelper(opctx, cmd, helperRequest{Find: &a})
	if err != nil {
		return nil, nil, err
	}
	if reply.Find == nil {
		return nil, nil, errors.New("UI Automation helper returned no search result")
	}
	return *reply.Find, nil, nil
}

// Search reads selectors and the password flag cheaply; full patterns, state and
// values are collected only for matching elements.
type controlSearchElement interface {
	searchMatch(proto.FindControlsArgs) (match, password, textCut bool, err error)
}

func searchControls(root controlElement, a proto.FindControlsArgs) (proto.FindControlsResult, error) {
	r := proto.FindControlsResult{Matches: []proto.ControlInfo{}}
	truncate := func(reason string) {
		r.Truncated = true
		if !slices.Contains(r.Truncation, reason) {
			r.Truncation = append(r.Truncation, reason)
		}
	}
	stop := false
	var visit func(controlElement, int) error
	visit = func(e controlElement, depth int) error {
		if r.Visited == a.MaxVisited {
			truncate("max_visited")
			stop = true
			return nil
		}
		r.Visited++
		match, password, cut, err := e.(controlSearchElement).searchMatch(a)
		if err != nil {
			return err
		}
		if cut {
			truncate("text_length")
		}
		if match {
			if len(r.Matches) == a.MaxMatches {
				truncate("max_matches")
				stop = true
				return nil
			}
			n, nowPassword, cut, err := e.info()
			if err != nil {
				return err
			}
			if cut {
				truncate("text_length")
			}
			password = password || nowPassword
			stillMatches := true
			if cut && (a.Name != "" || a.AutomationID != "") {
				// info's text cap also covers values and class names. Re-read
				// just the selectors to distinguish those from a shortened name
				// or automation ID that cannot establish an exact match.
				var currentPassword bool
				stillMatches, currentPassword, _, err = e.(controlSearchElement).searchMatch(a)
				if err != nil {
					return err
				}
				password = password || currentPassword
			}
			if !stillMatches || (a.Name != "" && (password || n.Name != a.Name)) || (a.AutomationID != "" && n.AutomationID != a.AutomationID) || (a.ControlType != 0 && n.ControlType != a.ControlType) {
				// UIA reads are not atomic; never return a node whose collected
				// properties visibly stopped matching during the read.
				truncate("element_changed")
			} else {
				n.Index, n.Parent, n.Depth = len(r.Matches), -1, depth
				r.Matches = append(r.Matches, n)
			}
		}
		if password {
			truncate("password_subtree")
			return nil
		}
		child, err := e.first()
		if err != nil {
			return err
		}
		if child != nil && depth == a.MaxDepth {
			child.release()
			truncate("max_depth")
			return nil
		}
		for child != nil {
			if err := visit(child, depth+1); err != nil {
				child.release()
				return err
			}
			if stop {
				child.release()
				return nil
			}
			next, err := child.next()
			child.release()
			if err != nil {
				return err
			}
			child = next
		}
		return nil
	}
	if err := visit(root, 0); err != nil {
		return proto.FindControlsResult{}, err
	}
	return r, nil
}

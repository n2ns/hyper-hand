package host

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

type launchIn struct {
	VM           string   `json:"vm,omitempty" jsonschema:"VM name from vm_list (required)"`
	Path         string   `json:"path" jsonschema:"program path in the guest"`
	Args         []string `json:"args,omitempty" jsonschema:"command-line arguments"`
	Cwd          string   `json:"cwd,omitempty" jsonschema:"working directory in the guest"`
	WaitWindowMs int      `json:"wait_window_ms,omitempty" jsonschema:"how long to wait for the process's first visible window; default 60000"`
	Admin        bool     `json:"admin,omitempty" jsonschema:"start elevated (administrator)"`
}

// launchOut is vm_launch's result: the process and its first visible top-level window.
type launchOut struct {
	PID       uint32 `json:"pid"`
	Handle    uint64 `json:"handle"`
	Title     string `json:"title"`
	Class     string `json:"class"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

// launchPollInterval is how often vm_launch lists the guest's windows while waiting for the new process's window.
var launchPollInterval = 300 * time.Millisecond

// registerLaunch registers vm_launch.
func registerLaunch(d *deps) {
	addToolIn(d, toolSpec{name: "vm_launch", desc: "Start a program in the guest as a detached process (it outlives the call) and wait until it shows a visible top-level window; returns the pid and the window's handle, title and class to pass to vm_observe and the actions. A splash screen or dialog of the process counts as its window. On no_window the process keeps running."}, func(ctx context.Context, in launchIn) (*mcp.CallToolResult, error) {
		if in.Path == "" {
			return nil, refuse(codeInvalidArgument, "pass path", nil, "path is required")
		}
		timeout := time.Duration(in.WaitWindowMs) * time.Millisecond
		if in.WaitWindowMs <= 0 {
			timeout = 60 * time.Second
		}
		c, err := d.m.Client(in.VM) // resolve the VM once: the polling must not move to another VM
		if err != nil {
			return nil, vmErr(err)
		}
		start := time.Now()
		var l proto.LaunchResult
		if _, err := c.Call(ctx, proto.OpLaunch, proto.LaunchArgs{Path: in.Path, Args: in.Args, Cwd: in.Cwd, Admin: in.Admin}, nil, &l); err != nil {
			return nil, agentErr(err)
		}
		w, found, err := waitWindow(ctx, c, l.PID, timeout)
		if err != nil {
			return nil, agentErr(err)
		}
		if !found {
			return nil, refuse(codeNoWindow, "call vm_windows later, or vm_observe without handle to see a splash screen or dialog", map[string]any{"pid": l.PID},
				"process %d showed no visible window within %v", l.PID, timeout)
		}
		return jsonResult(launchOut{PID: l.PID, Handle: w.Handle, Title: w.Title, Class: w.Class, ElapsedMs: time.Since(start).Milliseconds()})
	})
}

// waitWindow lists the guest's windows every launchPollInterval until a visible top-level window of pid appears
// (one with a title is preferred over the first) or timeout passes; found is false on timeout.
func waitWindow(ctx context.Context, c *Client, pid uint32, timeout time.Duration) (w proto.WindowInfo, found bool, err error) {
	end := time.Now().Add(timeout)
	for {
		var r proto.WindowsResult
		if _, err := c.Call(ctx, proto.OpListWindows, nil, nil, &r); err != nil {
			return w, false, err
		}
		if w, found = windowOfPID(r.Windows, pid); found {
			return w, true, nil
		}
		if !time.Now().Before(end) {
			return w, false, nil
		}
		timer := time.NewTimer(min(launchPollInterval, time.Until(end)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return w, false, ctx.Err()
		case <-timer.C:
		}
	}
}

// windowOfPID picks pid's window from ws: the first with a title, else the first.
func windowOfPID(ws []proto.WindowInfo, pid uint32) (proto.WindowInfo, bool) {
	var first *proto.WindowInfo
	for i := range ws {
		if ws[i].PID != pid {
			continue
		}
		if ws[i].Title != "" {
			return ws[i], true
		}
		if first == nil {
			first = &ws[i]
		}
	}
	if first != nil {
		return *first, true
	}
	return proto.WindowInfo{}, false
}

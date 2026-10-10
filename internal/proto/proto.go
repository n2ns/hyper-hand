// Package proto is the wire protocol between the host tray (hyperhand.exe) and the guest agent
// (hyperhand-agent.exe) over a Hyper-V socket. The host sends one request and waits for its response.
//
// A frame is: uint32 header length | uint64 payload length | header JSON | payload bytes.
// The payload carries file contents (streamed: see WriteFrameFrom / ReadHeader) and screenshots (PNG); it is empty
// otherwise.
package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
)

// ServiceID is the Hyper-V socket service GUID the agent listens on.
const ServiceID = "3ce544e1-2645-4383-b332-fedf8a18736b"

// Version is set at build time for releases: -ldflags "-X hyperhand/internal/proto.Version=0.1.0".
var Version = "dev"

// Protocol is the wire protocol generation. The host refuses an agent whose ping reports a lower Protocol with the
// error code agent_outdated; there is no compatibility path for older agents.
const Protocol = 5

const (
	OpPing         = "ping"          // -> PingResult
	OpExec         = "exec"          // ExecArgs -> ExecResult
	OpWriteFile    = "write_file"    // PathArgs + payload
	OpReadFile     = "read_file"     // PathArgs -> payload
	OpListDir      = "list_dir"      // PathArgs -> ListDirResult
	OpHashFiles    = "hash_files"    // PathsArgs -> HashesResult
	OpClipboardGet = "clipboard_get" // -> TextResult
	OpClipboardSet = "clipboard_set" // TextArgs
	OpFocusWindow  = "focus_window"  // TitleArgs -> FocusResult (the matched title and handle)
	OpListWindows  = "list_windows"  // ListWindowsArgs (optional) -> WindowsResult (windows, foreground, focused control, session)
	OpWindowAt     = "window_at"     // PointArgs -> HandleResult
	OpLaunch       = "launch"        // LaunchArgs -> LaunchResult: start a detached process
	OpHScroll      = "hscroll"       // HScrollArgs: horizontal wheel at a screen point (SendInput; the Hyper-V mouse has none)
	OpWait         = "wait"          // WaitArgs -> WaitResult
	OpSessionState = "session_state" // -> SessionStateResult
	OpUpdateAgent  = "update_agent"  // payload = new exe; the agent answers, replaces itself and restarts
)

type Request struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type Response struct {
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// SessionStateResult describes the agent's own logon session: Locked is its lock state as Windows reports it; Console
// means it is the session on the VM console, which the Hyper-V keyboard types into; SecureDesktop means keyboard input
// goes to a desktop the user cannot open (the sign-in screen's password box or a UAC prompt), not to the user's
// programs; LogonUI means the sign-in screen runs in the session; Consent means a UAC prompt (consent.exe) is open.
type SessionStateResult struct {
	Locked        bool `json:"locked"`
	Console       bool `json:"console"`
	SecureDesktop bool `json:"secure_desktop"`
	LogonUI       bool `json:"logonui"`
	Consent       bool `json:"consent"`
}

type PingResult struct {
	InstallID string `json:"install_id,omitempty"` // identifies the installation that launched this process
	Version   string `json:"version"`
	Protocol  int    `json:"protocol"` // see Protocol; 0 from agents that predate it
	Hostname  string `json:"hostname"`
	User      string `json:"user"`
}

// ExecArgs: Shell is "powershell" (default) or "cmd"; TimeoutMs 0 means 60 s. Admin runs it elevated; a UAC prompt in
// the guest is waited for within the timeout.
type ExecArgs struct {
	Command   string `json:"command"`
	Shell     string `json:"shell,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	Admin     bool   `json:"admin,omitempty"`
}

type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
	// ElevationPending: with Admin, the timeout came before the elevated worker received the command (a UAC prompt
	// was not answered, or the timeout was too short to elevate), so the command did not run. TimedOut is then true.
	ElevationPending bool `json:"elevation_pending,omitempty"`
}

type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

type ListDirResult struct {
	Entries []DirEntry `json:"entries"`
}

type PathsArgs struct {
	Paths []string `json:"paths"`
}

// HashesResult: the lowercase hex SHA-256 of each path, in order; "" when the file is missing or unreadable.
type HashesResult struct {
	Hashes []string `json:"hashes"`
}

type PathArgs struct {
	Path string `json:"path"`
}

type TextArgs struct {
	Text string `json:"text"`
}

// TitleArgs: a non-zero Handle selects that window and Title is ignored.
type TitleArgs struct {
	Title  string `json:"title"` // case-insensitive substring of the window title
	Handle uint64 `json:"handle,omitempty"`
}

// FocusResult: Text is the focused window's title (an older host reads only it).
type FocusResult struct {
	Text   string `json:"text"`
	Handle uint64 `json:"handle"`
}

// Rect is a window's visible frame in guest screen pixels (the coordinates of host screenshots and untargeted vm_click).
type Rect struct {
	Left   int32 `json:"left"`
	Top    int32 `json:"top"`
	Right  int32 `json:"right"`
	Bottom int32 `json:"bottom"`
}

// WindowInfo describes a visible top-level window. Owner is the owner window's handle (0 if none); Modal means the
// owner is disabled, as it is while a modal dialog runs. GroupRoot is the handle reached by following Owner links
// through listed windows of the same process (at most 8 links): the window itself when it has no such owner.
// Integrity is the process token's integrity level: "low", "medium", "high", "system", or "" when it cannot be read.
type WindowInfo struct {
	Handle     uint64 `json:"handle"`
	Title      string `json:"title"`
	Class      string `json:"class"`
	PID        uint32 `json:"pid"`
	Process    string `json:"process"`
	Rect       Rect   `json:"rect"`
	Enabled    bool   `json:"enabled"`
	Foreground bool   `json:"foreground"`
	Minimized  bool   `json:"minimized"`
	Owner      uint64 `json:"owner,omitempty"`
	Modal      bool   `json:"modal"`
	GroupRoot  uint64 `json:"group_root"`
	Integrity  string `json:"integrity,omitempty"`
}

// FocusedControl summarizes the UI Automation element with keyboard focus (IUIAutomation::GetFocusedElement). Window
// is the top-level window containing it. ControlType is the UIA control type name (ControlTypeName). RuntimeID is
// what control_action takes. Rect is in physical screen pixels.
type FocusedControl struct {
	Window       uint64 `json:"window"`
	Name         string `json:"name"`
	ControlType  string `json:"control_type"`
	AutomationID string `json:"automation_id,omitempty"`
	ClassName    string `json:"class_name,omitempty"`
	RuntimeID    string `json:"runtime_id,omitempty"`
	Rect         Rect   `json:"rect"`
}

// LaunchArgs starts Path with Args in Cwd as a detached process: not in a job object, so it outlives the request.
// Admin starts it elevated through the admin worker.
type LaunchArgs struct {
	Path  string   `json:"path"`
	Args  []string `json:"args,omitempty"`
	Cwd   string   `json:"cwd,omitempty"`
	Admin bool     `json:"admin,omitempty"`
}

type LaunchResult struct {
	PID uint32 `json:"pid"`
}

// HScrollArgs scrolls the horizontal wheel at screen point (X, Y) by Delta notches, positive to the right.
type HScrollArgs struct {
	X     int `json:"x"`
	Y     int `json:"y"`
	Delta int `json:"delta"`
}

type PointArgs struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// HandleResult: the top-level window that receives a click at the point; 0 when the point is off screen or no
// window is there. Class, PID and Process describe it; older agents leave them empty.
type HandleResult struct {
	Handle  uint64 `json:"handle"`
	Class   string `json:"class,omitempty"`
	PID     uint32 `json:"pid,omitempty"`
	Process string `json:"process,omitempty"`
}

// ListWindowsArgs: Focused asks for WindowsResult.Focused, which costs a UI Automation helper process (up to 2 s);
// callers that only check windows leave it false.
type ListWindowsArgs struct {
	Focused bool `json:"focused,omitempty"`
}

// WindowsResult lists the windows from the top of the Z order down. Foreground is the foreground window's handle (0
// if none); Focused describes the focused control (nil when UIA cannot report one); Session is the agent's session
// state.
type WindowsResult struct {
	Windows        []WindowInfo        `json:"windows"`
	Foreground     uint64              `json:"foreground"`
	Focused        *FocusedControl     `json:"focused,omitempty"`
	Session        *SessionStateResult `json:"session,omitempty"`
	AgentIntegrity string              `json:"agent_integrity,omitempty"` // the agent's own integrity level, same values as WindowInfo.Integrity
}

type TextResult struct {
	Text string `json:"text"`
}

// WaitArgs.Kind: "process_exit" / "process_running" (Name, e.g. "notepad") or "file_exists" (Path). TimeoutMs 0 means 60 s.
type WaitArgs struct {
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	Path      string `json:"path,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

type WaitResult struct {
	Satisfied bool `json:"satisfied"`
}

// WriteFrame writes a frame whose payload is in memory.
func WriteFrame(w io.Writer, header any, payload []byte) error {
	return WriteFrameFrom(w, header, int64(len(payload)), bytes.NewReader(payload))
}

// WriteFrameFrom writes a frame whose payload is the next size bytes of src (streamed, not buffered).
func WriteFrameFrom(w io.Writer, header any, size int64, src io.Reader) error {
	h, err := json.Marshal(header)
	if err != nil {
		return err
	}
	buf := make([]byte, 12, 12+len(h))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(h)))
	binary.BigEndian.PutUint64(buf[4:12], uint64(size))
	if _, err := w.Write(append(buf, h...)); err != nil {
		return err
	}
	if size > 0 {
		if _, err := io.CopyN(w, src, size); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads a frame with its payload in memory.
func ReadFrame(r io.Reader, header any) ([]byte, error) {
	size, err := ReadHeader(r, header)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, size)
	_, err = io.ReadFull(r, payload)
	return payload, err
}

// ReadHeader reads a frame's header and returns its payload size; the caller must then consume exactly that many
// bytes from r (e.g. io.CopyN to a file) before reading the next frame.
func ReadHeader(r io.Reader, header any) (int64, error) {
	var prefix [12]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return 0, err
	}
	h := make([]byte, binary.BigEndian.Uint32(prefix[0:4]))
	if _, err := io.ReadFull(r, h); err != nil {
		return 0, err
	}
	size := int64(binary.BigEndian.Uint64(prefix[4:12]))
	if err := json.Unmarshal(h, header); err != nil {
		// Skip the payload so the stream stays in sync.
		io.CopyN(io.Discard, r, size)
		return 0, err
	}
	return size, nil
}

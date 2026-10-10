package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"hyperhand/internal/hyperv"
)

type Client struct{}

var pipeServerPID = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeServerProcessId")

func connect(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := winio.DialPipeAccessImpLevel(ctx, PipePath, windows.GENERIC_READ|windows.FILE_WRITE_DATA, winio.PipeImpLevelIdentification)
	if err != nil {
		return nil, fmt.Errorf("connect HyperHand service (installation or repair may be required): %w", err)
	}
	if err := verifyServer(c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func verifyServer(c net.Conn) error {
	h, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("broker pipe handle unavailable")
	}
	var pid uint32
	if result, _, err := pipeServerPID.Call(h.Fd(), uintptr(unsafe.Pointer(&pid))); result == 0 {
		return fmt.Errorf("query broker pipe identity: %w", err)
	}
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(m)
	n, _ := windows.UTF16PtrFromString(ServiceName)
	s, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(s)
	var status windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(s, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed); err != nil {
		return err
	}
	if status.CurrentState != windows.SERVICE_RUNNING || pid == 0 || status.ProcessId != pid {
		return errors.New("named pipe server does not match the running HyperHand service")
	}
	return nil
}

func rpc(r request, src io.Reader, size int64) (response, []byte, error) {
	var out response
	if err := validateRequest(r, size); err != nil {
		return out, nil, err
	}
	c, err := connect(context.Background())
	if err != nil {
		return out, nil, err
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(operationTimeout(r.Op))); err != nil {
		return out, nil, err
	}
	if err := writeFrame(c, r, size, src); err != nil {
		return out, nil, err
	}
	n, err := readHeader(c, &out, maxScreenshot)
	if err != nil {
		return out, nil, err
	}
	if out.Error != "" {
		return out, nil, errors.New(out.Error)
	}
	if r.Op != "screenshot" && n != 0 {
		return out, nil, errors.New("unexpected broker response payload")
	}
	b := make([]byte, n)
	_, err = io.ReadFull(c, b)
	return out, b, err
}

func call(r request) error { _, _, err := rpc(r, nil, 0); return err }
func decode(r request, dst any) error {
	out, _, err := rpc(r, nil, 0)
	if err != nil {
		return err
	}
	return json.Unmarshal(out.Result, dst)
}

func (Client) ListVMs() ([]hyperv.VM, error) {
	var v []hyperv.VM
	err := decode(request{Op: "list"}, &v)
	return v, err
}
func (Client) Find(vm string) (hyperv.VM, error) {
	var v hyperv.VM
	err := decode(request{Op: "find", VM: vm}, &v)
	return v, err
}
func (Client) Start(vm string) error { return call(request{Op: "start", VM: vm}) }
func (Client) Stop(vm string) error  { return call(request{Op: "stop", VM: vm}) }
func (Client) Save(vm string) error  { return call(request{Op: "save", VM: vm}) }
func (Client) Pause(vm string) error { return call(request{Op: "pause", VM: vm}) }
func (Client) Shutdown(vm string) error {
	return call(request{Op: "shutdown", VM: vm})
}
func (Client) ListCheckpoints(vm string) (hyperv.CheckpointList, error) {
	var v hyperv.CheckpointList
	err := decode(request{Op: "checkpoints", VM: vm}, &v)
	return v, err
}
func (Client) CreateCheckpoint(vm, name string) (hyperv.Checkpoint, error) {
	var v hyperv.Checkpoint
	err := decode(request{Op: "checkpoint_create", VM: vm, Name: name}, &v)
	return v, err
}
func (Client) DeleteCheckpoint(vm, id string, subtree bool) error {
	return call(request{Op: "checkpoint_delete", VM: vm, ID: id, Subtree: subtree})
}
func (Client) RestoreCheckpoint(vm, id string) error {
	return call(request{Op: "checkpoint_restore", VM: vm, ID: id})
}
func (Client) RenameCheckpoint(vm, id, name string) error {
	return call(request{Op: "checkpoint_rename", VM: vm, ID: id, Name: name})
}
func (Client) Screenshot(vm string) ([]byte, int, int, error) {
	out, b, err := rpc(request{Op: "screenshot", VM: vm}, nil, 0)
	return b, out.Width, out.Height, err
}
func (Client) Click(vm string, x, y, button, count int, modifiers []string) error {
	return call(request{Op: "click", VM: vm, X: x, Y: y, Button: button, Count: count, Modifiers: modifiers})
}
func (Client) Drag(vm string, x1, y1, x2, y2 int, modifiers []string) error {
	return call(request{Op: "drag", VM: vm, X: x1, Y: y1, X2: x2, Y2: y2, Modifiers: modifiers})
}
func (Client) Scroll(vm string, x, y, delta int) error {
	return call(request{Op: "scroll", VM: vm, X: x, Y: y, Delta: delta})
}
func (Client) PressKeys(vm, text string) error { return call(request{Op: "keys", VM: vm, Text: text}) }
func (Client) TypeText(vm, text string) error  { return call(request{Op: "text", VM: vm, Text: text}) }
func (Client) CopyToGuest(vm, hostPath, guestPath string) error {
	f, err := os.Open(hostPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("copy source must be a regular file")
	}
	_, _, err = rpc(request{Op: "copy", VM: vm, GuestPath: guestPath}, f, st.Size())
	return err
}

// Dial opens a stream to the fixed HyperHand guest-agent service only.
func (Client) Dial(ctx context.Context, vmID string) (net.Conn, error) {
	c, err := connect(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	fail := func(err error) (net.Conn, error) { c.Close(); return nil, err }
	if err := c.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return fail(err)
	}
	if err := writeFrame(c, request{Op: "dial", VM: vmID}, 0, nil); err != nil {
		return fail(err)
	}
	var out response
	n, err := readHeader(c, &out, 0)
	if err != nil {
		return fail(err)
	}
	if n != 0 || out.Error != "" {
		return fail(fmt.Errorf("broker guest connection: %s", out.Error))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return c, nil
}

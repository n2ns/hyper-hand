package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"hyperhand/internal/hvsock"
	"hyperhand/internal/hyperv"
)

func configDir() string {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return filepath.Join(os.Getenv("ProgramData"), "HyperHand")
	}
	return filepath.Join(dir, "HyperHand")
}
func DataDir() string    { return filepath.Join(configDir(), "service-data") }
func ConfigPath() string { return filepath.Join(configDir(), "config.json") }

// Run runs the installed service; installing it is a separate elevated operation.
func Run() error { return svc.Run(ServiceName, &service{}) }

type service struct{}

func (service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	sid, err := serviceIdentity()
	if err != nil {
		logServiceError("identity", err)
		return true, 1
	}
	config, err := loadConfig()
	if err != nil {
		logServiceError("configuration", err)
		return true, 2
	}
	security, err := pipeSecurity(config.OwnerSID, sid)
	if err != nil {
		logServiceError("pipe security", err)
		return true, 3
	}
	// The installer provisions this directory without granting the desktop user write access.
	st, err := os.Stat(DataDir())
	if err != nil || !st.IsDir() {
		if err == nil {
			err = errors.New("service-data is not a directory")
		}
		logServiceError("data directory", err)
		return true, 4
	}
	l, err := winio.ListenPipe(PipePath, &winio.PipeConfig{SecurityDescriptor: security, InputBufferSize: 65536, OutputBufferSize: 65536})
	if err != nil {
		logServiceError("pipe listener", err)
		return true, 5
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{ctx: ctx, listener: l, conns: make(map[net.Conn]bool)}
	done := make(chan error, 1)
	go func() { done <- s.serve() }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			logServiceError("pipe accept", err)
			cancel()
			s.close()
			return true, 6
		case r := <-req:
			switch r.Cmd {
			case svc.Interrogate:
				status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
			case svc.Stop, svc.Shutdown:
				cancel()
				s.close()
				checkpoint := uint32(1)
				status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 15000}
				// serve stops adding workers before we wait for outstanding operations.
				<-done
				finished := make(chan struct{})
				go func() { s.workers.Wait(); close(finished) }()
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-finished:
						return false, 0
					case <-ticker.C:
						checkpoint++
						status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 15000}
					}
				}
			}
		}
	}
}

func loadConfig() (Config, error) {
	var c Config
	f, err := os.Open(ConfigPath())
	if err != nil {
		return c, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxHeader+1))
	if err != nil {
		return c, err
	}
	if len(b) > maxHeader {
		return c, errors.New("broker configuration exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("trailing broker configuration data")
	}
	return c, nil
}

func pipeSecurity(owner, serviceSID string) (string, error) {
	u, err := windows.StringToSid(owner)
	if err != nil {
		return "", fmt.Errorf("invalid installation owner SID: %w", err)
	}
	s, err := windows.StringToSid(serviceSID)
	if err != nil {
		return "", err
	}
	// 0x12019b excludes FILE_CREATE_PIPE_INSTANCE (0x4) from client read/write rights.
	// Explicit Medium integrity permits the ordinary desktop client to write to the pipe.
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + s.String() + ")(A;;0x12019b;;;" + u.String() + ")S:(ML;;NW;;;ME)", nil
}

func serviceIdentity() (string, error) {
	expected, _, _, err := windows.LookupSID("", `NT SERVICE\`+ServiceName)
	if err != nil {
		return "", err
	}
	t := windows.GetCurrentProcessToken()
	u, err := t.GetTokenUser()
	if err != nil {
		return "", err
	}
	if u.User.Sid.String() != expected.String() {
		return "", errors.New("service must use its dedicated virtual account")
	}
	gs, err := t.GetTokenGroups()
	if err != nil {
		return "", err
	}
	found := false
	for _, g := range gs.AllGroups() {
		enabled := g.Attributes&windows.SE_GROUP_ENABLED != 0 && g.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0
		if g.Sid.String() == "S-1-5-32-544" && enabled {
			return "", errors.New("service must not be a local administrator")
		}
		if g.Sid.String() == "S-1-5-32-578" && enabled {
			found = true
		}
	}
	if !found {
		return "", errors.New("service requires Hyper-V Administrators membership")
	}
	return expected.String(), nil
}

type server struct {
	ctx      context.Context
	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]bool
	closing  bool
	workers  sync.WaitGroup
}

func (s *server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		c.Close()
		return false
	}
	s.conns[c] = true
	return true
}
func (s *server) release(c net.Conn) { c.Close(); s.mu.Lock(); delete(s.conns, c); s.mu.Unlock() }
func (s *server) close() {
	s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	for c := range s.conns {
		c.Close()
	}
}

func (s *server) serve() error {
	slots := make(chan struct{}, 32)
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return err
		}
		if !s.track(c) {
			return context.Canceled
		}
		select {
		case slots <- struct{}{}:
			s.workers.Add(1)
			go func() { defer s.workers.Done(); defer func() { <-slots }(); defer s.release(c); s.handle(c) }()
		default:
			s.release(c)
		}
	}
}

func (s *server) handle(c net.Conn) {
	if c.SetDeadline(time.Now().Add(15*time.Second)) != nil {
		return
	}
	var r request
	size, err := readHeader(c, &r, maxCopy)
	if err != nil {
		return
	}
	if err = validateRequest(r, size); err != nil {
		_ = writeFrame(c, response{Error: err.Error()}, 0, nil)
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	if c.SetDeadline(time.Now().Add(operationTimeout(r.Op))) != nil {
		return
	}
	if r.Op == "dial" {
		s.tunnel(c, r.VM)
		return
	}
	var value any
	var payload []byte
	var out response
	switch r.Op {
	case "list":
		value, err = hyperv.ListVMs()
	case "find":
		value, err = hyperv.Find(r.VM)
	case "start":
		err = hyperv.Start(r.VM)
	case "stop":
		err = hyperv.Stop(r.VM)
	case "save":
		err = hyperv.Save(r.VM)
	case "pause":
		err = hyperv.Pause(r.VM)
	case "shutdown":
		err = hyperv.Shutdown(r.VM)
	case "checkpoints":
		value, err = hyperv.ListCheckpoints(r.VM)
	case "checkpoint_create":
		value, err = hyperv.CreateCheckpoint(r.VM, r.Name)
	case "checkpoint_restore":
		err = hyperv.RestoreCheckpoint(r.VM, r.ID)
	case "checkpoint_delete":
		err = hyperv.DeleteCheckpoint(r.VM, r.ID, r.Subtree)
	case "checkpoint_rename":
		err = hyperv.RenameCheckpoint(r.VM, r.ID, r.Name)
	case "screenshot":
		payload, out.Width, out.Height, err = screenshotReady(s.ctx, r.VM)
	case "click":
		err = hyperv.Click(r.VM, r.X, r.Y, r.Button, r.Count, r.Modifiers)
	case "drag":
		err = hyperv.Drag(r.VM, r.X, r.Y, r.X2, r.Y2, r.Modifiers)
	case "scroll":
		err = hyperv.Scroll(r.VM, r.X, r.Y, r.Delta)
	case "keys":
		err = hyperv.PressKeys(r.VM, r.Text)
	case "text":
		err = hyperv.TypeText(r.VM, r.Text)
	case "copy":
		err = s.copyGuest(c, r, size)
	}
	if err == nil && len(payload) > maxScreenshot {
		err = errors.New("screenshot exceeds broker payload limit")
	}
	if err == nil && value != nil {
		out.Result, err = json.Marshal(value)
	}
	if err != nil {
		out.Error = err.Error()
		payload = nil
		out.Result = nil
	}
	_ = writeFrame(c, out, int64(len(payload)), bytes.NewReader(payload))
}

var serviceLogMu sync.Mutex

// Logs contain lifecycle failures only, never RPC arguments or transferred data.
func logServiceError(stage string, err error) {
	message := fmt.Sprintf("%s: %v", stage, err)
	if event, openErr := eventlog.Open(ServiceName); openErr == nil {
		_ = event.Error(1, message)
		_ = event.Close()
	}
	serviceLogMu.Lock()
	defer serviceLogMu.Unlock()
	path := filepath.Join(DataDir(), "broker.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, statErr := os.Stat(path); statErr == nil && st.Size() > 1<<20 {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, openErr := os.OpenFile(path, flags, 0600)
	if openErr != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), message)
}

// A VM can report Running before its video head is ready. Only this read operation is retried.
func screenshotReady(ctx context.Context, vm string) ([]byte, int, int, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		b, w, h, err := hyperv.Screenshot(vm)
		if err == nil {
			return b, w, h, nil
		}
		last = err
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return nil, 0, 0, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return nil, 0, 0, last
}

func (s *server) copyGuest(src io.Reader, r request, size int64) error {
	f, err := os.CreateTemp(DataDir(), "copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, copyErr := io.CopyN(f, src, size)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return hyperv.CopyToGuest(r.VM, f.Name(), r.GuestPath)
}

func (s *server) tunnel(c net.Conn, vmID string) {
	requested, err := guid.FromString(vmID)
	if err != nil {
		_ = writeFrame(c, response{Error: "invalid VM ID"}, 0, nil)
		return
	}
	// Reject special host/parent/wildcard IDs: this endpoint connects to actual VMs only.
	vms, err := hyperv.ListVMs()
	if err != nil {
		_ = writeFrame(c, response{Error: err.Error()}, 0, nil)
		return
	}
	found := false
	for _, vm := range vms {
		id, parseErr := guid.FromString(vm.ID)
		if parseErr == nil && id == requested {
			found = true
			break
		}
	}
	if !found {
		_ = writeFrame(c, response{Error: "VM ID not found"}, 0, nil)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	guest, err := hvsock.Dial(ctx, vmID)
	cancel()
	if err != nil {
		_ = writeFrame(c, response{Error: err.Error()}, 0, nil)
		return
	}
	if !s.track(guest) {
		return
	}
	defer s.release(guest)
	if err := writeFrame(c, response{}, 0, nil); err != nil {
		return
	}
	if c.SetDeadline(time.Time{}) != nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(guest, c); guest.Close(); c.Close(); close(done) }()
	_, _ = io.Copy(c, guest)
	guest.Close()
	c.Close()
	<-done
}

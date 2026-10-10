package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

type adminEndpoint struct {
	Pipe          string
	ParentPID     uint32
	ParentCreated windows.Filetime
}

// adminRequest is what the elevated worker receives: a command to run, or with Launch set, a program to start detached.
type adminRequest struct {
	Args     proto.ExecArgs
	Launch   *proto.LaunchArgs
	Deadline time.Time
}

// errElevationPending: the deadline passed before the elevated worker received the request.
var errElevationPending = errors.New("timed out before the elevated worker received the request")

type adminReply struct {
	Result proto.ExecResult
	PID    uint32
	Error  string
}

// execAdmin runs a command elevated. A timeout returns TimedOut, with ElevationPending when the command never reached
// the worker.
func execAdmin(ctx context.Context, a proto.ExecArgs) (any, []byte, error) {
	return execAdminWithLauncher(ctx, a, launchAdmin)
}

func execAdminWithLauncher(ctx context.Context, a proto.ExecArgs, launch func(context.Context, adminEndpoint) error) (any, []byte, error) {
	opctx, cancel := context.WithTimeout(ctx, timeout(a.TimeoutMs))
	defer cancel()
	if a.Shell != "" && a.Shell != "powershell" && a.Shell != "cmd" {
		return nil, nil, fmt.Errorf("unknown shell %q", a.Shell)
	}
	if err := opctx.Err(); err != nil {
		return adminInterrupted(ctx, err)
	}
	if a.Cwd == "" {
		var err error
		a.Cwd, err = os.Getwd()
		if err != nil {
			return nil, nil, err
		}
	}
	a.Admin = false
	reply, err := adminCall(ctx, opctx, adminRequest{Args: a}, launch)
	if err != nil {
		if ctx.Err() == nil && opctx.Err() != nil {
			return proto.ExecResult{ExitCode: -1, TimedOut: true, ElevationPending: errors.Is(err, errElevationPending)}, nil, nil
		}
		return adminInterrupted(ctx, err)
	}
	return reply.Result, nil, nil
}

// adminLaunchTimeout bounds launch with Admin: elevation plus the start of the program.
const adminLaunchTimeout = 60 * time.Second

func launchAdminProcess(ctx context.Context, a proto.LaunchArgs) (any, []byte, error) {
	return launchAdminWithLauncher(ctx, a, launchAdmin)
}

func launchAdminWithLauncher(ctx context.Context, a proto.LaunchArgs, launch func(context.Context, adminEndpoint) error) (any, []byte, error) {
	opctx, cancel := context.WithTimeout(ctx, adminLaunchTimeout)
	defer cancel()
	if a.Cwd == "" {
		var err error
		a.Cwd, err = os.Getwd()
		if err != nil {
			return nil, nil, err
		}
	}
	a.Admin = false
	reply, err := adminCall(ctx, opctx, adminRequest{Launch: &a}, launch)
	if err != nil {
		if ctx.Err() == nil && opctx.Err() != nil {
			return nil, nil, fmt.Errorf("elevated launch not completed within %s (UAC prompt unanswered?)", adminLaunchTimeout)
		}
		return adminInterrupted(ctx, err)
	}
	return proto.LaunchResult{PID: reply.PID}, nil, nil
}

// adminCall sends one request to a freshly elevated worker and returns its reply, or errElevationPending when opctx's
// deadline passes before the worker received the request (so it did not run). The launcher runs only
// ShellExecuteEx, in a disposable ordinary process. The elevated worker receives the request only over this live,
// single-use pipe. opctx bounds the whole exchange; its deadline is the one the worker enforces.
func adminCall(ctx, opctx context.Context, req adminRequest, launch func(context.Context, adminEndpoint) error) (adminReply, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return adminReply{}, err
	}
	endpoint := adminEndpoint{Pipe: `\\.\pipe\hyperhand-admin-` + rand.Text(), ParentPID: uint32(os.Getpid())}
	var exited, kernel, userTime windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &endpoint.ParentCreated, &exited, &kernel, &userTime); err != nil {
		return adminReply{}, err
	}
	listener, err := winio.ListenPipe(endpoint.Pipe, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + user.User.Sid.String() + ")(A;;GA;;;BA)(A;;GA;;;SY)",
	})
	if err != nil {
		return adminReply{}, err
	}
	var conn net.Conn
	var acceptErr error
	accepted := make(chan struct{})
	go func() { conn, acceptErr = listener.Accept(); close(accepted) }()
	stopAccept := context.AfterFunc(opctx, func() { listener.Close() })
	defer func() {
		stopAccept()
		listener.Close()
		<-accepted
		if conn != nil {
			conn.Close()
		}
	}()
	launchCtx, stopLaunch := context.WithCancel(opctx)
	var launchErr error
	launched := make(chan struct{})
	go func() { launchErr = launch(launchCtx, endpoint); close(launched) }()
	defer func() { stopLaunch(); <-launched }()
	launchDone := launched
	// notSent is the error while the request has not reached the worker: an expired deadline means elevation pending.
	notSent := func() error {
		if ctx.Err() == nil && errors.Is(opctx.Err(), context.DeadlineExceeded) {
			return errElevationPending
		}
		return opctx.Err()
	}
	for {
		select {
		case <-opctx.Done():
			return adminReply{}, notSent()
		case <-launchDone:
			if opctx.Err() != nil {
				return adminReply{}, notSent()
			}
			if launchErr != nil {
				return adminReply{}, launchErr
			}
			launchDone = nil // runas succeeded; the worker may still be connecting
		case <-accepted:
			if opctx.Err() != nil {
				return adminReply{}, notSent()
			}
			if acceptErr != nil {
				return adminReply{}, acceptErr
			}
			deadline, _ := opctx.Deadline()
			// The worker enforces the original deadline. Allow it to reap its job
			// and return captured output; caller cancellation closes the pipe immediately.
			conn.SetDeadline(deadline.Add(5 * time.Second))
			stopIO := context.AfterFunc(ctx, func() { conn.Close() })
			defer stopIO()
			req.Deadline = deadline
			if err := json.NewEncoder(conn).Encode(req); err != nil {
				return adminReply{}, err
			}
			var reply adminReply
			if err := json.NewDecoder(conn).Decode(&reply); err != nil {
				if opctx.Err() != nil {
					return adminReply{}, opctx.Err()
				}
				return adminReply{}, err
			}
			if ctx.Err() != nil {
				return adminReply{}, ctx.Err()
			}
			if reply.Error != "" {
				return adminReply{}, errors.New(reply.Error)
			}
			return reply, nil
		}
	}
}

func adminInterrupted(ctx context.Context, err error) (any, []byte, error) {
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return proto.ExecResult{ExitCode: -1, TimedOut: true}, nil, nil
	}
	return nil, nil, err
}

// runAdminWorker executes one request: a command in a job object, or a detached launch. Checking both PID and
// creation time stops a late UAC approval from trusting a replacement pipe server after its parent exits.
func runAdminWorker(endpoint adminEndpoint) error {
	parent, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, endpoint.ParentPID)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	var created, exited, kernel, userTime windows.Filetime
	if err := windows.GetProcessTimes(parent, &created, &exited, &kernel, &userTime); err != nil {
		return err
	}
	state, err := windows.WaitForSingleObject(parent, 0)
	if err != nil || state != uint32(windows.WAIT_TIMEOUT) || created != endpoint.ParentCreated {
		return errors.New("admin request owner is no longer running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, endpoint.Pipe)
	if err != nil {
		return err
	}
	defer conn.Close()
	var serverPID uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(conn.(interface{ Fd() uintptr }).Fd()), &serverPID); err != nil {
		return err
	}
	if serverPID != endpoint.ParentPID {
		return errors.New("admin pipe server is not the request owner")
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var request adminRequest
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Time{})
	execCtx, stopExec := context.WithDeadline(context.Background(), request.Deadline)
	defer stopExec()
	watchDone := make(chan struct{})
	go func() {
		var b [1]byte
		conn.Read(b[:]) // EOF (or any further input) revokes the request
		stopExec()
		close(watchDone)
	}()
	defer func() { conn.Close(); <-watchDone }()
	var reply adminReply
	if request.Launch != nil {
		pid, err := startDetached(*request.Launch)
		if err != nil {
			reply.Error = err.Error()
		}
		reply.PID = pid
	} else {
		request.Args.Admin = false
		result, _, execErr := execAdminCommand(execCtx, request.Args)
		if execErr != nil {
			reply.Error = execErr.Error()
		} else {
			reply.Result = result.(proto.ExecResult)
		}
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return json.NewEncoder(conn).Encode(reply)
}

func execAdminCommand(ctx context.Context, a proto.ExecArgs) (any, []byte, error) {
	pattern := "hh-admin-*.ps1"
	// -File needs a BOM for UTF-8 and explicit propagation of the last statement's failure.
	script := "\xEF\xBB\xBF[Console]::OutputEncoding=[Text.Encoding]::UTF8\r\n" + a.Command + "\r\nif (-not $?) { exit 1 }\r\n"
	if a.Shell == "cmd" {
		pattern = "hh-admin-*.cmd"
		// Change the code page before cmd parses the user's command. A same-line
		// chcp prefix is too late. Escape batch expansion for the nested cmd.
		script = "@echo off\r\nchcp 65001 >nul\r\ncmd.exe /d /s /c \"" + strings.ReplaceAll(a.Command, "%", "%%") + "\"\r\n"
	} else if a.Shell != "" && a.Shell != "powershell" {
		return execCommand(ctx, a, "")
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(script)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, nil, err
	}
	if a.Shell == "cmd" {
		a.Command = `"` + f.Name() + `"`
		return execCommand(ctx, a, "")
	}
	return execCommand(ctx, a, f.Name())
}

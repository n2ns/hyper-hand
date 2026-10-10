package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

func timeout(ms int) time.Duration {
	if ms <= 0 {
		return 60 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

func execOp(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.ExecArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	if a.Admin {
		return execAdmin(ctx, a)
	}
	return execCommand(ctx, a, "")
}

// script is used by the elevated worker to preserve -File support for long scripts.
func execCommand(ctx context.Context, a proto.ExecArgs, script string) (any, []byte, error) {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return proto.ExecResult{ExitCode: -1, TimedOut: true}, nil, nil
		}
		return nil, nil, err
	}
	cmd, err := shellCommand(a, script)
	if err != nil {
		return nil, nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create command job: %w", err)
	}
	// Closing a job without KILL_ON_JOB_CLOSE preserves normally launched background apps.
	defer windows.CloseHandle(job)
	if err := startInJob(cmd, job); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var r proto.ExecResult
	t := time.NewTimer(timeout(a.TimeoutMs))
	defer t.Stop()
	completed := false
	select {
	case <-done:
		completed = true
	case <-t.C:
		r.TimedOut = true
	case <-ctx.Done():
		r.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	ctxErr := ctx.Err()
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		r.TimedOut = true
	}
	if r.TimedOut || ctxErr != nil {
		if err := windows.TerminateJobObject(job, 1); err != nil {
			cmd.Process.Kill()
			if !completed {
				<-done
			}
			return nil, nil, fmt.Errorf("terminate command job: %w", err)
		}
		if !completed {
			<-done
		}
	}
	if !r.TimedOut && ctxErr != nil {
		return nil, nil, ctxErr
	}
	r.ExitCode = cmd.ProcessState.ExitCode()
	r.Stdout, r.Stderr = toUTF8(stdout.Bytes()), toUTF8(stderr.Bytes())
	return r, nil, nil
}

// shellCommand builds the hidden shell process for a's command (or, for powershell, the script file).
func shellCommand(a proto.ExecArgs, script string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	attr := &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	switch a.Shell {
	case "", "powershell":
		if script != "" {
			cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		} else {
			cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
				"-Command", "[Console]::OutputEncoding=[Text.Encoding]::UTF8;"+a.Command)
		}
	case "cmd":
		cmd = exec.Command("cmd.exe")
		attr.CmdLine = `cmd.exe /d /s /c "` + a.Command + `"`
	default:
		return nil, fmt.Errorf("unknown shell %q", a.Shell)
	}
	cmd.SysProcAttr = attr
	cmd.Dir = a.Cwd
	return cmd, nil
}

// toUTF8 keeps valid UTF-8 as is and otherwise decodes from the OEM code page (cmd's output).
const cpOEM = 1 // CP_OEMCP

func toUTF8(b []byte) string {
	b = bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF")) // UTF-8 BOM
	if utf8.Valid(b) {
		return string(b)
	}
	n, _ := windows.MultiByteToWideChar(cpOEM, 0, &b[0], int32(len(b)), nil, 0)
	if n <= 0 {
		return string(b)
	}
	u := make([]uint16, n)
	windows.MultiByteToWideChar(cpOEM, 0, &b[0], int32(len(b)), &u[0], n)
	return string(utf16.Decode(u))
}

// writeFileStream writes the next size bytes of r (a write_file payload) to the file named in args, via
// a unique temporary file beside the destination. opErr is the request's error; the payload is always consumed,
// so connErr is set only when reading r fails (the stream is then broken).
func writeFileStream(r io.Reader, args json.RawMessage, size int64) (opErr, connErr error) {
	er := &errReader{r: r}
	payload := &io.LimitedReader{R: er, N: size}
	opErr = func() error {
		var a proto.PathArgs
		if err := decode(args, &a); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
			return err
		}
		f, err := os.CreateTemp(filepath.Dir(a.Path), ".hyperhand-*.hhpart")
		if err != nil {
			return err
		}
		part := f.Name()
		// struct{io.Writer} hides os.File.ReadFrom so the 1 MB buffer is used.
		n, err := io.CopyBuffer(struct{ io.Writer }{f}, payload, make([]byte, 1<<20))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n < size {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			err = os.Rename(part, a.Path)
		}
		if err != nil {
			os.Remove(part)
		}
		return err
	}()
	if er.err != nil {
		return opErr, er.err
	}
	if payload.N > 0 {
		if _, err := io.CopyN(io.Discard, payload, payload.N); err != nil {
			return opErr, err
		}
	}
	return opErr, nil
}

// errReader records the first read error.
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

// fileStream is read_file's result: Serve streams the open file as the response payload and closes it.
type fileStream struct {
	f    *os.File
	size int64
}

func readFile(_ context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.PathArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(a.Path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err == nil && st.IsDir() {
		err = fmt.Errorf("%s is a directory", a.Path)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return fileStream{f, st.Size()}, nil, nil
}

func listDir(_ context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.PathArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	ents, err := os.ReadDir(a.Path)
	if err != nil {
		return nil, nil, err
	}
	r := proto.ListDirResult{Entries: []proto.DirEntry{}}
	for _, e := range ents {
		if e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			continue // symlinks and junctions (e.g. "Application Data")
		}
		r.Entries = append(r.Entries, proto.DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return r, nil, nil
}

func waitOp(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.WaitArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	var check func() bool
	switch a.Kind {
	case "process_running", "process_exit":
		if a.Name == "" {
			return nil, nil, fmt.Errorf("wait %s: empty name", a.Kind)
		}
		a.Name = filepath.Base(a.Name)
	case "file_exists":
		if a.Path == "" {
			return nil, nil, fmt.Errorf("wait file_exists: empty path")
		}
	}
	switch a.Kind {
	case "process_running":
		check = func() bool { return processRunning(a.Name) }
	case "process_exit":
		check = func() bool { return !processRunning(a.Name) }
	case "file_exists":
		check = func() bool { _, err := os.Stat(a.Path); return err == nil }
	default:
		return nil, nil, fmt.Errorf("unknown wait kind %q", a.Kind)
	}
	tctx, cancel := context.WithTimeout(ctx, timeout(a.TimeoutMs))
	defer cancel()
	for {
		if check() {
			return proto.WaitResult{Satisfied: true}, nil, nil
		}
		select {
		case <-tctx.Done():
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return proto.WaitResult{}, nil, nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func processRunning(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".exe") + ".exe"
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if strings.ToLower(windows.UTF16ToString(e.ExeFile[:])) == name {
			return true
		}
	}
	return false
}

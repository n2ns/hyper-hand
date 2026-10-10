package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

func TestAdminPendingElevation(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var late adminEndpoint
			start := time.Now()
			result, _, err := execAdminWithLauncher(ctx, proto.ExecArgs{Command: "exit 0", TimeoutMs: 100},
				func(ctx context.Context, endpoint adminEndpoint) error {
					late = endpoint
					if mode == "cancel" {
						cancel()
					}
					<-ctx.Done() // simulate an unanswered UAC prompt
					return ctx.Err()
				})
			if time.Since(start) > time.Second {
				t.Fatal("pending elevation blocked the agent")
			}
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			} else if err != nil || !result.(proto.ExecResult).TimedOut || !elevationPending(t, result) {
				t.Fatalf("timeout: result=%+v err=%v", result, err)
			}
			// A late approval gets no command: the single-use listener is gone.
			if err := runAdminWorker(late); err == nil {
				t.Fatal("late worker accepted an expired request")
			}
		})
	}
}

func TestAdminLauncherFailure(t *testing.T) {
	denied := errors.New("UAC denied")
	_, _, err := execAdminWithLauncher(context.Background(), proto.ExecArgs{Command: "exit 0"},
		func(context.Context, adminEndpoint) error { return denied })
	if !errors.Is(err, denied) {
		t.Fatalf("launcher error: %v", err)
	}
}

func TestAdminEarlyLauncherFailureClosesPipe(t *testing.T) {
	// The launcher can reject the request before Accept has started connecting.
	// go-winio v0.6.2 could lose the listener close signal in this race and leave
	// execAdminWithLauncher blocked in cleanup even after its deadline expired.
	denied := errors.New("launcher rejected request")
	for i := 0; i < 500; i++ {
		done := make(chan error, 1)
		go func() {
			_, _, err := execAdminWithLauncher(context.Background(), proto.ExecArgs{Command: "exit 0", TimeoutMs: 1000},
				func(context.Context, adminEndpoint) error { return denied })
			done <- err
		}()
		timer := time.NewTimer(5 * time.Second)
		select {
		case err := <-done:
			timer.Stop()
			if !errors.Is(err, denied) {
				t.Fatalf("iteration %d: expected launcher rejection, got %v", i, err)
			}
		case <-timer.C:
			t.Fatalf("iteration %d: pipe cleanup did not finish after the request deadline", i)
		}
	}
}

func TestAdminWorkerRejectsWrongOwner(t *testing.T) {
	_, _, err := execAdminWithLauncher(context.Background(), proto.ExecArgs{Command: "exit 0", TimeoutMs: 1000},
		func(_ context.Context, endpoint adminEndpoint) error {
			endpoint.ParentCreated.LowDateTime++
			return runAdminWorker(endpoint)
		})
	if err == nil || !strings.Contains(err.Error(), "owner is no longer running") {
		t.Fatalf("wrong owner accepted: %v", err)
	}
}

func TestAdminWorkerRejectsReplacementPipeServer(t *testing.T) {
	const helperPipeEnv = "HYPERHAND_TEST_REPLACEMENT_PIPE"
	if pipe := os.Getenv(helperPipeEnv); pipe != "" {
		listener, err := winio.ListenPipe(pipe, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		// The rejected client can disconnect before Accept completes, causing
		// go-winio to retry Accept. Use parent EOF to end the helper explicitly.
		go func() {
			io.Copy(io.Discard, os.Stdin)
			listener.Close()
		}()
		fmt.Fprintln(os.Stdout, "ready")
		conn, err := listener.Accept()
		if errors.Is(err, winio.ErrPipeListenerClosed) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := io.Copy(io.Discard, conn); err != nil {
			t.Fatal(err)
		}
		return
	}

	// The original owner is alive with the right creation time, but a different
	// process now serves its pipe. This must reach the server-PID check.
	endpoint := adminEndpoint{Pipe: `\\.\pipe\hyperhand-admin-test-` + rand.Text(), ParentPID: uint32(os.Getpid())}
	var exited, kernel, userTime windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &endpoint.ParentCreated, &exited, &kernel, &userTime); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestAdminWorkerRejectsReplacementPipeServer$")
	cmd.Env = append(os.Environ(), helperPipeEnv+"="+endpoint.Pipe)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("replacement pipe server: %v, stderr=%s", err, &stderr)
		}
	}()
	if ready, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(ready) != "ready" {
		t.Fatalf("replacement pipe server not ready: %q, %v", ready, err)
	}
	if err := runAdminWorker(endpoint); err == nil || err.Error() != "admin pipe server is not the request owner" {
		t.Fatalf("replacement pipe server accepted: %v", err)
	}
}

func TestAdminLongPowerShellScript(t *testing.T) {
	command := "#" + strings.Repeat("x", 40000) + "\r\nWrite-Output 'long-script-ok'"
	result, _, err := execAdminWithLauncher(context.Background(), proto.ExecArgs{Command: command}, testAdminLauncher)
	if err != nil {
		t.Fatal(err)
	}
	r := result.(proto.ExecResult)
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "long-script-ok" {
		t.Fatalf("result: %+v", r)
	}
}

func TestAdminCmdUnicode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HYPERHAND_CMD_TEST", "expanded")
	result, _, err := execAdminWithLauncher(context.Background(), proto.ExecArgs{
		Shell: "cmd", Cwd: dir,
		Command: `echo 汉字 😀&echo 错误 😀 1>&2&echo %HYPERHAND_CMD_TEST%&echo 100%&echo 文件 😀>"汉字 😀 100%.txt"&exit /b 7`,
	}, testAdminLauncher)
	if err != nil {
		t.Fatal(err)
	}
	r := result.(proto.ExecResult)
	if r.ExitCode != 7 || r.TimedOut || r.Stdout != "汉字 😀\r\nexpanded\r\n100%\r\n" || strings.TrimSpace(r.Stderr) != "错误 😀" {
		t.Fatalf("result: %+v", r)
	}
	content, err := os.ReadFile(filepath.Join(dir, "汉字 😀 100%.txt"))
	if err != nil || string(content) != "文件 😀\r\n" {
		t.Fatalf("Unicode file: %q, %v", content, err)
	}
}

func testAdminLauncher(_ context.Context, endpoint adminEndpoint) error {
	// Exercise real named-pipe and process/job code without displaying host UAC.
	return runAdminWorker(endpoint)
}

func TestAdminExecResults(t *testing.T) {
	dir := t.TempDir()
	for _, a := range []proto.ExecArgs{
		{Command: "exit 3"},
		{Command: `Get-Item C:\nope-hh`},
		{Command: "cmd /c exit 5"},
		{Command: "Write-Output ok"},
		{Command: "cmd /c exit 0"},
		{Command: `Write-Output 'hello 世界'; [Console]::Error.WriteLine('error'); (Get-Location).Path`, Cwd: dir},
		{Shell: "cmd", Command: "echo 100% & echo error 1>&2 & exit /b 7", Cwd: dir},
	} {
		want, _, err := execOp(context.Background(), mustJSON(a), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := execAdminWithLauncher(context.Background(), a, testAdminLauncher)
		if err != nil {
			t.Fatal(err)
		}
		g, w := got.(proto.ExecResult), want.(proto.ExecResult)
		sameStderr := g.Stderr == w.Stderr
		if strings.HasPrefix(a.Command, "Get-Item") {
			// PowerShell diagnostics include the different -File/-Command source location.
			sameStderr = strings.Contains(g.Stderr, "PathNotFound,Microsoft.PowerShell.Commands.GetItemCommand")
		}
		if g.ExitCode != w.ExitCode || g.Stdout != w.Stdout || g.TimedOut != w.TimedOut || !sameStderr {
			t.Errorf("%q: admin=%+v, normal=%+v", a.Command, g, w)
		}
	}
}

func TestAdminExecutionDeadlineIncludesElevation(t *testing.T) {
	start := time.Now()
	result, _, err := execAdminWithLauncher(context.Background(), proto.ExecArgs{
		Command: "Write-Output started; Start-Sleep 10", TimeoutMs: 1500,
	}, func(ctx context.Context, endpoint adminEndpoint) error {
		time.Sleep(700 * time.Millisecond)
		return testAdminLauncher(ctx, endpoint)
	})
	if err != nil {
		t.Fatal(err)
	}
	r := result.(proto.ExecResult)
	if !r.TimedOut || !strings.Contains(r.Stdout, "started") || elevationPending(t, r) {
		t.Fatalf("result: %+v", r)
	}
	if elapsed := time.Since(start); elapsed > 2100*time.Millisecond {
		t.Fatalf("elevation reset the deadline: %v", elapsed)
	}
}

func TestAdminCancelRunningCommand(t *testing.T) {
	dir := t.TempDir()
	ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "marker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		command := fmt.Sprintf("[IO.File]::WriteAllText('%s','ready'); Start-Sleep 2; [IO.File]::WriteAllText('%s','late')", strings.ReplaceAll(ready, "'", "''"), strings.ReplaceAll(marker, "'", "''"))
		_, _, err := execAdminWithLauncher(ctx, proto.ExecArgs{Command: command}, testAdminLauncher)
		done <- err
	}()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("command did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("running command did not cancel")
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command survived cancellation: %v", err)
	}
}

func TestExpiredExecDoesNotStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r, _, err := execOp(ctx, mustJSON(proto.ExecArgs{Command: fmt.Sprintf("[IO.File]::WriteAllText('%s','late')", strings.ReplaceAll(marker, "'", "''"))}), nil)
	if err != nil || !r.(proto.ExecResult).TimedOut {
		t.Fatalf("expired command: %+v, %v", r, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expired command executed: %v", err)
	}
}

// elevationPending reads elevation_pending from the result as the host receives it.
func elevationPending(t *testing.T, result any) bool {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		ElevationPending bool `json:"elevation_pending"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r.ElevationPending
}

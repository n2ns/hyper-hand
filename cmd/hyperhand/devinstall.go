package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"

	"hyperhand/internal/broker"
)

// devInstallTask is the on-demand task scripts\dev-install-setup.ps1 registers: it runs
// <repo>\build\dev-install\hyperhand.exe install --quiet with the developer's elevated token, so development builds
// install without a UAC prompt.
const devInstallTask = "HyperHand Dev Install"

// Task Scheduler TASK_STATE values that mean a run is pending or in progress.
const (
	taskStateQueued  = 2
	taskStateRunning = 4
)

// devInstallCommand implements hyperhand.exe dev-install [--no-build] [--timeout <seconds>], meant to be run from the
// repository root as go run ./cmd/hyperhand dev-install. Progress goes to stdout, the error to stderr; the exit code
// is 0 only when the build is installed, the service runs and exactly one installed tray process is up.
func devInstallCommand(args []string) int {
	if err := devInstall(args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "dev-install failed:", err)
		return 1
	}
	return 0
}

func devInstall(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dev-install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	noBuild := fs.Bool("no-build", false, "install the files already in build\\dev-install instead of building them")
	timeout := fs.Int("timeout", 180, "seconds to wait for the install task to finish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *timeout <= 0 {
		return fmt.Errorf("usage: hyperhand.exe dev-install [--no-build] [--timeout <seconds>]")
	}
	repo, err := os.Getwd()
	if err != nil {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(repo, "go.mod")); err != nil || !strings.HasPrefix(strings.TrimSpace(string(b)), "module hyperhand") {
		return fmt.Errorf("%s is not the HyperHand repository root; run go run ./cmd/hyperhand dev-install from there", repo)
	}
	candidateDir := filepath.Join(repo, "build", "dev-install")
	candidate := filepath.Join(candidateDir, "hyperhand.exe")
	paths, err := installPaths()
	if err != nil {
		return err
	}

	runtime.LockOSThread() // Task Scheduler COM stays on this thread
	defer runtime.UnlockOSThread()
	ts, err := connectTaskScheduler()
	if err != nil {
		return fmt.Errorf("connect to Task Scheduler: %w", err)
	}
	defer ts.close()
	task, err := ts.task(devInstallTask)
	if err != nil {
		return fmt.Errorf("read task %q: %w", devInstallTask, err)
	}
	if task == nil {
		return fmt.Errorf("the task %q is not registered, so installing would need UAC. Ask the user to register it once from an elevated PowerShell: "+
			"powershell -NoProfile -ExecutionPolicy Bypass -File %q", devInstallTask, filepath.Join(repo, "scripts", "dev-install-setup.ps1"))
	}
	if len(task.actions) == 0 || !strings.EqualFold(task.actions[0].path, candidate) {
		got := ""
		if len(task.actions) > 0 {
			got = task.actions[0].path
		}
		return fmt.Errorf("the task %q runs %q, not %s", devInstallTask, got, candidate)
	}
	run, err := ts.runState(devInstallTask)
	if err != nil {
		return err
	}
	if taskBusy(run.state) {
		return errors.New("a development install is queued or running; wait for it before building or installing again")
	}

	if !*noBuild {
		version, err := devBuild(repo, candidateDir)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "built", version)
	}
	hashes := map[string]string{}
	for _, name := range []string{"hyperhand.exe", "hyperhand-agent.exe"} {
		h, err := fileHash(filepath.Join(candidateDir, name))
		if err != nil {
			return err
		}
		if h == "" {
			return fmt.Errorf("missing %s; build first (run without --no-build)", filepath.Join(candidateDir, name))
		}
		hashes[name] = h
	}

	previous := run.lastRun
	time.Sleep(runGap(previous, time.Now()))
	if err := ts.run(devInstallTask); err != nil {
		return err
	}
	fmt.Fprintf(out, "started task %q; waiting up to %d s\n", devInstallTask, *timeout)
	deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
	for {
		time.Sleep(500 * time.Millisecond)
		if run, err = ts.runState(devInstallTask); err != nil {
			return err
		}
		if taskRunDone(previous, run) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the install task did not finish within %d s; it was not stopped: inspect it before retrying", *timeout)
		}
	}
	logPath := filepath.Join(os.Getenv("LOCALAPPDATA"), "HyperHand", "hyperhand.log")
	if err := checkTaskResult(run.result, logPath); err != nil {
		return err
	}
	fmt.Fprintln(out, "install task finished; checking the installation")

	installed := map[string]string{}
	for name, path := range map[string]string{"hyperhand.exe": paths.hostExe, "hyperhand-agent.exe": paths.agentExe} {
		if installed[name], err = fileHash(path); err != nil {
			return err
		}
	}
	if err := checkInstalledHashes(hashes, installed); err != nil {
		return err
	}
	if running, err := serviceRunning(broker.ServiceName); err != nil {
		return fmt.Errorf("query %s: %w", broker.ServiceName, err)
	} else if !running {
		return fmt.Errorf("the HyperHand service (%s) is not running after the install", broker.ServiceName)
	}
	var pids []uint32
	trayDeadline := time.Now().Add(15 * time.Second)
	for {
		if pids, err = processIDs(paths.hostExe); err != nil {
			return err
		}
		if len(pids) == 1 || time.Now().After(trayDeadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(pids) != 1 {
		return fmt.Errorf("expected one installed tray process (%s), found %d", paths.hostExe, len(pids))
	}
	fmt.Fprintf(out, "installed without UAC: service running, tray pid %d. Next: vm_update_agent for each VM.\n", pids[0])
	return nil
}

// devBuild builds both executables into dir with a dev-<time>-<commit>[-dirty] version and returns the version.
func devBuild(repo, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	commit, err := gitOutput(repo, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	status, err := gitOutput(repo, "status", "--porcelain", "--", "cmd", "internal", "go.mod", "go.sum")
	if err != nil {
		return "", err
	}
	version := devVersion(time.Now(), commit, porcelainDirty(status))
	for _, name := range []string{"hyperhand", "hyperhand-agent"} {
		cmd := exec.Command("go", "build", "-ldflags", "-H windowsgui -X hyperhand/internal/proto.Version="+version,
			"-o", filepath.Join(dir, name+".exe"), "./cmd/"+name)
		cmd.Dir, cmd.Stdout, cmd.Stderr = repo, os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("go build failed: %s: %w", name, err)
		}
	}
	return version, nil
}

func gitOutput(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Stderr = os.Stderr
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(b)), nil
}

// devVersion is the version stamped into development builds: dev-<yyyyMMdd-HHmmss>-<short commit>[-dirty].
func devVersion(t time.Time, commit string, dirty bool) string {
	v := "dev-" + t.Format("20060102-150405") + "-" + commit
	if dirty {
		v += "-dirty"
	}
	return v
}

// porcelainDirty reports whether git status --porcelain listed any change.
func porcelainDirty(status string) bool { return strings.TrimSpace(status) != "" }

// taskRun is the run state of a registered task.
type taskRun struct {
	state   int64
	lastRun time.Time
	result  int32
}

func taskBusy(state int64) bool { return state == taskStateQueued || state == taskStateRunning }

// runGap is how long to wait before starting the task so that the new run's LastRunTime (a resolution of seconds)
// differs from previous: at most 2 seconds. LastRunTime comes back from COM as a local time labelled UTC, so it can
// look hours away from time.Now(); without the bound a past run could look like one in the future and block for hours.
func runGap(previous, now time.Time) time.Duration {
	d := previous.Add(2 * time.Second).Sub(now)
	if d < 0 {
		return 0
	}
	if d > 2*time.Second {
		return 2 * time.Second
	}
	return d
}

// taskRunDone reports whether a run newer than previous has completed.
func taskRunDone(previous time.Time, run taskRun) bool {
	return !run.lastRun.Equal(previous) && !taskBusy(run.state)
}

// checkTaskResult turns a nonzero LastTaskResult into an error with the last lines of the installer's log.
func checkTaskResult(result int32, logPath string) error {
	if result == 0 {
		return nil
	}
	tail := ""
	if b, err := os.ReadFile(logPath); err == nil {
		tail = lastLines(string(b), 3)
	}
	return fmt.Errorf("the installer failed (task result 0x%08X). Last log lines (%s):\n%s\nIf the HyperHand service is now stopped, "+
		"run dev-install again with --no-build: the installer restores it", uint32(result), logPath, tail)
}

// lastLines returns the last n lines of text, without the final line break.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// checkInstalledHashes compares the installed files' SHA-256 ("" when missing) with the candidates'.
func checkInstalledHashes(candidates, installed map[string]string) error {
	for _, name := range []string{"hyperhand.exe", "hyperhand-agent.exe"} {
		if installed[name] == "" {
			return fmt.Errorf("the installed %s is missing", name)
		}
		if !strings.EqualFold(installed[name], candidates[name]) {
			return fmt.Errorf("the installed %s is not the build (sha256 %s, build %s)", name, installed[name], candidates[name])
		}
	}
	return nil
}

// runState reads the task's State, LastRunTime and LastTaskResult.
func (ts *taskScheduler) runState(name string) (taskRun, error) {
	var r taskRun
	t, err := oleutil.CallMethod(ts.folder, "GetTask", name)
	if err != nil {
		return r, fmt.Errorf("read task %s: %w", name, err)
	}
	defer t.Clear()
	for _, p := range []string{"State", "LastRunTime", "LastTaskResult"} {
		v, err := oleutil.GetProperty(t.ToIDispatch(), p)
		if err != nil {
			return r, fmt.Errorf("read task %s %s: %w", name, p, err)
		}
		switch val := v.Value().(type) {
		case time.Time:
			r.lastRun = val
		case float64: // a DATE go-ole cannot convert, such as the never-run 1899-12-30
			r.lastRun = time.Time{}
		case int32:
			if p == "State" {
				r.state = int64(val)
			} else {
				r.result = val
			}
		case int64:
			if p == "State" {
				r.state = val
			} else {
				r.result = int32(val)
			}
		default:
			v.Clear()
			return r, fmt.Errorf("read task %s %s: unexpected type %T", name, p, val)
		}
		v.Clear()
	}
	return r, nil
}

// serviceRunning reports whether the service is running, with only the query rights an ordinary user has.
func serviceRunning(name string) (bool, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, err
	}
	defer windows.CloseServiceHandle(m)
	n, _ := windows.UTF16PtrFromString(name)
	s, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false, err
	}
	defer windows.CloseServiceHandle(s)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(s, &st); err != nil {
		return false, err
	}
	return st.CurrentState == windows.SERVICE_RUNNING, nil
}

// processIDs lists the processes other than this one running the executable exe.
// processIDs lists this user's processes running exe. dev-install runs unelevated, so it cannot open the service's
// process (another account, same executable); processes it cannot open are not the tray and are skipped.
func processIDs(exe string) ([]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	name := strings.ToLower(filepath.Base(exe))
	var pids []uint32
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if strings.ToLower(windows.UTF16ToString(e.ExeFile[:])) != name {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
		if err != nil {
			continue // exited, or another account's process (the service)
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		n := uint32(len(buf))
		if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil && strings.EqualFold(windows.UTF16ToString(buf[:n]), exe) {
			pids = append(pids, e.ProcessID)
		}
		windows.CloseHandle(h)
	}
	return pids, nil
}

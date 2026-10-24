package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

// TestLaunchChildProcess is the program TestLaunch starts: it records its PID in the marker file and exits.
func TestLaunchChildProcess(t *testing.T) {
	if marker := os.Getenv("HYPERHAND_LAUNCH_TEST_MARKER"); marker != "" {
		os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0o644)
		os.Exit(0)
	}
}

func TestLaunch(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "admin"}[admin], func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "pid")
			t.Setenv("HYPERHAND_LAUNCH_TEST_MARKER", marker)
			a := proto.LaunchArgs{Path: os.Args[0], Args: []string{"-test.run=^TestLaunchChildProcess$"}, Cwd: filepath.Dir(marker), Admin: admin}
			var res any
			var err error
			if admin {
				res, _, err = launchAdminWithLauncher(context.Background(), a, testAdminLauncher)
			} else {
				res, _, err = launch(context.Background(), mustJSON(a), nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			pid := res.(proto.LaunchResult).PID
			if pid == 0 {
				t.Fatal("no PID")
			}
			for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if data, err := os.ReadFile(marker); err == nil {
					if string(data) != strconv.Itoa(int(pid)) {
						t.Fatalf("child PID %s, launch reported %d", data, pid)
					}
					return
				}
				if time.Now().After(end) {
					t.Fatal("launched process did not run")
				}
			}
		})
	}
}

func TestLaunchErrors(t *testing.T) {
	for _, a := range []proto.LaunchArgs{{}, {Path: filepath.Join(t.TempDir(), "missing.exe")}} {
		if _, _, err := launch(context.Background(), mustJSON(a), nil); err == nil {
			t.Errorf("accepted %+v", a)
		}
	}
}

func TestLaunchAUMIDErrors(t *testing.T) {
	aumid := "HyperHand.Test.NotInstalled_0000000000000!App"
	for _, a := range []proto.LaunchArgs{
		{AUMID: aumid, Path: os.Args[0]},
		{AUMID: aumid, Cwd: t.TempDir()},
		{AUMID: aumid, Admin: true},
		{AUMID: `C:\Windows\notepad.exe`},
		// Well formed but not installed: ActivateApplication fails (AO_NOERRORUI, so no dialog) and nothing starts.
		{AUMID: aumid},
	} {
		_, _, err := launch(context.Background(), mustJSON(a), nil)
		if err == nil {
			t.Errorf("accepted %+v", a)
			continue
		}
		t.Logf("%+v: %v", a, err)
	}
}

func TestProcessAUMIDOfDesktopProcess(t *testing.T) {
	if id := processAUMID(windows.CurrentProcess()); id != "" {
		t.Errorf("the test process has no package identity, got %q", id)
	}
}

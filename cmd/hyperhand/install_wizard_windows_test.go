package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWizardInstallRejectsExtraArgumentsWithoutDialog(t *testing.T) {
	if os.Getenv("HYPERHAND_TEST_WIZARD_ARGS") == "1" {
		os.Args = []string{os.Args[0], "install", "--wizard", "--owner-user", "another-account"}
		main()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWizardInstallRejectsExtraArgumentsWithoutDialog$")
	cmd.Env = append(os.Environ(), "HYPERHAND_TEST_WIZARD_ARGS=1", "LOCALAPPDATA="+t.TempDir())
	err := cmd.Run()
	var exitErr *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("wizard failure must exit 1 without a dialog: err=%v, context=%v", err, ctx.Err())
	}
}

func TestWaitWizardInstall(t *testing.T) {
	if value := os.Getenv("HYPERHAND_TEST_WIZARD_EXIT"); value != "" {
		code, _ := strconv.Atoi(value)
		time.Sleep(200 * time.Millisecond)
		if err := os.WriteFile(os.Getenv("HYPERHAND_TEST_WIZARD_DONE"), []byte("done"), 0o600); err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	for _, code := range []int{0, 23} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := filepath.Join(t.TempDir(), "done")
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWaitWizardInstall$")
			cmd.Env = append(os.Environ(), "HYPERHAND_TEST_WIZARD_EXIT="+strconv.Itoa(code), "HYPERHAND_TEST_WIZARD_DONE="+done)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Wait()
			h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(h)
			err = waitWizardInstall(h)
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			if code == 0 && err != nil || code != 0 && (err == nil || !strings.Contains(err.Error(), "exit code 23")) {
				t.Fatalf("exit %d: %v", code, err)
			}
			if b, err := os.ReadFile(done); err != nil || string(b) != "done" {
				t.Fatalf("returned before installation completed: %q, %v", b, err)
			}
		})
	}
}

func TestWizardWorkerUsesSeparateImage(t *testing.T) {
	if os.Getenv("HYPERHAND_TEST_WIZARD_WORKER") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	worker, err := stageWizardInstall(os.Args[0], t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{os.Args[0], worker} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, image, "-test.run=^TestWizardWorkerUsesSeparateImage$")
		cmd.Env = append(os.Environ(), "HYPERHAND_TEST_WIZARD_WORKER=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// This is the same path selection stopProcesses uses, but without terminating
		// anything. The installer's worker image must never match its waiting parent.
		h, err := openProcessWithPath(uint32(cmd.Process.Pid), windows.PROCESS_QUERY_LIMITED_INFORMATION, worker)
		matches := h != 0
		if h != 0 {
			windows.CloseHandle(h)
		}
		cmd.Process.Kill()
		cmd.Wait()
		if err != nil || matches != (image == worker) {
			t.Fatalf("image %q: selected=%v, err=%v", image, matches, err)
		}
	}
}

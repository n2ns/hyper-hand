package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDevVersion(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 5, 7, 0, time.Local)
	if v := devVersion(at, "5de891d", false); v != "dev-20261011-090507-5de891d" {
		t.Errorf("clean: %s", v)
	}
	if v := devVersion(at, "5de891d", true); v != "dev-20261011-090507-5de891d-dirty" {
		t.Errorf("dirty: %s", v)
	}
}

func TestPorcelainDirty(t *testing.T) {
	for status, want := range map[string]bool{
		"":                              false,
		"\n":                            false,
		" M cmd/hyperhand/main.go\n":    true,
		"?? internal/agent/new.go":      true,
		"M  go.mod\n?? internal/x.go\n": true,
	} {
		if got := porcelainDirty(status); got != want {
			t.Errorf("%q: %v", status, got)
		}
	}
}

func TestTaskRunDone(t *testing.T) {
	prev := time.Date(2026, 10, 11, 9, 0, 0, 0, time.Local)
	later := prev.Add(3 * time.Second)
	for _, tt := range []struct {
		run  taskRun
		want bool
	}{
		{taskRun{state: 3, lastRun: prev}, false},                 // not started yet
		{taskRun{state: taskStateQueued, lastRun: prev}, false},   // queued
		{taskRun{state: taskStateRunning, lastRun: later}, false}, // running
		{taskRun{state: 3, lastRun: later}, true},                 // ready again after a new run
		{taskRun{state: 1, lastRun: later}, true},                 // disabled meanwhile, but the run completed
	} {
		if got := taskRunDone(prev, tt.run); got != tt.want {
			t.Errorf("%+v: %v", tt.run, got)
		}
	}
	if !taskRunDone(time.Time{}, taskRun{state: 3, lastRun: later}) {
		t.Error("first run of a never-run task")
	}
}

func TestCheckTaskResult(t *testing.T) {
	if err := checkTaskResult(0, filepath.Join(t.TempDir(), "missing.log")); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "hyperhand.log")
	if err := os.WriteFile(log, []byte("one\r\ntwo\r\nthree\r\nfour\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkTaskResult(-2147024891, log) // 0x80070005
	if err == nil {
		t.Fatal("nonzero result accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "0x80070005") || !strings.Contains(msg, "two\nthree\nfour\n") || strings.Contains(msg, "one") || !strings.Contains(msg, "--no-build") {
		t.Fatalf("message: %s", msg)
	}
	if err := checkTaskResult(1, filepath.Join(t.TempDir(), "missing.log")); err == nil || !strings.Contains(err.Error(), "--no-build") {
		t.Fatalf("missing log: %v", err)
	}
}

func TestLastLines(t *testing.T) {
	for text, want := range map[string]string{"": "", "a": "a", "a\nb\nc\nd\n": "b\nc\nd", "a\r\nb": "a\nb"} {
		if got := lastLines(text, 3); got != want {
			t.Errorf("%q: %q", text, got)
		}
	}
}

func TestCheckInstalledHashes(t *testing.T) {
	build := map[string]string{"hyperhand.exe": "aa", "hyperhand-agent.exe": "bb"}
	if err := checkInstalledHashes(build, map[string]string{"hyperhand.exe": "AA", "hyperhand-agent.exe": "bb"}); err != nil {
		t.Fatal(err)
	}
	for _, installed := range []map[string]string{
		{"hyperhand.exe": "aa", "hyperhand-agent.exe": "cc"},
		{"hyperhand.exe": "", "hyperhand-agent.exe": "bb"},
	} {
		if err := checkInstalledHashes(build, installed); err == nil {
			t.Errorf("accepted %v", installed)
		}
	}
}

func TestDevInstallRefusesOutsideRepository(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	var out strings.Builder
	err := devInstall([]string{"--no-build"}, &out)
	if err == nil || !strings.Contains(err.Error(), "repository root") || out.Len() != 0 {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
	if err := devInstall([]string{"extra"}, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("extra argument: %v", err)
	}
}

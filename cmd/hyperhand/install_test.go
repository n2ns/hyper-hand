package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseInstallArgs(t *testing.T) {
	owner, err := setupOwner(nil)
	if err != nil {
		t.Fatal(err)
	}
	identityArgs := []string{"--owner-sid", owner.SID, "--owner-user", owner.User}
	for _, tc := range []struct {
		name  string
		args  []string
		quiet bool
	}{
		{"normal", nil, false},
		{"normal owner", identityArgs, false},
		{"quiet", []string{"--quiet"}, true},
		{"quiet owner", append([]string{"--quiet"}, identityArgs...), true},
		{"owner quiet", append(append([]string{}, identityArgs...), "--quiet"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quiet, args, err := parseInstallArgs(tc.args)
			if err != nil || quiet != tc.quiet {
				t.Fatalf("quiet=%v, err=%v", quiet, err)
			}
			got, err := setupOwner(args)
			if err != nil || got != owner {
				t.Fatalf("owner=%#v, err=%v", got, err)
			}
			if strings.Contains(tc.name, "owner") && !reflect.DeepEqual(args, identityArgs) {
				t.Fatalf("owner arguments changed: %v", args)
			}
		})
	}
	for _, args := range [][]string{{"--quiet", "--unknown"}, {"--quiet", "unexpected"}, {"--quiet", "--owner-user"}} {
		quiet, _, err := parseInstallArgs(args)
		if err == nil || !quiet {
			t.Fatalf("invalid quiet arguments %v: quiet=%v, err=%v", args, quiet, err)
		}
	}
}

func TestQuietInstallRequiresElevationBeforeSetup(t *testing.T) {
	// An invalid account proves this returns before owner lookup or installation.
	quiet, err := installWithElevation([]string{"--quiet", "--owner-user", "not-a-real-setup-account"}, false)
	if !quiet || err == nil || !strings.Contains(err.Error(), "requires an elevated process") {
		t.Fatalf("quiet=%v, err=%v", quiet, err)
	}
}

func TestQuietInstallErrorExitsWithoutDialog(t *testing.T) {
	if os.Getenv("HYPERHAND_TEST_QUIET_INSTALL") == "1" {
		os.Args = []string{os.Args[0], "install", "--quiet", "--unknown"}
		main()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestQuietInstallErrorExitsWithoutDialog$")
	cmd.Env = append(os.Environ(), "HYPERHAND_TEST_QUIET_INSTALL=1", "LOCALAPPDATA="+t.TempDir())
	err := cmd.Run()
	var exitErr *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("quiet failure must exit 1 without a dialog: err=%v, context=%v", err, ctx.Err())
	}
}

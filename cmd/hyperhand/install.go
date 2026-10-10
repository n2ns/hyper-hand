package main

import (
	"errors"
	"flag"
	"io"

	"golang.org/x/sys/windows"
)

func parseInstallArgs(args []string) (quiet bool, ownerArgs []string, err error) {
	var owner setupIdentity
	f := flag.NewFlagSet("install", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.BoolVar(&quiet, "quiet", false, "install without dialogs; requires an elevated process")
	f.StringVar(&owner.SID, "owner-sid", "", "original installing user SID")
	f.StringVar(&owner.User, "owner-user", "", "original installing account")
	if err = f.Parse(args); err != nil {
		return
	}
	if f.NArg() != 0 {
		return quiet, nil, errors.New("unexpected install arguments")
	}
	if owner.SID != "" || owner.User != "" {
		ownerArgs = []string{"--owner-sid", owner.SID, "--owner-user", owner.User}
	}
	return
}

// install is the only elevation boundary; the installed tray remains a normal
// interactive-user application and the broker uses a virtual service account.
func install(args []string) (bool, error) {
	return installWithElevation(args, windows.GetCurrentProcessToken().IsElevated())
}

func installWithElevation(args []string, elevated bool) (bool, error) {
	quiet, ownerArgs, err := parseInstallArgs(args)
	if err != nil {
		return quiet, err
	}
	if quiet && !elevated {
		return true, errors.New("quiet install requires an elevated process; run the authorized development install task or use install without --quiet")
	}
	completed, err := runSetup("install", ownerArgs)
	if err == nil && completed && !quiet {
		msgBox("HyperHand installed. The dedicated broker service starts automatically; the tray runs at logon with normal user permissions.\nMCP: http://127.0.0.1:8770/mcp", windows.MB_ICONINFORMATION)
	}
	return quiet, err
}

package main

import (
	"errors"
	"log"
	"os"
	"slices"

	"golang.org/x/sys/windows"
)

// parseUninstallArgs separates --quiet from the arguments runSetup takes (the owner of an elevated relaunch).
func parseUninstallArgs(args []string) (quiet bool, setupArgs []string) {
	setupArgs = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--quiet" || a == "-quiet" })
	return len(setupArgs) != len(args), setupArgs
}

func uninstall() {
	quiet, args := parseUninstallArgs(os.Args[2:])
	if quiet && !windows.GetCurrentProcessToken().IsElevated() {
		// Like install --quiet: an elevated relaunch could not report its result to the caller (a package manager).
		log.Print("uninstall: ", errors.New("quiet uninstall requires an elevated process; use uninstall without --quiet"))
		os.Exit(1)
	}
	completed, err := runSetup("uninstall", args)
	if quiet {
		if err != nil {
			log.Print("uninstall: ", err)
			os.Exit(1)
		}
		return
	}
	if err != nil {
		msgBox("HyperHand uninstall incomplete:\n"+err.Error(), windows.MB_ICONERROR)
	} else if completed {
		msgBox("HyperHand service, logon task and service-account permissions removed. Installed executables will be removed after this dialog closes. Guest files, user logs and non-empty service data are preserved.", windows.MB_ICONINFORMATION)
	}
}

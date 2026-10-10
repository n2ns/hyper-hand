package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func uninstall() {
	completed, err := runSetup("uninstall", os.Args[2:])
	if err != nil {
		msgBox("HyperHand uninstall incomplete:\n"+err.Error(), windows.MB_ICONERROR)
	} else if completed {
		msgBox("HyperHand service, logon task and service-account permissions removed. Installed executables will be removed after this dialog closes. Guest files, user logs and non-empty service data are preserved.", windows.MB_ICONINFORMATION)
	}
}

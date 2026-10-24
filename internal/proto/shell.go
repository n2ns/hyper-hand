package proto

import (
	"slices"
	"strings"
)

// shellFlyoutProcesses are the image names (lower case) of the shell processes whose foreground windows are flyouts
// that close on Esc: Start, search, action center and notification flyouts, the touch keyboard and emoji panel.
var shellFlyoutProcesses = []string{
	"startmenuexperiencehost.exe", "searchhost.exe", "searchapp.exe", "searchui.exe", "shellexperiencehost.exe", "textinputhost.exe",
}

// ShellFlyout reports whether a window of this process image name and window class is a shell flyout that closes on
// Esc. The agent's focus_window closes one with Esc before activating; the host names Esc for one that stayed.
func ShellFlyout(process, class string) bool {
	return slices.Contains(shellFlyoutProcesses, strings.ToLower(process)) || strings.EqualFold(class, "Shell_LightDismissOverlay")
}

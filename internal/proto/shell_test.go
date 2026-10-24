package proto

import "testing"

func TestShellFlyout(t *testing.T) {
	for _, tc := range []struct {
		process, class string
		want           bool
	}{
		{"SearchHost.exe", "", true},
		{"STARTMENUEXPERIENCEHOST.EXE", "", true},
		{"explorer.exe", "shell_lightdismissoverlay", true},
		{"explorer.exe", "CabinetWClass", false},
		{"acad.exe", "Afx:400000", false},
		{"", "", false},
	} {
		if got := ShellFlyout(tc.process, tc.class); got != tc.want {
			t.Errorf("ShellFlyout(%q, %q) = %v", tc.process, tc.class, got)
		}
	}
}

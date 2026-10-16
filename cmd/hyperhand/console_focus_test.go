package main

import "testing"

func TestConsoleFocusRequestRequiresExactIdentity(t *testing.T) {
	vm, h, pid, err := parseConsoleFocusRequest([]string{"Win10-Example", "3409288", "26744"})
	if err != nil || vm != "Win10-Example" || h != 3409288 || pid != 26744 {
		t.Fatalf("parsed %q %d %d: %v", vm, h, pid, err)
	}
	for _, args := range [][]string{
		nil, {"Win10"}, {"Win10", "1", "2", "extra"},
		{"", "1", "2"}, {"Win10\x00", "1", "2"}, {"Win10\r\n", "1", "2"},
		{`Win10"`, "1", "2"}, {`Win10\`, "1", "2"},
		{"Win10", "0", "2"}, {"Win10", "1", "0"},
		{"Win10", "-1", "2"}, {"Win10", "+1", "2"}, {"Win10", " 1", "2"},
		{"Win10", "0x123", "2"}, {"Win10", "1 /edit", "2"},
		{"Win10", "18446744073709551616", "2"}, {"Win10", "1", "4294967296"},
		{"Win10", "1", "2&exit"}, {"Win10", "", "2"}, {"Win10", "1", ""},
	} {
		if _, _, _, err := parseConsoleFocusRequest(args); err == nil {
			t.Errorf("accepted invalid request %q", args)
		}
	}
}

func TestConsoleFocusTargetRequiresLocalSystemViewer(t *testing.T) {
	const system = `C:\Windows\System32`
	for _, tc := range []struct {
		name, vm, title, image string
		pid, currentPID        uint32
		want                   bool
	}{
		{"local host", "Win10", "Win10 on TEST-HOST - Virtual Machine Connection", system + `\vmconnect.exe`, 12, 12, true},
		{"localhost Chinese", "Win10", "localhost 上的 Win10 - 虚拟机连接", `c:\windows\system32\VMCONNECT.EXE`, 12, 12, true},
		{"changed PID", "Win10", "Win10 on TEST-HOST - Virtual Machine Connection", system + `\vmconnect.exe`, 12, 13, false},
		{"zero PID", "Win10", "Win10 on TEST-HOST - Virtual Machine Connection", system + `\vmconnect.exe`, 0, 0, false},
		{"wrong VM", "Win10", "Win10-Example on TEST-HOST - Virtual Machine Connection", system + `\vmconnect.exe`, 12, 12, false},
		{"VM case mismatch", "Win10", "win10 on TEST-HOST - Virtual Machine Connection", system + `\vmconnect.exe`, 12, 12, false},
		{"remote host", "Win10", "Win10 on OTHER - Virtual Machine Connection", system + `\vmconnect.exe`, 12, 12, false},
		{"unknown title", "Win10", "Win10", system + `\vmconnect.exe`, 12, 12, false},
		{"wrong image", "Win10", "Win10 on TEST-HOST - Virtual Machine Connection", `C:\Temp\vmconnect.exe`, 12, 12, false},
		{"wrong executable", "Win10", "Win10 on TEST-HOST - Virtual Machine Connection", system + `\notepad.exe`, 12, 12, false},
		{"unreadable image", "Win10", "Win10 on TEST-HOST - Virtual Machine Connection", "", 12, 12, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := consoleFocusTargetMatches(tc.vm, tc.pid, tc.currentPID, tc.title, tc.image, system, "TEST-HOST"); got != tc.want {
				t.Fatalf("match %v, want %v", got, tc.want)
			}
		})
	}
}

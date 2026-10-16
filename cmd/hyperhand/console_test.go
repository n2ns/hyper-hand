package main

import (
	"path/filepath"
	"testing"

	"hyperhand/internal/hyperv"
)

func TestCheckConsoleVM(t *testing.T) {
	vms := []hyperv.VM{{Name: "Win10"}, {Name: "Test VM"}}
	for _, ok := range []string{"Win10", "Test VM"} {
		if err := checkConsoleVM(ok, vms); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	// Only exact names of existing VMs: the name becomes a quoted vmconnect.exe argument.
	for _, bad := range []string{"", "win10", "Missing", `Win10" /edit`, "Win10\r\n"} {
		if err := checkConsoleVM(bad, vms); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
	for _, name := range []string{`a"b`, `Test\`} {
		if err := checkConsoleVM(name, []hyperv.VM{{Name: name}}); err == nil {
			t.Errorf("%q must be refused even when it exists", name)
		}
	}
}

func TestConsoleTitleVM(t *testing.T) {
	for _, c := range []struct {
		title, want string
		ok          bool
	}{
		{"localhost 上的 Win10 - 虚拟机连接", "Win10", true},
		{"Win10 on localhost - Virtual Machine Connection", "Win10", true},
		{"Win10 Test on localhost - Virtual Machine Connection", "Win10 Test", true},
		{"localhost 上的 Win10 Test - 虚拟机连接", "Win10 Test", true},
		{"localhost 上的 A - B - 虚拟机连接", "A - B", true}, // the product name follows the last " - "
		{"localhost on localhost - Virtual Machine Connection", "localhost", true},
		{"VECTOR 上的 Win10-PipeSifu - 虚拟机连接", "Win10-PipeSifu", true},
		{"Win10-PipeSifu on VECTOR - Virtual Machine Connection", "Win10-PipeSifu", true},
		{"vector 上的 Win10-PipeSifu - 虚拟机连接", "Win10-PipeSifu", true},
		{"LOCALHOST 上的 Win10-PipeSifu - 虚拟机连接", "Win10-PipeSifu", true},
		{"VECTOR 上的 Test on Other - 虚拟机连接", "Test on Other", true},
		{"Test on Other on VECTOR - Virtual Machine Connection", "Test on Other", true},
		{"Test 上的 Other on VECTOR - Virtual Machine Connection", "Test 上的 Other", true},
		{"VECTOR 上的 Test 上的 Other - 虚拟机连接", "Test 上的 Other", true},
		{"OTHER 上的 Test on VECTOR - 虚拟机连接", "", false},
		{"VECTOR 上的 Test on OTHER - Virtual Machine Connection", "", false},
		{"OTHER 上的 Win10-PipeSifu - 虚拟机连接", "", false},
		{"Win10-PipeSifu on OTHER - Virtual Machine Connection", "", false},
		{"VECTOR2 上的 Win10-PipeSifu - 虚拟机连接", "", false},
		{"VECTOR wrong Win10-PipeSifu - 虚拟机连接", "", false},
		{"VECTOR 上的  - 虚拟机连接", "", false},
		{"虚拟机连接", "", false},
		{"Win10 - Virtual Machine Connection", "", false},
		{"localhost - Virtual Machine Connection", "", false},
	} {
		if got, ok := consoleTitleVMForHost(c.title, "VECTOR"); got != c.want || ok != c.ok {
			t.Errorf("%q: %q %v, want %q %v", c.title, got, ok, c.want, c.ok)
		}
	}
	if _, ok := consoleTitleVMForHost("Win10 on VECTOR - Virtual Machine Connection", ""); ok {
		t.Fatal("accepted an unknown hostname")
	}
}

func TestConsoleSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json")
	s := loadHostSettings(path) // missing file: nothing enabled
	if s.onStart("Win10") {
		t.Fatal("enabled without a settings file")
	}
	if err := s.setOnStart("Win10", true); err != nil {
		t.Fatal(err)
	}
	if err := s.setOnStart("Other", true); err != nil {
		t.Fatal(err)
	}
	if err := s.setOnStart("Other", false); err != nil {
		t.Fatal(err)
	}
	if s.port() != 0 {
		t.Errorf("default port %d", s.port())
	}
	if err := s.setPort(8899); err != nil {
		t.Fatal(err)
	}
	r := loadHostSettings(path)
	if !r.onStart("Win10") || r.onStart("Other") || r.port() != 8899 {
		t.Errorf("reloaded: %v port %d", r.vms, r.port())
	}
}

func TestMCPPort(t *testing.T) {
	for _, c := range []struct {
		flag     int
		explicit bool
		saved    int
		want     int
	}{
		{8770, false, 0, 8770},
		{8770, false, 8899, 8899},
		{9000, true, 8899, 9000}, // an explicit -port wins
	} {
		if got := mcpPort(c.flag, c.explicit, c.saved); got != c.want {
			t.Errorf("%+v: %d", c, got)
		}
	}
	for _, bad := range []string{"", "abc", "0", "80", "1023", "65536", "8770x"} {
		if _, err := parsePort(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if p, err := parsePort(" 8899 "); err != nil || p != 8899 {
		t.Errorf("8899: %d %v", p, err)
	}
}

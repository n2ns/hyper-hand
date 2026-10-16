package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"hyperhand/internal/proto"
)

func TestConsoleRequestOnlyAcceptsFixedModeAndGUID(t *testing.T) {
	const id = "d7786ff7-1402-47a1-a8bd-11c8fa0ff8c5"
	for _, mode := range []string{"open", "recover"} {
		got, vm, err := parseConsoleRequest([]string{mode + ":" + id})
		if err != nil || got != mode || vm != id {
			t.Fatalf("%s: %s %s %v", mode, got, vm, err)
		}
	}
	for _, args := range [][]string{nil, {"open:" + id, "extra"}, {"install:" + id}, {"open:Win10"}, {"open:{" + id + "}"}, {"open:" + id + `" /edit`}, {"recover:xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"}} {
		if _, _, err := parseConsoleRequest(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestConsoleTaskMigrationOnlyAcceptsKnownActions(t *testing.T) {
	p := setupPaths{hostExe: `C:\Program Files\HyperHand\hyperhand.exe`, vmconnect: `C:\Windows\System32\vmconnect.exe`}
	for _, action := range []taskAction{{p.vmconnect, `localhost "$(Arg0)"`}, {p.hostExe, consoleTaskArgs}} {
		task := &taskInfo{userSID: "owner", runLevel: 1, actions: []taskAction{action}}
		if !recognizedConsoleTask(task, p, "owner") {
			t.Fatalf("rejected known action: %+v", action)
		}
		task.actions[0].args += " arbitrary"
		if recognizedConsoleTask(task, p, "owner") {
			t.Fatal("accepted extra command arguments")
		}
		task.actions[0] = action
		task.userSID = "other"
		if recognizedConsoleTask(task, p, "owner") {
			t.Fatal("accepted another owner")
		}
		task.userSID = "owner"
		task.triggers = 1
		if recognizedConsoleTask(task, p, "owner") {
			t.Fatal("accepted a triggered helper")
		}
	}
	if recognizedConsoleTask(nil, p, "owner") {
		t.Fatal("accepted missing task")
	}
}

func TestConsoleReconnectPromptDoesNotMatchOtherDialogs(t *testing.T) {
	button := func(name string) proto.ControlInfo {
		return proto.ControlInfo{Name: name, ControlType: 50000, Enabled: true}
	}
	for _, names := range [][2]string{{"&Reconnect", "E&xit"}, {"重新连接(R)", "退出(X)"}, {"Connect", "Exit"}, {"连接(C)", "退出(X)"}} {
		if !reconnectPrompt([]proto.ControlInfo{button(names[0]), button(names[1])}) {
			t.Errorf("missed %v", names)
		}
	}
	for _, nodes := range [][]proto.ControlInfo{{button("Connect"), button("Cancel")}, {button("Reconnect")}, {button("Exit")}, nil} {
		if reconnectPrompt(nodes) {
			t.Errorf("matched unrelated dialog %+v", nodes)
		}
	}
	hidden := button("Reconnect")
	hidden.Offscreen = true
	if reconnectPrompt([]proto.ControlInfo{hidden, button("Exit")}) {
		t.Fatal("matched hidden reconnect")
	}
}

func TestConsoleConnectedScopesSearchToStatusBar(t *testing.T) {
	for _, tc := range []struct {
		lock proto.ControlInfo
		want bool
	}{
		{proto.ControlInfo{ControlType: 50020, Name: "连接不安全"}, true},
		{proto.ControlInfo{ControlType: 50020}, false},
		{proto.ControlInfo{ControlType: 50020, Name: "Connection unsecure", Offscreen: true}, false},
		{proto.ControlInfo{ControlType: 50020, Name: "状态: 正在运行"}, false},
		{proto.ControlInfo{ControlType: 50020, Name: "已捕获到键盘输入"}, false},
		{proto.ControlInfo{ClassName: "TscShellContainerClass"}, false},
	} {
		calls := 0
		got, err := findConsoleVideo(123, 456, func(a proto.FindControlsArgs) (proto.FindControlsResult, error) {
			calls++
			if a.Handle != 123 || a.PID != 456 || a.MaxDepth != 1 {
				t.Fatalf("unbounded or wrong-window query: %+v", a)
			}
			if calls == 1 {
				if a.AutomationID != "StatusBar" || a.RootRuntimeID != "" {
					t.Fatalf("first query is not shallow status-bar search: %+v", a)
				}
				return proto.FindControlsResult{Matches: []proto.ControlInfo{{RuntimeID: "status", Rect: proto.Rect{Left: 10}}}}, nil
			}
			if a.RootRuntimeID != "status" || a.HintRect == nil || a.HintRect.Left != 10 || a.AutomationID != "" || a.ControlType != 50020 {
				t.Fatalf("video query escaped status bar: %+v", a)
			}
			return proto.FindControlsResult{Matches: []proto.ControlInfo{tc.lock}}, nil
		})
		if err != nil || got != tc.want || calls != 2 {
			t.Fatalf("got %v, %v, calls %d; want %v", got, err, calls, tc.want)
		}
	}
	if ready, err := findConsoleVideo(123, 456, func(proto.FindControlsArgs) (proto.FindControlsResult, error) {
		return proto.FindControlsResult{}, context.DeadlineExceeded
	}); ready || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must remain unknown: %v, %v", ready, err)
	}
}

func TestConsoleVideoLabelsMatchOnlyConnectedResources(t *testing.T) {
	for _, label := range []string{
		"Connection unsecure", "Connection secured using certificate", "Connection secured using Kerberos", "Connection secured using Kerberos with certification",
		"连接不安全", "已使用证书保证连接安全", "已使用 Kerberos 保证连接安全", "已使用带证书的 Kerberos 保证连接安全",
	} {
		if !consoleVideoLabel(label) {
			t.Errorf("missed video status %q", label)
		}
	}
	for _, label := range []string{"", "状态: 正在运行", "State: Running", "Connection unsecure pending", "未捕获到鼠标输入", "LockIcon"} {
		if consoleVideoLabel(label) {
			t.Errorf("accepted unrelated status %q", label)
		}
	}
}

func TestConsoleVideoSurfaceSupportsBasicAndShellRenderers(t *testing.T) {
	for _, class := range []string{"OPWindowClass", "TscShellContainerClass"} {
		if !consoleVideoSurface(class, true) {
			t.Errorf("missed visible %s", class)
		}
		if consoleVideoSurface(class, false) {
			t.Errorf("accepted hidden %s", class)
		}
	}
	for _, class := range []string{"", "IHWindowClass", "UIContainerClass", "OPContainerClass", "WindowsForms10.Window.8.app.0.aa0c13_r6_ad1"} {
		if consoleVideoSurface(class, true) {
			t.Errorf("accepted a non-video surface %s", class)
		}
	}
}

func TestConsoleReusedViewerObservationDoesNotClaimBasic(t *testing.T) {
	for _, tc := range []struct {
		connected bool
		err       error
		want      string
	}{
		{true, nil, "viewer_connected"},
		{false, nil, "unchanged"},
		{false, context.DeadlineExceeded, "unchanged"},
		{true, context.DeadlineExceeded, "unchanged"},
	} {
		calls := 0
		state, err := reusedConsoleState(func() (bool, error) {
			calls++
			return tc.connected, tc.err
		})
		if state != tc.want || !errors.Is(err, tc.err) || calls != 1 {
			t.Fatalf("state %s err %v calls %d; want %s, %v", state, err, calls, tc.want, tc.err)
		}
	}
}

func TestConsoleBasicPreferenceRequiresExplicitFalse(t *testing.T) {
	for _, c := range []struct {
		value string
		want  bool
	}{{"False", true}, {"false", true}, {"True", false}, {"", false}, {"invalid", false}} {
		data := fmt.Sprintf(`<configuration><Microsoft.Virtualization.Client.ClientVirtualizationSettings><setting name="VMConnectUseEnhancedMode" type="System.Boolean"><value>%s</value></setting></Microsoft.Virtualization.Client.ClientVirtualizationSettings></configuration>`, c.value)
		if got := parseBasicConsolePreference([]byte(data)); got != c.want {
			t.Errorf("%q: %v", c.value, got)
		}
	}
	if parseBasicConsolePreference([]byte(`<configuration/>`)) || parseBasicConsolePreference([]byte(`invalid`)) {
		t.Fatal("unknown preferences accepted as basic")
	}
}

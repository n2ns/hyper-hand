package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

func TestAppsCombine(t *testing.T) {
	source := appSourceResult{Completed: 1, Apps: []appCandidate{
		{Name: "Zulu", Path: `C:\Apps\z.exe`},
		{Name: "Editor", Path: `C:\Apps\edit.exe`, Arguments: `--profile "中文 空格" ""`, Cwd: `C:\Work`},
		{Name: "Editor alias", Path: `c:\apps\EDIT.EXE`, Arguments: `--profile "中文 空格" ""`, Cwd: `c:\WORK`},
		{Name: "Editor other profile", Path: `C:\Apps\edit.exe`, Arguments: `--profile other`, Cwd: `C:\Work`},
		{Name: "Editor other cwd", Path: `C:\Apps\edit.exe`, Arguments: `--profile "中文 空格" ""`, Cwd: `C:\Other`},
		{Name: "Unsupported", Path: `C:\Apps\script.cmd`},
	}}
	r, err := combineApps(proto.ListAppsArgs{Limit: 2}, source)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 4 || len(r.Apps) != 2 || !r.Truncated {
		t.Fatalf("unexpected totals: %+v", r)
	}
	if r.Apps[0].Name != "Editor" || !reflect.DeepEqual(r.Apps[0].Launch.Args, []string{"--profile", "中文 空格", ""}) {
		t.Fatalf("shortcut semantics lost: %+v", r.Apps[0])
	}
	alias, err := combineApps(proto.ListAppsArgs{Query: "ALIAS", Limit: 50}, source)
	if err != nil || alias.Total != 1 || alias.Apps[0].ID != r.Apps[0].ID {
		t.Fatalf("alias identity changed: %+v, %v", alias, err)
	}
	path, err := combineApps(proto.ListAppsArgs{Query: `APPS\Z.EXE`, Limit: 50}, source)
	if err != nil || path.Total != 1 || path.Apps[0].Name != "Zulu" {
		t.Fatalf("path query: %+v, %v", path, err)
	}
	for i, j := 0, len(source.Apps)-1; i < j; i, j = i+1, j-1 {
		source.Apps[i], source.Apps[j] = source.Apps[j], source.Apps[i]
	}
	reversed, err := combineApps(proto.ListAppsArgs{Limit: 2}, source)
	if err != nil || !reflect.DeepEqual(r, reversed) {
		t.Fatalf("source order changed results: %+v, %v", reversed, err)
	}
}

func TestAppsSourceFailure(t *testing.T) {
	failed := appSourceResult{Warnings: []string{"Start Menu: access denied"}}
	if _, err := combineApps(proto.ListAppsArgs{Limit: 50}, failed); err == nil {
		t.Fatal("all failed sources must return error")
	}
	r, err := combineApps(proto.ListAppsArgs{Limit: 50}, failed, appSourceResult{Completed: 1, Apps: []appCandidate{{Name: "App", Path: `C:\App.exe`}}})
	if err != nil || len(r.Apps) != 1 || len(r.Warnings) != 1 {
		t.Fatalf("partial result lost: %+v, %v", r, err)
	}
	r, err = combineApps(proto.ListAppsArgs{Limit: 50}, appSourceResult{Completed: 1})
	if err != nil || r.Apps == nil || r.Warnings == nil {
		t.Fatalf("empty sources need arrays: %+v, %v", r, err)
	}
}

func TestAppsInvalidLimitAndCancellation(t *testing.T) {
	for _, limit := range []int{-1, 201} {
		args, _ := json.Marshal(proto.ListAppsArgs{Limit: limit})
		if _, _, err := listApps(context.Background(), args, nil); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := listApps(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: %v", err)
	}
}

// An 8.3 short path and the long path of the same executable have the same running key.
func TestRunningPathKeyShortAndLong(t *testing.T) {
	long := filepath.Join(t.TempDir(), "HyperHand long directory name", "Some Application.exe")
	if err := os.MkdirAll(filepath.Dir(long), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(long, nil, 0600); err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(long)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	short := windows.UTF16ToString(buf[:n])
	if err != nil || strings.EqualFold(short, long) {
		t.Skip("the volume has no 8.3 names")
	}
	if runningPathKey(short) != runningPathKey(long) {
		t.Errorf("short %q -> %q, long %q -> %q", short, runningPathKey(short), long, runningPathKey(long))
	}
	if missing := `C:\does not exist\x.exe`; runningPathKey(missing) != appPathKey(missing) {
		t.Errorf("missing path changed: %q", runningPathKey(missing))
	}
}

// Creates and reads only a temporary shortcut; the target is never launched. Its name, working directory and
// arguments hold characters outside every ANSI code page (Ŵ, an emoji) besides Chinese, so the round trip also holds on
// a guest whose code page cannot represent them.
func TestAppsShortcutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "中文 工作目录 Ŵ")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"with space", "中文", "Ŵ 😀", "", `a"b`, `C:\with space\`}
	quoted := make([]string, len(wantArgs))
	for i, arg := range wantArgs {
		quoted[i] = windows.EscapeArg(arg)
	}
	config := struct{ Link, Target, Arguments, Cwd string }{filepath.Join(dir, "测试 应用 Ŵ😀.lnk"), exe, strings.Join(quoted, " "), cwd}
	data, _ := json.Marshal(config)
	configPath := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	script := "param([string]$Config)\n$ErrorActionPreference = 'Stop'\nAdd-Type -TypeDefinition @'\n" + shellLinkCSharp + "\n'@\n" + `$c = Get-Content -LiteralPath $Config -Raw -Encoding UTF8 | ConvertFrom-Json
[HHShellLink]::Write($c.Link, $c.Target, $c.Arguments, $c.Cwd)
`
	scriptPath := filepath.Join(dir, "fixture.ps1")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-Config", configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, out)
	}
	source := readStartMenuApps(ctx, dir)
	if len(source.Warnings) != 0 || source.Completed != 1 || len(source.Apps) != 1 {
		t.Fatalf("shortcut source: %+v", source)
	}
	r, err := combineApps(proto.ListAppsArgs{Limit: 50}, source)
	if err != nil || len(r.Apps) != 1 {
		t.Fatalf("result: %+v, %v", r, err)
	}
	app := r.Apps[0]
	// The shell link stores the target's long path; exe is short when TEMP is (as on CI runners), so compare files.
	got, gotErr := os.Stat(app.Launch.Path)
	want, wantErr := os.Stat(exe)
	if app.Name != "测试 应用 Ŵ😀" || gotErr != nil || wantErr != nil || !os.SameFile(got, want) || app.Launch.Cwd != cwd || !reflect.DeepEqual(app.Launch.Args, wantArgs) {
		t.Fatalf("roundtrip changed launch: %+v", app)
	}
	r.Apps = append(r.Apps, proto.AppInfo{Launch: proto.AppLaunch{Path: filepath.Join(dir, filepath.Base(exe))}})
	if err := attachAppProcesses(ctx, r.Apps); err != nil {
		t.Log("process inspection warning:", err)
	}
	if !r.Apps[0].Running {
		t.Fatal("current test process was not matched by its full path")
	}
	if r.Apps[1].Running {
		t.Fatal("same basename in a different directory incorrectly matched a running process")
	}
}

func TestAppsDiscoverySmoke(t *testing.T) {
	raw, _, err := listApps(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := raw.(proto.ListAppsResult)
	if len(r.Apps) > 50 || r.Total < len(r.Apps) {
		t.Fatal("invalid discovery totals")
	}
	t.Logf("discovered %d matching apps; returned %d; warnings %d", r.Total, len(r.Apps), len(r.Warnings))
}

func TestPackagedAUMID(t *testing.T) {
	for id, want := range map[string]bool{
		"Microsoft.WindowsCalculator_8wekyb3d8bbwe!App":                                       true,
		"windows.immersivecontrolpanel_cw5n1h2txyewy!microsoft.windows.immersivecontrolpanel": true,
		"Microsoft.Windows.Explorer":                                                          false,
		`{6D809377-6AF0-444B-8957-A3773F02200E}\app!x.exe`:                                    false,
		`C:\Tools\a!b.exe`:     false,
		"!App":                 false,
		"Family_hash!":         false,
		"Family_hash!App!More": false,
		"Family hash!App":      false,
		"":                     false,
	} {
		if got := packagedAUMID(id); got != want {
			t.Errorf("packagedAUMID(%q) = %v, want %v", id, got, want)
		}
	}
}

// Packaged apps merge with desktop programs under the same sorting, query, deduplication and limit rules.
func TestAppsCombinePackaged(t *testing.T) {
	win32 := appSourceResult{Completed: 1, Apps: []appCandidate{{Name: "Editor", Path: `C:\Apps\edit.exe`}}}
	packaged := appSourceResult{Completed: 1, Apps: []appCandidate{
		{Name: "计算器", AUMID: "Microsoft.WindowsCalculator_8wekyb3d8bbwe!App"},
		{Name: "Calculator duplicate", AUMID: "microsoft.windowscalculator_8WEKYB3D8BBWE!app"},
		{Name: "Alarms", AUMID: "Microsoft.WindowsAlarms_8wekyb3d8bbwe!App"},
		{Name: "Not packaged", AUMID: "Microsoft.Windows.Explorer"},
	}}
	r, err := combineApps(proto.ListAppsArgs{Limit: 50}, win32, packaged)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range r.Apps {
		names = append(names, a.Name)
	}
	if r.Total != 3 || r.Truncated || !reflect.DeepEqual(names, []string{"Alarms", "Calculator duplicate", "Editor"}) {
		t.Fatalf("merged result: %+v", r)
	}
	calc := r.Apps[1]
	if calc.Launch.AUMID != "microsoft.windowscalculator_8WEKYB3D8BBWE!app" || calc.Launch.Path != "" || calc.Launch.Cwd != "" || calc.Launch.Args == nil || len(calc.Launch.Args) != 0 || calc.Windows == nil {
		t.Fatalf("packaged launch: %+v", calc)
	}
	if r.Apps[2].Launch.AUMID != "" || r.Apps[2].Launch.Path != `C:\Apps\edit.exe` {
		t.Fatalf("desktop launch changed: %+v", r.Apps[2])
	}
	// The same AUMID in any case has one stable ID; the display name and the AUMID are both searched.
	byName, err := combineApps(proto.ListAppsArgs{Query: "计算", Limit: 50}, packaged)
	if err != nil || byName.Total != 1 || byName.Apps[0].ID != calc.ID || byName.Apps[0].Name != "计算器" {
		t.Fatalf("display-name query: %+v, %v", byName, err)
	}
	byID, err := combineApps(proto.ListAppsArgs{Query: "WINDOWSALARMS_8", Limit: 50}, win32, packaged)
	if err != nil || byID.Total != 1 || byID.Apps[0].Name != "Alarms" {
		t.Fatalf("AUMID query: %+v, %v", byID, err)
	}
	limited, err := combineApps(proto.ListAppsArgs{Limit: 1}, packaged, win32)
	if err != nil || limited.Total != 3 || !limited.Truncated || len(limited.Apps) != 1 || limited.Apps[0].Name != "Alarms" {
		t.Fatalf("limit: %+v, %v", limited, err)
	}
	// A failed packaged source keeps the desktop results and reports a warning.
	failed := appSourceResult{Warnings: []string{"Get-StartApps: not available"}}
	partial, err := combineApps(proto.ListAppsArgs{Limit: 50}, win32, failed)
	if err != nil || partial.Total != 1 || !reflect.DeepEqual(partial.Warnings, failed.Warnings) {
		t.Fatalf("partial: %+v, %v", partial, err)
	}
}

// Reads the current user's packaged apps through Get-StartApps; nothing is launched.
func TestPackagedAppsDiscovery(t *testing.T) {
	source := packagedApps(context.Background())
	if source.Completed == 0 {
		t.Skipf("Get-StartApps is not available on this Windows edition: %v", source.Warnings)
	}
	r, err := combineApps(proto.ListAppsArgs{Limit: 200}, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Apps {
		if !packagedAUMID(a.Launch.AUMID) || a.Launch.Path != "" || a.Name == "" {
			t.Errorf("packaged entry %+v", a)
		}
	}
	if err := attachAppProcesses(context.Background(), r.Apps); err != nil {
		t.Log("process inspection warning:", err)
	}
	t.Logf("%d packaged apps, warnings %v", r.Total, source.Warnings)
}

// Only ApplicationFrameHost frames have a hosted PID, and it is never the frame's own process.
func TestHostedPID(t *testing.T) {
	for _, h := range topWindows() {
		var pid uint32
		windows.GetWindowThreadProcessId(h, &pid)
		class := className(h)
		got := hostedPID(h, class, pid)
		if class != "ApplicationFrameWindow" && got != 0 {
			t.Errorf("window %x of class %q has hosted PID %d", h, class, got)
		}
		if got != 0 && got == pid {
			t.Errorf("frame %x hosts its own PID", h)
		}
		if got != 0 {
			aumid := ""
			if p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, got); err == nil {
				aumid = processAUMID(p)
				windows.CloseHandle(p)
			}
			t.Logf("frame %x %q hosts PID %d (%s)", h, windowText(h), got, aumid)
		}
	}
}

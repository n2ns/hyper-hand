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

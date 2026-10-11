package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"hyperhand/internal/proto"
)

type appCandidate struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Arguments string `json:"arguments"`
	Cwd       string `json:"cwd"`
}

type appSourceResult struct {
	Apps      []appCandidate
	Warnings  []string
	Completed int
}

func listApps(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.ListAppsArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	if a.Limit == 0 {
		a.Limit = 50
	}
	if a.Limit < 1 || a.Limit > 200 {
		return nil, nil, errors.New("limit must be between 1 and 200")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	reg := appPaths(ctx)
	shortcuts := startMenuApps(ctx)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	r, err := combineApps(a, reg, shortcuts)
	if err != nil {
		return nil, nil, err
	}
	if err := attachAppProcesses(ctx, r.Apps); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		r.Warnings = append(r.Warnings, "running state incomplete: "+err.Error())
	}
	return r, nil, nil
}

// Launch identity includes case-sensitive arguments and the working directory: two shortcuts to the same exe can
// launch different profiles or documents. Sorting before deduplication gives a stable display name and result order.
func combineApps(a proto.ListAppsArgs, sources ...appSourceResult) (proto.ListAppsResult, error) {
	r := proto.ListAppsResult{Apps: []proto.AppInfo{}, Warnings: []string{}}
	var candidates []appCandidate
	completed := 0
	for _, source := range sources {
		candidates = append(candidates, source.Apps...)
		r.Warnings = append(r.Warnings, source.Warnings...)
		completed += source.Completed
	}
	if completed == 0 {
		return r, fmt.Errorf("application discovery failed: %s", strings.Join(r.Warnings, "; "))
	}
	sort.Slice(candidates, func(i, j int) bool {
		x, y := candidates[i], candidates[j]
		if strings.ToLower(x.Name) != strings.ToLower(y.Name) {
			return strings.ToLower(x.Name) < strings.ToLower(y.Name)
		}
		xb, _ := json.Marshal(x)
		yb, _ := json.Marshal(y)
		return string(xb) < string(yb)
	})
	seen := map[string]bool{}
	query := strings.ToLower(strings.TrimSpace(a.Query))
	for _, c := range candidates {
		if !filepath.IsAbs(c.Path) || !strings.EqualFold(filepath.Ext(c.Path), ".exe") {
			continue
		}
		argv, err := windows.DecomposeCommandLine("app.exe " + c.Arguments)
		if err != nil {
			r.Warnings = append(r.Warnings, "cannot parse shortcut arguments for "+c.Name)
			continue
		}
		argv = argv[1:]
		if argv == nil {
			argv = []string{}
		}
		identity, _ := json.Marshal([]any{appPathKey(c.Path), argv, appPathKey(c.Cwd)})
		hash := sha256.Sum256(identity)
		id := "app-" + hex.EncodeToString(hash[:16])
		// Match all aliases before deduplication, so searching the registry name still finds a named shortcut.
		if query != "" && !strings.Contains(strings.ToLower(c.Name), query) && !strings.Contains(strings.ToLower(c.Path), query) {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		r.Apps = append(r.Apps, proto.AppInfo{ID: id, Name: c.Name, Launch: proto.AppLaunch{Path: c.Path, Args: argv, Cwd: c.Cwd}, Windows: []proto.AppWindow{}})
	}
	r.Total = len(r.Apps)
	if len(r.Apps) > a.Limit {
		r.Truncated = true
		r.Apps = r.Apps[:a.Limit]
	}
	return r, nil
}

func appPathKey(path string) string {
	if path == "" {
		return ""
	}
	return strings.ToLower(filepath.Clean(strings.ReplaceAll(path, "/", `\`)))
}

// runningPathKey is appPathKey of path's long form, so that a process started from an 8.3 short path and a shortcut
// target stored in long form (or the reverse) match the same executable. Paths that cannot be expanded stay as given.
func runningPathKey(path string) string {
	if p, err := windows.UTF16PtrFromString(path); err == nil && path != "" {
		buf := make([]uint16, windows.MAX_LONG_PATH)
		if n, err := windows.GetLongPathName(p, &buf[0], uint32(len(buf))); err == nil && n > 0 && int(n) < len(buf) {
			path = windows.UTF16ToString(buf[:n])
		}
	}
	return appPathKey(path)
}

func appPaths(ctx context.Context) appSourceResult {
	r := appSourceResult{}
	for _, hive := range []struct {
		key  registry.Key
		name string
	}{{registry.CURRENT_USER, "HKCU"}, {registry.LOCAL_MACHINE, "HKLM"}} {
		for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
			if ctx.Err() != nil {
				return r
			}
			label := fmt.Sprintf("%s App Paths (view %d)", hive.name, view)
			key, err := registry.OpenKey(hive.key, `Software\Microsoft\Windows\CurrentVersion\App Paths`, registry.READ|view)
			if errors.Is(err, registry.ErrNotExist) {
				r.Completed++
				continue
			}
			if err != nil {
				r.Warnings = append(r.Warnings, label+": "+err.Error())
				continue
			}
			names, err := key.ReadSubKeyNames(-1)
			if err != nil {
				key.Close()
				r.Warnings = append(r.Warnings, label+": "+err.Error())
				continue
			}
			r.Completed++
			for _, name := range names {
				if ctx.Err() != nil {
					key.Close()
					return r
				}
				entry, err := registry.OpenKey(key, name, registry.QUERY_VALUE|view)
				if err != nil {
					r.Warnings = append(r.Warnings, label+"/"+name+": "+err.Error())
					continue
				}
				path, _, err := entry.GetStringValue("")
				entry.Close()
				if errors.Is(err, registry.ErrNotExist) {
					continue
				}
				if err != nil {
					r.Warnings = append(r.Warnings, label+"/"+name+": "+err.Error())
					continue
				}
				path, err = registry.ExpandString(strings.Trim(strings.TrimSpace(path), `"`))
				if err != nil {
					r.Warnings = append(r.Warnings, label+"/"+name+": "+err.Error())
					continue
				}
				// UNC targets are outside this local desktop discovery scope. Drive letters can still be mapped drives.
				if !localAppPath(path) {
					continue
				}
				if info, err := os.Stat(path); err == nil && !info.IsDir() {
					r.Apps = append(r.Apps, appCandidate{Name: strings.TrimSuffix(name, filepath.Ext(name)), Path: path})
				} else if err != nil && !errors.Is(err, os.ErrNotExist) {
					r.Warnings = append(r.Warnings, label+"/"+name+": "+err.Error())
				}
			}
			key.Close()
		}
	}
	return r
}

func localAppPath(path string) bool {
	return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') && filepath.IsAbs(path) && strings.EqualFold(filepath.Ext(path), ".exe")
}

// shellLinkCSharp reads and writes shortcuts through IShellLinkW and IPersistFile, which keep every string in
// Unicode. WScript.Shell converts paths and arguments through the system ANSI code page, so a shortcut whose name,
// target or arguments hold characters outside it (Chinese on an English guest, emoji anywhere) failed to load or came
// back as '?'. PowerShell compiles it with Add-Type.
const shellLinkCSharp = `
using System;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;
using System.Text;

public static class HHShellLink {
    [ComImport, Guid("000214F9-0000-0000-C000-000000000046"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    interface IShellLinkW {
        void GetPath([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder file, int cch, IntPtr findData, uint flags);
        void GetIDList(out IntPtr pidl);
        void SetIDList(IntPtr pidl);
        void GetDescription([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder name, int cch);
        void SetDescription([MarshalAs(UnmanagedType.LPWStr)] string name);
        void GetWorkingDirectory([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder dir, int cch);
        void SetWorkingDirectory([MarshalAs(UnmanagedType.LPWStr)] string dir);
        void GetArguments([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder args, int cch);
        void SetArguments([MarshalAs(UnmanagedType.LPWStr)] string args);
        void GetHotkey(out short hotkey);
        void SetHotkey(short hotkey);
        void GetShowCmd(out int showCmd);
        void SetShowCmd(int showCmd);
        void GetIconLocation([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder iconPath, int cch, out int icon);
        void SetIconLocation([MarshalAs(UnmanagedType.LPWStr)] string iconPath, int icon);
        void SetRelativePath([MarshalAs(UnmanagedType.LPWStr)] string pathRel, uint reserved);
        void Resolve(IntPtr hwnd, uint flags);
        void SetPath([MarshalAs(UnmanagedType.LPWStr)] string file);
    }

    [ComImport, Guid("00021401-0000-0000-C000-000000000046")]
    class ShellLink {}

    const int Size = 32768;

    // Read returns the target path, arguments and working directory of the shortcut at path.
    public static string[] Read(string path) {
        object link = new ShellLink();
        try {
            ((IPersistFile)link).Load(path, 0);
            IShellLinkW l = (IShellLinkW)link;
            StringBuilder target = new StringBuilder(Size), args = new StringBuilder(Size), dir = new StringBuilder(Size);
            l.GetPath(target, Size, IntPtr.Zero, 0);
            l.GetArguments(args, Size);
            l.GetWorkingDirectory(dir, Size);
            return new string[] { target.ToString(), args.ToString(), dir.ToString() };
        } finally { Marshal.FinalReleaseComObject(link); }
    }

    // Write creates the shortcut at path (the tests' fixture).
    public static void Write(string path, string target, string args, string dir) {
        object link = new ShellLink();
        try {
            IShellLinkW l = (IShellLinkW)link;
            l.SetPath(target);
            l.SetArguments(args);
            l.SetWorkingDirectory(dir);
            ((IPersistFile)link).Save(path, true);
        } finally { Marshal.FinalReleaseComObject(link); }
    }
}
`

// COM shortcut parsing is isolated in a bounded child process; a broken shell extension cannot wedge the agent.
// Each record is flushed independently, preserving completed sources if a later source fails or times out.
const appShortcutsScript = `
param([string]$Root)
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
function Emit($value) { [Console]::WriteLine(($value | ConvertTo-Json -Compress -Depth 4)) }
Add-Type -TypeDefinition @'
` + shellLinkCSharp + `
'@
$roots = @([Environment]::GetFolderPath('StartMenu'), [Environment]::GetFolderPath('CommonStartMenu'))
if ($Root) { $roots = @($Root) }
foreach ($folder in $roots) {
    try {
        $root = $folder
        if (!$root) { throw 'folder path unavailable' }
        if (Test-Path -LiteralPath $root) {
            $files = [Collections.Generic.List[IO.FileInfo]]::new()
            $pending = [Collections.Generic.Stack[string]]::new()
            $pending.Push($root)
            while ($pending.Count -gt 0) {
                $directory = $pending.Pop()
                try {
                    foreach ($entry in (Get-ChildItem -LiteralPath $directory -Force)) {
                        if (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
                        if ($entry.PSIsContainer) { $pending.Push($entry.FullName) }
                        elseif ($entry.Extension -ieq '.lnk') { $files.Add($entry) }
                    }
                } catch { Emit @{ warning = ($directory + ': ' + $_.Exception.Message) } }
            }
            foreach ($file in $files) {
                try {
                    $link = [HHShellLink]::Read($file.FullName)
                    $target = [Environment]::ExpandEnvironmentVariables($link[0])
                    if ($target -match '^[A-Za-z]:[\\/]' -and [IO.Path]::GetExtension($target) -ieq '.exe' -and (Test-Path -LiteralPath $target -PathType Leaf)) {
                        Emit @{ app = @{ name = $file.BaseName; path = $target; arguments = $link[1]; cwd = [Environment]::ExpandEnvironmentVariables($link[2]) } }
                    }
                } catch { Emit @{ warning = ($folder + '/' + $file.Name + ': ' + $_.Exception.Message) } }
            }
        }
        Emit @{ completed = 1 }
    } catch { Emit @{ warning = ($folder + ': ' + $_.Exception.Message) } }
}
`

func startMenuApps(ctx context.Context) appSourceResult {
	return readStartMenuApps(ctx, "")
}

func readStartMenuApps(ctx context.Context, root string) appSourceResult {
	r := appSourceResult{}
	f, err := os.CreateTemp("", "hyperhand-apps-*.ps1")
	if err != nil {
		r.Warnings = append(r.Warnings, "Start Menu: "+err.Error())
		return r
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(appShortcutsScript)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		r.Warnings = append(r.Warnings, "Start Menu: "+err.Error())
		return r
	}
	opctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(opctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", f.Name())
	if root != "" {
		cmd.Args = append(cmd.Args, "-Root", root)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.WaitDelay = time.Second
	var out controlsBuffer
	cmd.Stdout = &out
	err = cmd.Run()
	scanner := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var record struct {
			App       *appCandidate `json:"app"`
			Warning   string        `json:"warning"`
			Completed int           `json:"completed"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			r.Warnings = append(r.Warnings, "Start Menu: invalid helper output")
			continue
		}
		if record.App != nil {
			r.Apps = append(r.Apps, *record.App)
		}
		if record.Warning != "" {
			r.Warnings = append(r.Warnings, record.Warning)
		}
		r.Completed += record.Completed
	}
	if scanner.Err() != nil {
		r.Warnings = append(r.Warnings, "Start Menu: "+scanner.Err().Error())
	}
	if opctx.Err() != nil {
		err = opctx.Err()
	}
	if err != nil {
		r.Warnings = append(r.Warnings, "Start Menu: "+err.Error())
	}
	return r
}

func attachAppProcesses(ctx context.Context, apps []proto.AppInfo) error {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		return err
	}
	var buf unsafe.Pointer
	var n uint32
	if r, _, err := pWTSEnumerateProcessesW.Call(0, 0, 1, uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n))); r == 0 {
		return err
	}
	defer pWTSFreeMemory.Call(uintptr(buf))
	paths := map[uint32]string{}
	unreadable := 0
	for _, p := range unsafe.Slice((*wtsProcessInfo)(buf), n) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.SessionID != session || p.ProcessID == 0 {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p.ProcessID)
		if err != nil {
			unreadable++
			continue
		}
		name := make([]uint16, windows.MAX_LONG_PATH)
		size := uint32(len(name))
		r, _, _ := pQueryFullProcessImageName.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&size)))
		windows.CloseHandle(h)
		if r != 0 {
			paths[p.ProcessID] = runningPathKey(windows.UTF16ToString(name[:size]))
		} else {
			unreadable++
		}
	}
	byPath := map[string][]proto.AppWindow{}
	for _, path := range paths {
		if _, ok := byPath[path]; !ok {
			byPath[path] = []proto.AppWindow{}
		}
	}
	for _, h := range topWindows() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pid uint32
		windows.GetWindowThreadProcessId(h, &pid)
		if path, ok := paths[pid]; ok {
			byPath[path] = append(byPath[path], proto.AppWindow{Handle: uint64(h), PID: pid, Title: windowText(h)})
		}
	}
	for i := range apps {
		if ws, ok := byPath[runningPathKey(apps[i].Launch.Path)]; ok {
			apps[i].Running = true
			apps[i].Windows = ws
		}
	}
	if unreadable > 0 {
		return fmt.Errorf("could not inspect %d processes in the current session", unreadable)
	}
	return nil
}

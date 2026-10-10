package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

func runFileInfo(t *testing.T, ctx context.Context, paths ...string) []proto.FileInfo {
	t.Helper()
	args, _ := json.Marshal(proto.PathsArgs{Paths: paths})
	r, _, err := fileInfo(ctx, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := r.(proto.FileInfoResult).Files
	if len(files) != len(paths) {
		t.Fatalf("%d entries for %d paths", len(files), len(paths))
	}
	return files
}

func TestFileInfoFileDirMissingAndEnv(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello file_info")
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(file, mod, mod); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HH_FILEINFO_DIR", dir)
	files := runFileInfo(t, context.Background(), `%HH_FILEINFO_DIR%\a.txt`, dir, filepath.Join(dir, "missing.txt"), empty)

	sum := sha256.Sum256(content)
	f := files[0]
	if f.Path != `%HH_FILEINFO_DIR%\a.txt` || f.Resolved != file || !f.Exists || f.Type != "file" || f.Size == nil || *f.Size != int64(len(content)) ||
		f.SHA256 != hex.EncodeToString(sum[:]) || f.Version != "" || f.Modified != "2026-01-02T03:04:05Z" || f.Error != "" {
		t.Errorf("file: %+v", f)
	}
	d := files[1]
	if !d.Exists || d.Type != "dir" || d.Size != nil || d.SHA256 != "" || d.Modified == "" || d.Error != "" {
		t.Errorf("dir: %+v", d)
	}
	m := files[2]
	if m.Exists || m.Type != "" || m.Error != "" || m.Modified != "" {
		t.Errorf("missing: %+v", m)
	}
	e := files[3]
	emptySum := sha256.Sum256(nil)
	if !e.Exists || e.Size == nil || *e.Size != 0 || e.SHA256 != hex.EncodeToString(emptySum[:]) {
		t.Errorf("empty file: %+v", e)
	}
	// The JSON keeps size 0 and omits the fields that do not apply.
	b, _ := json.Marshal(proto.FileInfoResult{Files: files})
	if !strings.Contains(string(b), `"size":0`) || strings.Count(string(b), `"sha256"`) != 2 || strings.Contains(string(b), `"version"`) {
		t.Errorf("json: %s", b)
	}
}

func TestFileInfoProductVersion(t *testing.T) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	f := runFileInfo(t, context.Background(), filepath.Join(sys, "kernel32.dll"))[0]
	if !f.Exists || f.Version == "" || !strings.Contains(f.Version, ".") || len(f.SHA256) != 64 {
		t.Fatalf("kernel32.dll: %+v", f)
	}
}

func TestFileInfoErrors(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.bin")
	if err := os.WriteFile(locked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(locked)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0) // no sharing: others cannot open it
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := fileInfoHashLimit
	fileInfoHashLimit = 5
	defer func() { fileInfoHashLimit = old }()
	files := runFileInfo(t, context.Background(), locked, big, `relative\file.txt`)
	if f := files[0]; !f.Exists || f.Type != "file" || f.SHA256 != "" || f.Error == "" {
		t.Errorf("locked: %+v", f)
	}
	if f := files[1]; !f.Exists || f.Size == nil || *f.Size != 10 || f.SHA256 != "" || !strings.Contains(f.Error, "larger") {
		t.Errorf("big: %+v", f)
	}
	if f := files[2]; f.Exists || !strings.Contains(f.Error, "absolute") {
		t.Errorf("relative: %+v", f)
	}
}

func TestFileInfoArgsAndCancel(t *testing.T) {
	for _, n := range []int{0, proto.MaxFileInfoPaths + 1} {
		args, _ := json.Marshal(proto.PathsArgs{Paths: make([]string, n)})
		if _, _, err := fileInfo(context.Background(), args, nil); err == nil {
			t.Errorf("accepted %d paths", n)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	args, _ := json.Marshal(proto.PathsArgs{Paths: []string{os.TempDir()}})
	if _, _, err := fileInfo(ctx, args, nil); err == nil {
		t.Error("cancelled request succeeded")
	}
}

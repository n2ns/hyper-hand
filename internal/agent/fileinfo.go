package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

// fileInfoHashLimit is the largest file file_info hashes; a variable so tests can lower it.
var fileInfoHashLimit int64 = 1 << 30

// fileInfo reports existence, type, size, SHA-256, PE ProductVersion and modification time of each path, after
// expanding environment variables in this process's (the logged-on user's) environment.
func fileInfo(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.PathsArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	if len(a.Paths) == 0 || len(a.Paths) > proto.MaxFileInfoPaths {
		return nil, nil, fmt.Errorf("pass 1 to %d paths, got %d", proto.MaxFileInfoPaths, len(a.Paths))
	}
	res := proto.FileInfoResult{Files: make([]proto.FileInfo, len(a.Paths))}
	buf := make([]byte, 1<<20)
	for i, p := range a.Paths {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		res.Files[i] = describeFile(ctx, p, buf)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return res, nil, nil
}

func describeFile(ctx context.Context, path string, buf []byte) proto.FileInfo {
	info := proto.FileInfo{Path: path, Resolved: path}
	resolved, err := expandEnv(path)
	if err != nil {
		info.Error = "expand environment variables: " + err.Error()
		return info
	}
	info.Resolved = resolved
	if !filepath.IsAbs(resolved) {
		info.Error = "not an absolute path after expanding environment variables; pass a full path such as C:\\dir\\file or %APPDATA%\\dir\\file"
		return info
	}
	info.Resolved = filepath.Clean(resolved)
	st, err := os.Stat(info.Resolved)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			info.Error = err.Error()
		}
		return info
	}
	info.Exists = true
	info.Modified = st.ModTime().UTC().Format(time.RFC3339)
	if st.IsDir() {
		info.Type = "dir"
		return info
	}
	info.Type = "file"
	size := st.Size()
	info.Size = &size
	info.Version = productVersion(info.Resolved)
	if size > fileInfoHashLimit {
		info.Error = fmt.Sprintf("file is larger than %d bytes; sha256 not computed", fileInfoHashLimit)
		return info
	}
	sum, err := sha256File(ctx, info.Resolved, buf)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.SHA256 = sum
	return info
}

func sha256File(ctx context.Context, path string, buf []byte) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, ctxReader{ctx, f}, buf); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// expandEnv expands %NAME% references with ExpandEnvironmentStrings; unknown names stay as written.
func expandEnv(s string) (string, error) {
	src, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, 512)
	for {
		n, err := windows.ExpandEnvironmentStrings(src, &buf[0], uint32(len(buf)))
		if err != nil {
			return "", err
		}
		if int(n) <= len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n)
	}
}

// productVersion returns the ProductVersion string of path's version resource, or "" when it has none.
func productVersion(path string) string {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return ""
	}
	block := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&block[0])); err != nil {
		return ""
	}
	// The string tables are keyed by language and code page: the translations the resource lists, then US English
	// in Unicode and Windows-1252 for resources without a translation table.
	var keys []string
	var ptr unsafe.Pointer
	var n uint32
	if windows.VerQueryValue(unsafe.Pointer(&block[0]), `\VarFileInfo\Translation`, unsafe.Pointer(&ptr), &n) == nil && n >= 4 {
		pairs := unsafe.Slice((*uint16)(ptr), n/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			keys = append(keys, fmt.Sprintf("%04x%04x", pairs[i], pairs[i+1]))
		}
	}
	keys = append(keys, "040904b0", "040904e4")
	for _, k := range keys {
		var s unsafe.Pointer
		var l uint32
		if windows.VerQueryValue(unsafe.Pointer(&block[0]), `\StringFileInfo\`+k+`\ProductVersion`, unsafe.Pointer(&s), &l) != nil || l == 0 {
			continue
		}
		if v := windows.UTF16ToString(unsafe.Slice((*uint16)(s), l)); v != "" {
			return v
		}
	}
	return ""
}

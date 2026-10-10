package proto

// OpFileInfo reads facts about guest paths without changing anything: PathsArgs -> FileInfoResult.
const OpFileInfo = "file_info"

// MaxFileInfoPaths is the most paths one file_info request may name.
const MaxFileInfoPaths = 64

// FileInfo describes one requested path. Path is as given; Resolved is it with environment variables expanded in
// the agent's user context. Type is "file" or "dir" when Exists. Size and SHA256 (lowercase hex) are for files only;
// Version is the PE ProductVersion string of a file with a version resource. Modified is RFC 3339 UTC. Error explains
// a fact that could not be read (an unreadable or too large file, a relative path); the other fields stay valid.
type FileInfo struct {
	Path     string `json:"path"`
	Resolved string `json:"resolved"`
	Exists   bool   `json:"exists"`
	Type     string `json:"type,omitempty"`
	Size     *int64 `json:"size,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Version  string `json:"version,omitempty"`
	Modified string `json:"modified,omitempty"`
	Error    string `json:"error,omitempty"`
}

// FileInfoResult has one entry per requested path, in order.
type FileInfoResult struct {
	Files []FileInfo `json:"files"`
}

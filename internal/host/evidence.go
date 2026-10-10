package host

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows"

	"hyperhand/internal/credential"
	"hyperhand/internal/proto"
)

// Journal limits per task: the oldest calls, and the oldest screenshots, are dropped beyond them (and counted).
const (
	journalMaxEntries    = 2000
	journalMaxText       = 256 << 10
	journalMaxImageBytes = 128 << 20
)

// minRedact is the shortest secret vm_evidence redacts.
const minRedact = 4

// journalSeqKey carries the journal sequence number of the call in progress, so calls made inside it (vm_batch
// steps) record it as their parent.
type journalSeqKey struct{}

// journalEntry is one recorded tool call: its arguments without task_id, the final result's text item (truncated
// beyond journalMaxText) and its PNG images.
type journalEntry struct {
	Seq, Parent int
	Tool, VM    string
	Args        json.RawMessage
	Started     time.Time
	Elapsed     time.Duration
	IsError     bool
	Text        string
	Truncated   bool
	Images      [][]byte
}

// taskJournal records a task's tool calls for vm_evidence. The zero value is ready; it lives and ends with the task.
type taskJournal struct {
	mu                          sync.Mutex
	next                        int
	entries                     []journalEntry
	imageBytes                  int
	droppedCalls, droppedImages int
}

// begin starts recording a call; the returned context carries its sequence number and end records the final result.
func (j *taskJournal) begin(ctx context.Context, tool, vm string, args json.RawMessage) (context.Context, func(*mcp.CallToolResult)) {
	j.mu.Lock()
	j.next++
	seq := j.next
	j.mu.Unlock()
	parent, _ := ctx.Value(journalSeqKey{}).(int)
	started := time.Now()
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) == nil && m != nil {
		delete(m, "task_id")
		args, _ = json.Marshal(m)
	}
	return context.WithValue(ctx, journalSeqKey{}, seq), func(r *mcp.CallToolResult) {
		e := journalEntry{Seq: seq, Parent: parent, Tool: tool, VM: vm, Args: args, Started: started, Elapsed: time.Since(started)}
		if r != nil {
			e.IsError = r.IsError
			for _, c := range r.Content {
				switch c := c.(type) {
				case *mcp.TextContent:
					e.Text = c.Text
				case *mcp.ImageContent:
					if tool != "vm_batch" { // a batch's images are its steps', recorded with them
						e.Images = append(e.Images, c.Data)
					}
				}
			}
		}
		if len(e.Text) > journalMaxText {
			e.Text, e.Truncated = shrinkJSON(e.Text, journalMaxText), true
		}
		j.add(e)
	}
}

// shrinkJSON fits a JSON text into max bytes by shortening its long strings (control trees, command output), so
// that the structure (statuses, assertions, file hashes) stays readable; text that is not JSON, or still too long,
// is cut at max bytes.
func shrinkJSON(text string, max int) string {
	var v any
	if json.Unmarshal([]byte(text), &v) == nil {
		for _, keep := range []int{4096, 512, 64} {
			b, err := json.Marshal(walkStrings(v, func(s string) string {
				if len(s) <= keep {
					return s
				}
				return s[:keep] + fmt.Sprintf("…[truncated %d bytes]", len(s)-keep)
			}))
			if err == nil && len(b) <= max {
				return string(b)
			}
		}
	}
	return text[:max]
}

// walkStrings returns a copy of the decoded JSON value v with every string, map keys included, replaced by f.
func walkStrings(v any, f func(string) string) any {
	switch v := v.(type) {
	case string:
		return f(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[f(k)] = walkStrings(x, f)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = walkStrings(x, f)
		}
		return out
	}
	return v
}

func (j *taskJournal) add(e journalEntry) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, e)
	for _, img := range e.Images {
		j.imageBytes += len(img)
	}
	for len(j.entries) > journalMaxEntries {
		for _, img := range j.entries[0].Images {
			j.imageBytes -= len(img)
			j.droppedImages++
		}
		j.entries = j.entries[1:]
		j.droppedCalls++
	}
	for i := 0; j.imageBytes > journalMaxImageBytes && i < len(j.entries); i++ {
		for _, img := range j.entries[i].Images {
			j.imageBytes -= len(img)
			j.droppedImages++
		}
		j.entries[i].Images = nil
	}
}

// snapshot returns the recorded calls in sequence order and the drop counters.
func (j *taskJournal) snapshot() ([]journalEntry, int, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := append([]journalEntry(nil), j.entries...)
	sort.Slice(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	return out, j.droppedCalls, j.droppedImages
}

type evidenceIn struct {
	VM     string   `json:"vm,omitempty"`
	Title  string   `json:"title,omitempty" jsonschema:"what the run verified, e.g. 'AutoCAD plugin install on Win10'; shown at the top of the report"`
	Dir    string   `json:"dir,omitempty" jsonschema:"absolute host directory for the zip (created if missing); default %LOCALAPPDATA%\\HyperHand\\evidence"`
	Redact []string `json:"redact,omitempty" jsonschema:"strings to replace with [redacted] in every argument and result, e.g. license keys or passwords you passed to vm_exec or vm_type; the VM's stored unlock password is always redacted. Screenshots are not redacted."`
	Files  []string `json:"files,omitempty" jsonschema:"up to 64 guest paths to hash now (as vm_file_info) and include, e.g. the deployed files the run verified"`
}

// evidenceSecrets returns the strings always redacted for vm: its stored unlock password. Tests replace it.
var evidenceSecrets = func(vm string) []string {
	if _, pw, ok, err := credential.Read(vm); err == nil && ok && pw != "" {
		return []string{pw}
	}
	return nil
}

const evidenceDesc = "Export this task's work on one VM as a reviewable acceptance-evidence zip on the host: evidence.json and report.md with the host and agent versions and executable hashes, the VM, its checkpoint and the agent's user, every tool call of this task on the VM in order (arguments, results or errors, timing; vm_batch steps nested under their batch), the assertions of vm_batch steps and of vm_wait assert, file hashes from vm_file_info calls and from files, and the screenshots as PNG files. " +
	"Only this task's calls on this VM are included, recorded since the task began (up to 2000 calls and 128 MiB of screenshots; older ones are dropped and counted). The VM's stored unlock password and the strings in redact are replaced with [redacted] in all text when they have at least 4 characters; shorter ones would garble the record, so they are left and counted in skipped_short_secrets. Screenshots are not redacted. Call it before vm_end_turn, which ends the task and its record. " +
	"Result: {path, sha256, bytes, calls, screenshots, assertions: {passed, failed, not_evaluated}, files, redactions, skipped_short_secrets, omitted: {calls, screenshots}}."

func registerEvidence(d *deps) {
	addToolIn(d, toolSpec{name: "vm_evidence", desc: evidenceDesc, readOnly: true}, func(ctx context.Context, in evidenceIn) (*mcp.CallToolResult, error) {
		task, _ := ctx.Value(taskContextKey{}).(*taskState)
		if task == nil {
			return nil, refuse(codeFailed, "pass task_id on every call of the work, then call vm_evidence with the same task_id", nil, "this call has no task, so there is no record to export")
		}
		if len(in.Files) > proto.MaxFileInfoPaths {
			return nil, refuse(codeInvalidArgument, "pass at most 64 files", map[string]any{"files": len(in.Files)}, "files holds %d paths", len(in.Files))
		}
		dir := in.Dir
		if dir == "" {
			dir = filepath.Join(os.Getenv("LOCALAPPDATA"), "HyperHand", "evidence")
		} else if !filepath.IsAbs(dir) {
			return nil, refuse(codeInvalidArgument, "pass an absolute host directory, or omit dir", nil, "dir %q is not absolute", dir)
		}
		v, err := d.raw.Find(in.VM)
		if err != nil {
			return nil, vmErr(err)
		}
		// A secret shorter than minRedact would replace common letters and digits everywhere and garble the record, so it
		// is skipped and reported instead.
		var secrets []string
		short := 0
		for _, s := range append(evidenceSecrets(v.Name), in.Redact...) {
			switch n := len([]rune(s)); {
			case n == 0:
			case n < minRedact:
				short++
			default:
				secrets = append(secrets, s)
			}
		}
		red := &redactor{secrets: secrets}
		entries, droppedCalls, droppedImages := task.journal.snapshot()
		ev := evidence{
			Title:     in.Title,
			CreatedAt: time.Now().Format(time.RFC3339),
			TaskID:    task.id,
			RunID:     task.runID,
			Host:      hostFacts(),
			VM:        evidenceVM{Name: v.Name, ID: v.ID, State: powerState(v.State)},
			Steps:     []evidenceStep{},
			Asserts:   []evidenceAssert{},
			Files:     []evidenceFile{},
			Omitted:   evidenceOmitted{Calls: droppedCalls, Screenshots: droppedImages},
		}
		if l, err := d.raw.ListCheckpoints(v.Name); err == nil {
			ev.VM.CheckpointType = l.CheckpointType
			for _, c := range l.Checkpoints {
				if c.ID == l.CurrentParentID {
					ref := checkpointRefOf(c)
					ev.VM.CurrentCheckpoint = &ref
				}
			}
		}
		if v.State == "Running" {
			ev.Agent = agentFacts(ctx, d, v.Name)
		}
		var shots []evidenceShot
		for _, e := range entries {
			if !strings.EqualFold(e.VM, v.Name) && !strings.EqualFold(e.VM, in.VM) {
				continue
			}
			s := evidenceStep{Seq: e.Seq, Parent: e.Parent, Tool: e.Tool, StartedAt: e.Started.Format(time.RFC3339Nano), ElapsedMS: e.Elapsed.Milliseconds(), OK: !e.IsError, Truncated: e.Truncated}
			s.Args = decodeJSON(string(e.Args))
			body := decodeJSON(e.Text)
			if e.IsError {
				s.Error = body
			} else {
				s.Result = body
			}
			for i, img := range e.Images {
				name := fmt.Sprintf("screenshots/%04d-%s-%d.png", e.Seq, e.Tool, i+1)
				s.Screenshots = append(s.Screenshots, name)
				shots = append(shots, evidenceShot{name, img})
			}
			ev.Steps = append(ev.Steps, s)
			ev.Asserts = append(ev.Asserts, stepAssertions(s)...)
			if e.Tool == "vm_file_info" && !e.IsError {
				ev.Files = append(ev.Files, stepFiles(s)...)
			}
		}
		if len(in.Files) > 0 {
			var fi proto.FileInfoResult
			if _, err := d.call(ctx, v.Name, proto.OpFileInfo, proto.PathsArgs{Paths: in.Files}, nil, &fi); err != nil {
				return nil, agentErr(err)
			}
			for _, f := range fi.Files {
				ev.Files = append(ev.Files, evidenceFile{Source: "export", FileInfo: f})
			}
		}
		// One pass over everything that goes into the zip: decoded strings, so JSON escapes cannot hide a secret.
		if err := red.evidence(&ev); err != nil {
			return nil, err
		}
		ev.Redactions, ev.SkippedShortSecrets = red.count, short
		path, sum, size, err := writeEvidence(dir, fmt.Sprintf("evidence-%s-%s-%s.zip", safeName(ev.VM.Name), safeName(ev.TaskID), time.Now().Format("20060102-150405")), &ev, shots)
		if err != nil {
			return nil, refuse(codeFailed, "pass dir with a writable absolute host directory", nil, "writing the evidence zip failed: %v", err)
		}
		passed, failed, open := 0, 0, 0
		for _, a := range ev.Asserts {
			switch {
			case a.Passed == nil:
				open++
			case *a.Passed:
				passed++
			default:
				failed++
			}
		}
		return jsonResult(map[string]any{
			"path": path, "sha256": sum, "bytes": size, "calls": len(ev.Steps), "screenshots": len(shots),
			"assertions": map[string]int{"passed": passed, "failed": failed, "not_evaluated": open},
			"files":      len(ev.Files), "redactions": ev.Redactions, "skipped_short_secrets": short,
			"omitted": map[string]int{"calls": droppedCalls, "screenshots": droppedImages},
		})
	})
}

type evidence struct {
	Title      string           `json:"title"`
	CreatedAt  string           `json:"created_at"`
	TaskID     string           `json:"task_id"`
	RunID      string           `json:"run_id"`
	Host       evidenceHost     `json:"host"`
	VM         evidenceVM       `json:"vm"`
	Agent      map[string]any   `json:"agent"`
	Steps      []evidenceStep   `json:"steps"`
	Asserts    []evidenceAssert `json:"assertions"`
	Files      []evidenceFile   `json:"files"`
	Redactions int              `json:"redactions"` // replacements made; screenshots are not redacted
	// SkippedShortSecrets counts secrets (the stored unlock password, redact entries) shorter than minRedact
	// characters, which were not redacted.
	SkippedShortSecrets int             `json:"skipped_short_secrets"`
	Omitted             evidenceOmitted `json:"omitted"`
}

type evidenceHost struct {
	Version     string            `json:"version"`
	Protocol    int               `json:"protocol"`
	OS          string            `json:"os"`
	Executables map[string]string `json:"executables"` // file name -> SHA-256 of the running host's directory
}

type evidenceVM struct {
	Name              string         `json:"name"`
	ID                string         `json:"id"`
	State             string         `json:"state"`
	CheckpointType    string         `json:"checkpoint_type,omitempty"`
	CurrentCheckpoint *checkpointRef `json:"current_checkpoint"`
}

type evidenceStep struct {
	Seq         int      `json:"seq"`
	Parent      int      `json:"parent,omitempty"` // the vm_batch call this step ran in
	Tool        string   `json:"tool"`
	StartedAt   string   `json:"started_at"`
	ElapsedMS   int64    `json:"elapsed_ms"`
	OK          bool     `json:"ok"`
	Args        any      `json:"args"`
	Result      any      `json:"result,omitempty"`
	Error       any      `json:"error,omitempty"`
	Screenshots []string `json:"screenshots,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
}

// evidenceAssert is one assertion: a vm_batch step's (Step, Condition) or a vm_wait assert. Passed is null when the
// batch stopped before it was evaluated.
type evidenceAssert struct {
	Seq       int    `json:"seq"`
	Tool      string `json:"tool"`
	Step      *int   `json:"step,omitempty"`
	StepTool  string `json:"step_tool,omitempty"`
	Condition any    `json:"condition"`
	Passed    *bool  `json:"passed"`
	Actual    any    `json:"actual,omitempty"`
}

type evidenceFile struct {
	Source string `json:"source"` // "call <seq>" (a vm_file_info call) or "export" (vm_evidence files)
	proto.FileInfo
}

type evidenceOmitted struct {
	Calls       int `json:"calls"`
	Screenshots int `json:"screenshots"`
}

type evidenceShot struct {
	name string
	png  []byte
}

// redactor replaces secrets in decoded strings and counts the replacements.
type redactor struct {
	secrets []string
	count   int
}

func (r *redactor) text(s string) string {
	for _, sec := range r.secrets {
		if n := strings.Count(s, sec); n > 0 {
			r.count += n
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	return s
}

// evidence redacts every string of ev (map keys included) through a JSON round trip.
func (r *redactor) evidence(ev *evidence) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if b, err = json.Marshal(walkStrings(v, r.text)); err != nil {
		return err
	}
	*ev = evidence{}
	return json.Unmarshal(b, ev)
}

// decodeJSON decodes s as JSON, or returns s itself when it is not JSON.
func decodeJSON(s string) any {
	var v any
	if s != "" && json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}

// stepAssertions extracts the assertions of a vm_batch call (from its step arguments and outcome) or a vm_wait assert.
func stepAssertions(s evidenceStep) []evidenceAssert {
	args, _ := s.Args.(map[string]any)
	switch s.Tool {
	case "vm_wait":
		if a, _ := args["assert"].(bool); !a {
			return nil
		}
		passed := s.OK
		cond := map[string]any{}
		for k, v := range args {
			if k != "vm" && k != "assert" {
				cond[k] = v
			}
		}
		out := evidenceAssert{Seq: s.Seq, Tool: s.Tool, Condition: cond, Passed: &passed}
		if e, _ := s.Error.(map[string]any); e != nil && e["error"] != "assertion_failed" {
			out.Passed = nil // the wait failed for another reason; the condition was not decided
		}
		return []evidenceAssert{out}
	case "vm_batch":
	default:
		return nil
	}
	body, _ := s.Result.(map[string]any)
	if body == nil {
		body, _ = s.Error.(map[string]any)
	}
	steps, _ := args["steps"].([]any)
	var ran []any
	if body != nil {
		ran, _ = body["steps"].([]any)
	}
	failedAssert, _ := body["failed_assertion"].(map[string]any)
	var out []evidenceAssert
	for i, st := range steps {
		st, _ := st.(map[string]any)
		asserts, _ := st["assert"].([]any)
		if len(asserts) == 0 {
			continue
		}
		var done map[string]any
		if i < len(ran) {
			done, _ = ran[i].(map[string]any)
		}
		passedN := 0
		if done != nil {
			if n, ok := done["assertions_passed"].(float64); ok {
				passedN = int(n)
			}
		}
		tool, _ := st["tool"].(string)
		for k, a := range asserts {
			step := i
			ea := evidenceAssert{Seq: s.Seq, Tool: s.Tool, Step: &step, StepTool: tool, Condition: a}
			switch {
			case done == nil:
			case k < passedN:
				t := true
				ea.Passed = &t
			case done["ok"] == false && done["result"] != nil && k == passedN && failedAssert != nil:
				f := false
				ea.Passed = &f
				ea.Actual = failedAssert["actual"]
			}
			out = append(out, ea)
		}
	}
	return out
}

// stepFiles returns the files of a vm_file_info call's result.
func stepFiles(s evidenceStep) []evidenceFile {
	body, _ := s.Result.(map[string]any)
	b, _ := json.Marshal(body["files"])
	var files []proto.FileInfo
	_ = json.Unmarshal(b, &files)
	out := make([]evidenceFile, 0, len(files))
	for _, f := range files {
		out = append(out, evidenceFile{Source: fmt.Sprintf("call %d", s.Seq), FileInfo: f})
	}
	return out
}

func hostFacts() evidenceHost {
	h := evidenceHost{Version: proto.Version, Protocol: proto.Protocol, Executables: map[string]string{}}
	if v := windows.RtlGetVersion(); v != nil {
		h.OS = fmt.Sprintf("Windows %d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	}
	if exe, err := os.Executable(); err == nil {
		for _, name := range []string{filepath.Base(exe), "hyperhand-agent.exe"} {
			if sum, err := fileSHA256(filepath.Join(filepath.Dir(exe), name)); err == nil {
				h.Executables[name] = sum
			}
		}
	}
	return h
}

func agentFacts(ctx context.Context, d *deps, vm string) map[string]any {
	c, err := d.m.Client(vm)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var p proto.PingResult
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.TryCall(pctx, proto.OpPing, &p); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"version": p.Version, "protocol": p.Protocol, "hostname": p.Hostname, "user": p.User}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string { return strings.Trim(unsafeName.ReplaceAllString(s, "_"), "_") }

// writeEvidence writes the zip (evidence.json, report.md, screenshots) to dir/name through a temporary file and
// returns its path, SHA-256 and size.
func writeEvidence(dir, name string, ev *evidence, shots []evidenceShot) (string, string, int64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", 0, err
	}
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return "", "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	z := zip.NewWriter(io.MultiWriter(tmp, h))
	put := func(name string, b []byte) error {
		w, err := z.Create(name)
		if err == nil {
			_, err = w.Write(b)
		}
		return err
	}
	js, _ := json.MarshalIndent(ev, "", "  ")
	err = put("evidence.json", js)
	if err == nil {
		err = put("report.md", []byte(evidenceReport(ev)))
	}
	for _, s := range shots {
		if err == nil {
			err = put(s.name, s.png)
		}
	}
	if err == nil {
		err = z.Close()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", 0, err
	}
	path := filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", "", 0, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", "", 0, err
	}
	return path, hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

// evidenceReport renders the human-readable summary of ev.
func evidenceReport(ev *evidence) string {
	var b strings.Builder
	title := ev.Title
	if title == "" {
		title = "HyperHand acceptance evidence"
	}
	fmt.Fprintf(&b, "# %s\n\nCreated %s. Task `%s`, run `%s`.\n\n## Environment\n\n", mdCell(title), ev.CreatedAt, ev.TaskID, ev.RunID)
	fmt.Fprintf(&b, "| Item | Value |\n|---|---|\n| Host | HyperHand %s (protocol %d), %s |\n", ev.Host.Version, ev.Host.Protocol, ev.Host.OS)
	names := make([]string, 0, len(ev.Host.Executables))
	for n := range ev.Host.Executables {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "| `%s` SHA-256 | `%s` |\n", n, ev.Host.Executables[n])
	}
	fmt.Fprintf(&b, "| VM | %s (`%s`), %s |\n", mdCell(ev.VM.Name), ev.VM.ID, ev.VM.State)
	if ev.VM.CurrentCheckpoint != nil {
		fmt.Fprintf(&b, "| Current checkpoint | %s (`%s`), setting %s |\n", mdCell(ev.VM.CurrentCheckpoint.Name), ev.VM.CurrentCheckpoint.ID, ev.VM.CheckpointType)
	}
	if ev.Agent != nil {
		if e, ok := ev.Agent["error"]; ok {
			fmt.Fprintf(&b, "| Agent | not answering: %s |\n", mdCell(fmt.Sprint(e)))
		} else {
			fmt.Fprintf(&b, "| Agent | %v (protocol %v), user %s on %s |\n", ev.Agent["version"], ev.Agent["protocol"], mdCell(fmt.Sprint(ev.Agent["user"])), mdCell(fmt.Sprint(ev.Agent["hostname"])))
		}
	}
	fmt.Fprintf(&b, "\n## Steps\n\n%d calls", len(ev.Steps))
	if ev.Omitted.Calls > 0 || ev.Omitted.Screenshots > 0 {
		fmt.Fprintf(&b, " (older calls omitted: %d; screenshots omitted: %d)", ev.Omitted.Calls, ev.Omitted.Screenshots)
	}
	b.WriteString(". Steps of a `vm_batch` name it as their batch. Full arguments and results are in `evidence.json`.\n\n| # | Batch | Tool | Outcome | ms | Detail |\n|---|---|---|---|---|---|\n")
	for _, s := range ev.Steps {
		batch := ""
		if s.Parent != 0 {
			batch = fmt.Sprint(s.Parent)
		}
		outcome, detail := "ok", summarize(s.Result)
		if !s.OK {
			e, _ := s.Error.(map[string]any)
			outcome = fmt.Sprintf("**%v**", e["error"])
			detail = fmt.Sprint(e["reason"])
		}
		fmt.Fprintf(&b, "| %d | %s | `%s` | %s | %d | %s |\n", s.Seq, batch, s.Tool, outcome, s.ElapsedMS, mdCell(detail))
	}
	if len(ev.Asserts) > 0 {
		b.WriteString("\n## Assertions\n\n| Call | Step | Condition | Outcome | Actual |\n|---|---|---|---|---|\n")
		for _, a := range ev.Asserts {
			step := ""
			if a.Step != nil {
				step = fmt.Sprintf("%d `%s`", *a.Step, a.StepTool)
			}
			outcome := "not evaluated"
			if a.Passed != nil {
				outcome = map[bool]string{true: "passed", false: "**failed**"}[*a.Passed]
			}
			cond, _ := json.Marshal(a.Condition)
			actual := ""
			if a.Actual != nil {
				j, _ := json.Marshal(a.Actual)
				actual = string(j)
			}
			fmt.Fprintf(&b, "| %d `%s` | %s | `%s` | %s | %s |\n", a.Seq, a.Tool, step, mdCell(string(cond)), outcome, mdCell(actual))
		}
	}
	if len(ev.Files) > 0 {
		b.WriteString("\n## Files\n\n| Source | Path | Exists | SHA-256 | Version | Size |\n|---|---|---|---|---|---|\n")
		for _, f := range ev.Files {
			size := ""
			if f.Size != nil {
				size = fmt.Sprint(*f.Size)
			}
			fmt.Fprintf(&b, "| %s | %s | %v | `%s` | %s | %s |\n", f.Source, mdCell(f.Resolved), f.Exists, f.SHA256, mdCell(f.Version), size)
		}
	}
	shots := false
	for _, s := range ev.Steps {
		for _, p := range s.Screenshots {
			if !shots {
				b.WriteString("\n## Screenshots\n")
				shots = true
			}
			fmt.Fprintf(&b, "\n### Call %d `%s`\n\n![call %d](%s)\n", s.Seq, s.Tool, s.Seq, p)
		}
	}
	fmt.Fprintf(&b, "\n---\n\n%d redactions (the VM's stored unlock password and the requested strings); screenshots are not redacted.\n", ev.Redactions)
	if ev.SkippedShortSecrets > 0 {
		fmt.Fprintf(&b, "\n%d secrets shorter than %d characters were not redacted.\n", ev.SkippedShortSecrets, minRedact)
	}
	return b.String()
}

// summarize describes a result in one short line for the report.
func summarize(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, k := range []string{"state", "exit_code", "status", "satisfied", "completed", "handle", "pid", "observation_id", "applied_chars", "verified", "value"} {
		if x, ok := m[k]; ok && x != nil {
			if f, ok := x.(float64); ok { // decoded JSON numbers: handles and PIDs in full, not 1.9e+06
				x = strconv.FormatFloat(f, 'f', -1, 64)
			}
			parts = append(parts, fmt.Sprintf("%s=%v", k, x))
		}
	}
	if s, ok := m["stdout"].(string); ok && s != "" {
		s = strings.TrimSpace(s)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		parts = append(parts, "stdout="+s)
	}
	return strings.Join(parts, ", ")
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\n", " ")
}

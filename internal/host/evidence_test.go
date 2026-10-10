package host

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

func TestEvidenceExportsTheTaskRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	orig := evidenceSecrets
	evidenceSecrets = func(string) []string { return []string{"pw-secret", "z"} } // "z" is too short to redact
	defer func() { evidenceSecrets = orig }()
	rec := &execRecorder{}
	cs, d := connectTools(t, ctx, newFakeAgentBackend(func(req proto.Request) (any, error) {
		switch req.Op {
		case proto.OpFileInfo:
			var a proto.PathsArgs
			_ = json.Unmarshal(req.Args, &a)
			var out proto.FileInfoResult
			for _, p := range a.Paths {
				out.Files = append(out.Files, proto.FileInfo{Path: p, Resolved: p, Exists: true, Type: "file", SHA256: "ab" + p[len(p)-1:]})
			}
			return out, nil
		case proto.OpPing:
			return proto.PingResult{Version: "agent-test", Protocol: proto.Protocol, User: "WIN10\\tester"}, nil
		case proto.OpExec:
			return rec.respond(req)
		}
		return nil, errors.New("unexpected op " + req.Op)
	}))
	d.tasks = newTaskRegistry()
	registerFileInfo(d)
	addToolIn(d, toolSpec{name: "vm_fake_image", readOnly: true}, func(ctx context.Context, in vmIn) (*mcp.CallToolResult, error) {
		return jsonImageResult(map[string]any{"n": 1}, []byte("\x89PNG shot"))
	})
	with := func(m map[string]any) map[string]any {
		out := map[string]any{"task_id": "t-ev"}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	var ignored map[string]any
	callJSON(t, ctx, cs, "vm_exec", with(map[string]any{"command": "echo secret-xyz pw-secret"}), &ignored)
	callRefused(t, ctx, cs, "vm_batch", with(map[string]any{"steps": []any{
		map[string]any{"tool": "vm_fake_image", "assert": []any{map[string]any{"path": "n", "equals": 1}}},
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "b"}, "assert": []any{map[string]any{"path": "exit_code", "equals": 1}}},
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "c"}, "assert": []any{map[string]any{"path": "stdout", "exists": true}}},
	}}))
	callJSON(t, ctx, cs, "vm_file_info", with(map[string]any{"paths": []any{`C:\a`}}), &ignored)
	callJSON(t, ctx, cs, "vm_file_info", map[string]any{"task_id": "t-other", "paths": []any{`C:\unrelated`}}, &ignored)

	dir := t.TempDir()
	var out struct {
		Path        string         `json:"path"`
		SHA256      string         `json:"sha256"`
		Calls       int            `json:"calls"`
		Screenshots int            `json:"screenshots"`
		Assertions  map[string]int `json:"assertions"`
		Files       int            `json:"files"`
		Redactions  int            `json:"redactions"`
		Skipped     int            `json:"skipped_short_secrets"`
	}
	callJSON(t, ctx, cs, "vm_evidence", with(map[string]any{"dir": dir, "title": "test run", "redact": []any{"secret-xyz"}, "files": []any{`C:\b`}}), &out)
	if out.Calls != 5 || out.Screenshots != 1 || out.Files != 2 || out.Redactions < 2 || out.Skipped != 1 || len(out.SHA256) != 64 ||
		out.Assertions["passed"] != 1 || out.Assertions["failed"] != 1 || out.Assertions["not_evaluated"] != 1 {
		t.Fatalf("export %+v", out)
	}
	z, err := zip.OpenReader(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	files := map[string]string{}
	for _, f := range z.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		files[f.Name] = string(b)
	}
	for name, body := range files {
		if strings.Contains(body, "secret-xyz") || strings.Contains(body, "pw-secret") {
			t.Errorf("%s holds a redacted string", name)
		}
		if strings.Contains(body, "unrelated") {
			t.Errorf("%s holds another task's call", name)
		}
	}
	if !strings.Contains(files["evidence.json"], `"task_id": "t-ev"`) || files["report.md"] == "" || !strings.HasPrefix(files["report.md"], "# test run") || len(files) != 3 {
		t.Errorf("zip entries %v", keys(files))
	}
	var ev evidence
	if err := json.Unmarshal([]byte(files["evidence.json"]), &ev); err != nil {
		t.Fatal(err)
	}
	batch := ev.Steps[1]
	if batch.Tool != "vm_fake_image" && batch.Tool != "vm_batch" {
		t.Fatalf("steps %+v", ev.Steps)
	}
	var batchSeq int
	for _, s := range ev.Steps {
		if s.Tool == "vm_batch" {
			batchSeq = s.Seq
		}
	}
	nested := 0
	for _, s := range ev.Steps {
		if s.Parent == batchSeq && batchSeq != 0 {
			nested++
		}
		if s.Tool == "vm_fake_image" && (len(s.Screenshots) != 1 || files[s.Screenshots[0]] != "\x89PNG shot") {
			t.Errorf("screenshot of %+v", s)
		}
	}
	if nested != 2 || ev.Agent["version"] != "agent-test" || ev.VM.Name != "Win10" || ev.Files[1].Source != "export" || ev.Files[1].SHA256 != "abb" {
		t.Errorf("evidence: nested %d agent %v vm %+v files %+v", nested, ev.Agent, ev.VM, ev.Files)
	}
	for _, a := range ev.Asserts {
		if a.Step != nil && *a.Step == 1 && (a.Passed == nil || *a.Passed || a.Actual != float64(0)) {
			t.Errorf("failed assertion %+v", a)
		}
	}
	// Without a task there is no record to export.
	d.tasks = nil
	if e := callRefused(t, ctx, cs, "vm_evidence", map[string]any{"dir": dir}); e["error"] != codeFailed {
		t.Errorf("no task: %v", e)
	}
}

// A secret sent JSON-escaped (\uXXXX, as Python's json.dumps does) is still redacted, in every part of the record.
func TestEvidenceRedactsDecodedStrings(t *testing.T) {
	ev := evidence{
		TaskID: "task-密码-xyz",
		Steps:  []evidenceStep{{Seq: 1, Tool: "vm_exec", Args: decodeJSON(`{"command":"echo \u5bc6\u7801-xyz"}`)}},
		Files:  []evidenceFile{{Source: "export", FileInfo: proto.FileInfo{Error: `open C:\密码-xyz: denied`}}},
		Agent:  map[string]any{"error": "密码-xyz"},
	}
	r := &redactor{secrets: []string{"密码-xyz"}}
	if err := r.evidence(&ev); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), "密码") || strings.Contains(string(b), `\u5bc6`) || r.count != 4 {
		t.Errorf("redacted %d: %s", r.count, b)
	}
}

// An over-long result keeps its structure: long strings are shortened, small fields survive.
func TestEvidenceShrinkKeepsStructure(t *testing.T) {
	big := map[string]any{"status": "complete", "steps": []any{map[string]any{"ok": true, "assertions_passed": 2, "result": map[string]any{"controls": strings.Repeat("x", 300000)}}}}
	b, _ := json.Marshal(big)
	out := shrinkJSON(string(b), journalMaxText)
	var v map[string]any
	if len(out) > journalMaxText || json.Unmarshal([]byte(out), &v) != nil || v["status"] != "complete" {
		t.Fatalf("shrunk to %d bytes: %.200s", len(out), out)
	}
	step := v["steps"].([]any)[0].(map[string]any)
	if step["assertions_passed"] != float64(2) || !strings.Contains(step["result"].(map[string]any)["controls"].(string), "truncated") {
		t.Errorf("step %v", step)
	}
	if got := shrinkJSON(strings.Repeat("y", 20), 10); got != strings.Repeat("y", 10) {
		t.Errorf("plain text %q", got)
	}
}

// Calls dropped beyond the entry limit count their screenshots as omitted too.
func TestEvidenceJournalCountsDroppedScreenshots(t *testing.T) {
	var j taskJournal
	for i := 0; i < journalMaxEntries+1; i++ {
		j.add(journalEntry{Seq: i + 1, Images: [][]byte{[]byte("png")}})
	}
	entries, calls, shots := j.snapshot()
	if len(entries) != journalMaxEntries || calls != 1 || shots != 1 || entries[0].Seq != 2 {
		t.Errorf("entries %d dropped calls %d screenshots %d first %d", len(entries), calls, shots, entries[0].Seq)
	}
}

func TestEvidenceSummaryNumbers(t *testing.T) {
	if got := summarize(map[string]any{"handle": float64(1966184), "exit_code": float64(0)}); got != "exit_code=0, handle=1966184" {
		t.Errorf("summary %q", got)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

func startTestJob(t *testing.T, a proto.ExecArgs) proto.JobInfo {
	t.Helper()
	b, _ := json.Marshal(a)
	r, _, err := jobStart(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.(proto.JobInfo)
}

func readTestJob(t *testing.T, a proto.JobReadArgs) proto.JobResult {
	t.Helper()
	b, _ := json.Marshal(a)
	r, _, err := jobRead(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.(proto.JobResult)
}

// waitJob reads the job until it is no longer running, collecting its output incrementally.
func waitJob(t *testing.T, id string) (proto.JobResult, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	a := proto.JobReadArgs{ID: id, MaxBytes: 7}
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := readTestJob(t, a)
		out.WriteString(r.Stdout)
		errOut.WriteString(r.Stderr)
		a.StdoutOffset, a.StderrOffset = r.StdoutNext, r.StderrNext
		if r.State != proto.JobRunning && r.StdoutNext == r.StdoutBytes && r.StderrNext == r.StderrBytes {
			return r, out.String(), errOut.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still %s", id, r.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestJobRunsToExitWithIncrementalOutput(t *testing.T) {
	info := startTestJob(t, proto.ExecArgs{Command: "Write-Output 'line one'; Start-Sleep -Milliseconds 300; Write-Output '管道 two'; [Console]::Error.WriteLine('oops'); exit 3"})
	if info.State != proto.JobRunning || info.PID == 0 || !strings.HasPrefix(info.ID, "job-") || info.Shell != "powershell" || info.TimeoutMs != proto.JobDefaultTimeoutMs {
		t.Fatalf("start: %+v", info)
	}
	r, out, errOut := waitJob(t, info.ID)
	if r.State != proto.JobExited || r.ExitCode == nil || *r.ExitCode != 3 || r.EndedAt == "" {
		t.Fatalf("final: %+v", r)
	}
	if !strings.Contains(out, "line one") || !strings.Contains(out, "管道 two") || !strings.Contains(errOut, "oops") {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
}

func TestJobCancelTerminatesProcessTree(t *testing.T) {
	// The shell starts a separate child process; cancelling the job must end it too.
	info := startTestJob(t, proto.ExecArgs{Command: "$p = Start-Process ping.exe -ArgumentList '-n','60','127.0.0.1' -PassThru -WindowStyle Hidden; Write-Output $p.Id; Wait-Process -Id $p.Id"})
	var child uint32
	for deadline := time.Now().Add(20 * time.Second); child == 0; time.Sleep(50 * time.Millisecond) {
		r := readTestJob(t, proto.JobReadArgs{ID: info.ID})
		if n, err := strconv.Atoi(strings.TrimSpace(r.Stdout)); err == nil {
			child = uint32(n)
		}
		if time.Now().After(deadline) || r.State != proto.JobRunning {
			t.Fatalf("no child PID: %+v", r)
		}
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, child)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	b, _ := json.Marshal(proto.JobArgs{ID: info.ID})
	r, _, err := jobCancel(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := r.(proto.JobResult)
	if res.State != proto.JobCancelled || res.ExitCode == nil || res.ElapsedMs > 20000 {
		t.Fatalf("cancel: %+v", res)
	}
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("child %d still runs after cancel", child)
	}
	// Cancelling again is harmless and reports the same final state.
	r, _, err = jobCancel(context.Background(), b, nil)
	if err != nil || r.(proto.JobResult).State != proto.JobCancelled {
		t.Fatalf("second cancel: %+v %v", r, err)
	}
}

func TestJobTimeout(t *testing.T) {
	info := startTestJob(t, proto.ExecArgs{Command: "Start-Sleep -Seconds 30", TimeoutMs: 500})
	r, _, _ := waitJob(t, info.ID)
	if r.State != proto.JobTimedOut || r.ElapsedMs > 20000 {
		t.Fatalf("timeout: %+v", r)
	}
}

func TestJobRefusals(t *testing.T) {
	for name, a := range map[string]proto.ExecArgs{
		"admin":   {Command: "hostname", Admin: true},
		"timeout": {Command: "hostname", TimeoutMs: proto.JobMaxTimeoutMs + 1},
		"shell":   {Command: "hostname", Shell: "bash"},
	} {
		b, _ := json.Marshal(a)
		if _, _, err := jobStart(context.Background(), b, nil); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	b, _ := json.Marshal(proto.JobReadArgs{ID: "job-missing"})
	if _, _, err := jobRead(context.Background(), b, nil); err == nil || !strings.HasPrefix(err.Error(), proto.ErrNoJob) {
		t.Fatalf("missing job: %v", err)
	}
	info := startTestJob(t, proto.ExecArgs{Command: "Write-Output hi"})
	waitJob(t, info.ID)
	b, _ = json.Marshal(proto.JobReadArgs{ID: info.ID, StdoutOffset: 1 << 20})
	if _, _, err := jobRead(context.Background(), b, nil); err == nil {
		t.Fatal("offset beyond the output was accepted")
	}
	r, _, err := jobList(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range r.(proto.JobListResult).Jobs {
		found = found || j.ID == info.ID
	}
	if !found {
		t.Fatal("job_list misses a job")
	}
}

func TestOutputBufUTF8Boundary(t *testing.T) {
	var o outputBuf
	o.Write([]byte("a管")) // 'a' + 3 bytes
	text, next, _, _, _ := o.read(0, 3, false)
	if text != "a" || next != 1 {
		t.Fatalf("split read: %q %d", text, next)
	}
	text, next, _, _, _ = o.read(next, 10, false)
	if text != "管" || next != 4 {
		t.Fatalf("rest: %q %d", text, next)
	}
	// Once final, an incomplete trailing sequence is returned as it is (it can never be completed).
	o.Write([]byte{0xe7})
	if _, next, _, _, _ = o.read(4, 10, true); next != 5 {
		t.Fatalf("final read stopped at %d", next)
	}
}

// A read smaller than one character still returns that character (once all its bytes are there), so readers always
// make progress; OEM (double-byte) output is cut after a line break while more may come.
func TestOutputBufSmallReadsAndOEM(t *testing.T) {
	var o outputBuf
	o.Write([]byte("é管"))
	var got string
	for off := int64(0); off < 5; {
		text, next, _, _, err := o.read(off, 1, true)
		if err != nil || next <= off {
			t.Fatalf("read at %d: %q %d %v", off, text, next, err)
		}
		got, off = got+text, next
	}
	if got != "é管" {
		t.Fatalf("got %q", got)
	}
	var partial outputBuf
	partial.Write([]byte{0xe7, 0xae}) // the first two bytes of 管
	if text, next, _, _, _ := partial.read(0, 1, false); text != "" || next != 0 {
		t.Fatalf("incomplete rune of a running job: %q %d", text, next)
	}
	var oem outputBuf
	oem.Write([]byte("ab\r\n\xb9\xdc")) // GBK 管 after a line
	if _, next, _, _, _ := oem.read(0, 64, false); next != 4 {
		t.Fatalf("OEM chunk of a running job ended at %d, want after the line break", next)
	}
	if _, next, _, _, _ := oem.read(0, 64, true); next != 6 {
		t.Fatalf("OEM chunk of a finished job ended at %d", next)
	}
	// One-byte reads of double-byte OEM text advance (a running job may still complete a trailing lead byte).
	for off := int64(4); off < 6; {
		_, next, _, _, _ := oem.read(off, 1, true)
		if next <= off {
			t.Fatalf("OEM one-byte read at %d did not advance", off)
		}
		off = next
	}
}

func TestOutputBufCap(t *testing.T) {
	var o outputBuf
	o.Write(make([]byte, proto.JobMaxOutputBytes-1))
	if n, _ := o.Write([]byte("xyz")); n != 3 {
		t.Fatal("Write must report the whole input as written")
	}
	if len(o.b) != proto.JobMaxOutputBytes || o.dropped != 2 {
		t.Fatalf("stored %d dropped %d", len(o.b), o.dropped)
	}
	// Once full, nothing written later is stored (the stored bytes stay a prefix of the stream), and a character cut
	// at the cap is returned as its bytes: the end of a full stream is final.
	var c outputBuf
	c.Write(make([]byte, proto.JobMaxOutputBytes-1))
	c.Write([]byte("管"))
	c.Write([]byte("ok\n"))
	if len(c.b) != proto.JobMaxOutputBytes || c.dropped != 5 || !c.full {
		t.Fatalf("stored %d dropped %d full %v", len(c.b), c.dropped, c.full)
	}
	for off := int64(len(c.b)) - 2; off < int64(len(c.b)); {
		_, next, _, _, _ := c.read(off, 1, false)
		if next <= off {
			t.Fatalf("read of a full stream stalled at %d", off)
		}
		off = next
	}
}

// A finished stream that ends inside a character (or with double-byte OEM text read one byte at a time) still
// reaches its end.
func TestOutputBufFinalIncompleteCharacter(t *testing.T) {
	var o outputBuf
	o.Write([]byte{'a', 0xe4, 0xb8})
	for off := int64(0); off < 3; {
		_, next, _, _, _ := o.read(off, 1, true)
		if next <= off {
			t.Fatalf("stalled at %d", off)
		}
		off = next
	}
}

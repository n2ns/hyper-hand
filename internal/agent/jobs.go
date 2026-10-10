package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

// outputBuf stores a job stream's first proto.JobMaxOutputBytes and counts the rest. Once it drops anything it is
// full: everything later is dropped too, so the stored output is always a prefix of the stream.
type outputBuf struct {
	mu      sync.Mutex
	b       []byte
	dropped int64
	full    bool
}

func (o *outputBuf) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.full {
		o.dropped += int64(len(p))
		return len(p), nil
	}
	n := min(len(p), proto.JobMaxOutputBytes-len(o.b))
	o.b = append(o.b, p[:n]...)
	o.dropped += int64(len(p) - n)
	// Stored bytes are never removed (readers may already have them); a character cut at the cap stays cut, and
	// read treats the end of a full stream as final.
	o.full = o.full || n < len(p)
	return len(p), nil
}

// read returns at most limit bytes from off and the stored size; final means no more output can arrive.
func (o *outputBuf) read(off int64, limit int, final bool) (text string, next, size, dropped int64, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	size, dropped = int64(len(o.b)), o.dropped
	if off < 0 || off > size {
		return "", off, size, dropped, fmt.Errorf("offset %d is outside the %d bytes of output", off, size)
	}
	end := min(size, off+int64(limit))
	chunk := o.b[off:end]
	// A full stream gets no more bytes: like a finished one, its end is final.
	final = final || o.full
	if end < size || !final {
		chunk = wholeChars(chunk, o.b[off:], final)
	}
	return toUTF8(chunk), off + int64(len(chunk)), size, dropped, nil
}

// wholeChars shortens chunk (a prefix of rest, the stored output from the same offset) so that it does not end inside a
// character that later bytes complete (final: no more bytes come). UTF-8 output ends at a whole rune: a chunk shorter than one rune is extended to
// that rune when rest holds all of it, so every read of complete text makes progress. Other output (the OEM code page,
// possibly double-byte) ends after its last line break when it has one.
func wholeChars(chunk, rest []byte, final bool) []byte {
	if utf8.Valid(chunk) {
		return chunk
	}
	for i := len(chunk) - 1; i >= 0 && i >= len(chunk)-utf8.UTFMax; i-- {
		if utf8.RuneStart(chunk[i]) {
			if !utf8.FullRune(chunk[i:]) && utf8.Valid(chunk[:i]) {
				if i > 0 {
					return chunk[:i]
				}
				if !utf8.FullRune(rest) {
					if final {
						return rest // the stream ends inside this character: return its bytes
					}
					return chunk[:0] // the character's remaining bytes are not written yet
				}
				if _, n := utf8.DecodeRune(rest); n > 1 {
					return rest[:n]
				}
				return chunk // not UTF-8 (an OEM byte): return it rather than stall
			}
			break
		}
	}
	if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
		return chunk[:i+1]
	}
	return chunk
}

type bgJob struct {
	id, command, shell, cwd string
	pid                     int
	started                 time.Time
	timeout                 time.Duration
	stdout, stderr          outputBuf
	cancel                  chan struct{}
	cancelOnce              sync.Once
	done                    chan struct{} // closed when the shell has exited and the state is final

	mu    sync.Mutex
	state string
	exit  *int
	ended time.Time
}

type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*bgJob
}

var jobs = &jobStore{jobs: map[string]*bgJob{}}

func (j *bgJob) info() proto.JobInfo {
	j.mu.Lock()
	state, exit, ended := j.state, j.exit, j.ended
	j.mu.Unlock()
	end := time.Now()
	if !ended.IsZero() {
		end = ended
	}
	i := proto.JobInfo{ID: j.id, PID: j.pid, Command: j.command, Shell: j.shell, Cwd: j.cwd, State: state, ExitCode: exit,
		StartedAt: j.started.Format(time.RFC3339), ElapsedMs: end.Sub(j.started).Milliseconds(), TimeoutMs: int(j.timeout.Milliseconds())}
	if !ended.IsZero() {
		i.EndedAt = ended.Format(time.RFC3339)
	}
	j.stdout.mu.Lock()
	i.StdoutBytes, i.StdoutDropped = int64(len(j.stdout.b)), j.stdout.dropped
	j.stdout.mu.Unlock()
	j.stderr.mu.Lock()
	i.StderrBytes, i.StderrDropped = int64(len(j.stderr.b)), j.stderr.dropped
	j.stderr.mu.Unlock()
	return i
}

func (j *bgJob) finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// run waits for the shell to exit, for the timeout or for a cancellation; the latter two terminate the job object
// (the whole process tree). The job handle is closed once the shell has exited.
func (j *bgJob) run(cmd *exec.Cmd, job windows.Handle) {
	defer windows.CloseHandle(job)
	waited := make(chan struct{})
	go func() { cmd.Wait(); close(waited) }()
	t := time.NewTimer(j.timeout)
	defer t.Stop()
	state := proto.JobExited
	select {
	case <-waited:
	case <-t.C:
		state = proto.JobTimedOut
	case <-j.cancel:
		state = proto.JobCancelled
	}
	if state != proto.JobExited {
		if err := windows.TerminateJobObject(job, 1); err != nil {
			cmd.Process.Kill()
		}
		<-waited
	}
	code := cmd.ProcessState.ExitCode()
	j.mu.Lock()
	j.state, j.exit, j.ended = state, &code, time.Now()
	j.mu.Unlock()
	close(j.done)
}

// pruneLocked drops jobs that ended more than JobRetentionMs ago and, while the store is full, the oldest finished one.
func (s *jobStore) pruneLocked(now time.Time) error {
	var finished []*bgJob
	for id, j := range s.jobs {
		if !j.finished() {
			continue
		}
		j.mu.Lock()
		ended := j.ended
		j.mu.Unlock()
		if now.Sub(ended) > proto.JobRetentionMs*time.Millisecond {
			delete(s.jobs, id)
			continue
		}
		finished = append(finished, j)
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].started.Before(finished[b].started) })
	for len(s.jobs) >= proto.JobMaxJobs {
		if len(finished) == 0 {
			return fmt.Errorf("%d background jobs are running; cancel one with job_cancel first", len(s.jobs))
		}
		delete(s.jobs, finished[0].id)
		finished = finished[1:]
	}
	return nil
}

func (s *jobStore) get(id string) (*bgJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	if j == nil {
		return nil, fmt.Errorf("%s %q: it never existed, was dropped (%d jobs kept, finished ones for 24 hours) or the agent restarted since (update, reboot), which forgets its jobs but does not stop their processes", proto.ErrNoJob, id, proto.JobMaxJobs)
	}
	return j, nil
}

func jobStart(_ context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.ExecArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	if a.Admin {
		return nil, nil, errors.New("background jobs run as the agent's user; elevated background jobs are not supported")
	}
	if a.TimeoutMs < 0 || a.TimeoutMs > proto.JobMaxTimeoutMs {
		return nil, nil, fmt.Errorf("timeout_ms must be 0 (default %d) to %d", proto.JobDefaultTimeoutMs, proto.JobMaxTimeoutMs)
	}
	if a.TimeoutMs == 0 {
		a.TimeoutMs = proto.JobDefaultTimeoutMs
	}
	cmd, err := shellCommand(a, "")
	if err != nil {
		return nil, nil, err
	}
	var idb [6]byte
	rand.Read(idb[:])
	shell := a.Shell
	if shell == "" {
		shell = "powershell"
	}
	j := &bgJob{id: "job-" + hex.EncodeToString(idb[:]), command: a.Command, shell: shell, cwd: a.Cwd, timeout: time.Duration(a.TimeoutMs) * time.Millisecond,
		cancel: make(chan struct{}), done: make(chan struct{}), state: proto.JobRunning}
	cmd.Stdout, cmd.Stderr = &j.stdout, &j.stderr
	cmd.WaitDelay = 5 * time.Second
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if err := jobs.pruneLocked(time.Now()); err != nil {
		return nil, nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create command job: %w", err)
	}
	if err := startInJob(cmd, job); err != nil {
		windows.CloseHandle(job)
		return nil, nil, err
	}
	j.pid, j.started = cmd.Process.Pid, time.Now()
	jobs.jobs[j.id] = j
	go j.run(cmd, job)
	return j.info(), nil, nil
}

func jobRead(_ context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.JobReadArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	j, err := jobs.get(a.ID)
	if err != nil {
		return nil, nil, err
	}
	r, err := readJob(j, a)
	return r, nil, err
}

func readJob(j *bgJob, a proto.JobReadArgs) (proto.JobResult, error) {
	limit := a.MaxBytes
	if limit <= 0 {
		limit = 64 << 10
	}
	limit = min(limit, proto.JobReadMaxBytes)
	final := j.finished() // read before the output: a finished job writes nothing more
	r := proto.JobResult{JobInfo: j.info()}
	var err error
	if r.Stdout, r.StdoutNext, _, _, err = j.stdout.read(a.StdoutOffset, limit, final); err != nil {
		return r, fmt.Errorf("stdout: %w", err)
	}
	if r.Stderr, r.StderrNext, _, _, err = j.stderr.read(a.StderrOffset, limit, final); err != nil {
		return r, fmt.Errorf("stderr: %w", err)
	}
	return r, nil
}

func jobCancel(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.JobArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	j, err := jobs.get(a.ID)
	if err != nil {
		return nil, nil, err
	}
	j.cancelOnce.Do(func() { close(j.cancel) })
	select {
	case <-j.done:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-time.After(15 * time.Second):
		return nil, nil, errors.New("the job's process tree was told to terminate but its shell has not exited within 15 s")
	}
	r, err := readJob(j, proto.JobReadArgs{ID: j.id, StdoutOffset: 0, StderrOffset: 0, MaxBytes: 1})
	// Report the state only: cancel is not an output read.
	r.Stdout, r.Stderr, r.StdoutNext, r.StderrNext = "", "", 0, 0
	return r, nil, err
}

func jobList(context.Context, json.RawMessage, []byte) (any, []byte, error) {
	jobs.mu.Lock()
	all := make([]*bgJob, 0, len(jobs.jobs))
	for _, j := range jobs.jobs {
		all = append(all, j)
	}
	jobs.mu.Unlock()
	sort.Slice(all, func(a, b int) bool { return all[a].started.Before(all[b].started) })
	r := proto.JobListResult{Jobs: []proto.JobInfo{}}
	for _, j := range all {
		r.Jobs = append(r.Jobs, j.info())
	}
	return r, nil, nil
}

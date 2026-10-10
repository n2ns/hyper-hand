package proto

// Background command jobs: a command started with job_start keeps running in the agent after the request returns;
// its state and output are read with job_read until the agent process exits (update, reboot), whatever happens to
// the host connection.
const (
	OpJobStart  = "job_start"  // ExecArgs (TimeoutMs 0 means JobDefaultTimeoutMs; Admin is refused) -> JobInfo
	OpJobRead   = "job_read"   // JobReadArgs -> JobResult
	OpJobCancel = "job_cancel" // JobArgs -> JobResult: terminate the job's process tree
	OpJobList   = "job_list"   // -> JobListResult
)

const (
	JobDefaultTimeoutMs = 60 * 60 * 1000      // 1 hour
	JobMaxTimeoutMs     = 24 * 60 * 60 * 1000 // 24 hours
	JobMaxJobs          = 32                  // jobs kept; the oldest finished one is dropped first
	JobRetentionMs      = 24 * 60 * 60 * 1000 // a finished job is dropped this long after it ended
	JobMaxOutputBytes   = 16 << 20            // stored per stream; later output is counted in *_dropped
	JobReadMaxBytes     = 1 << 20             // per stream per job_read
)

// Job states. Running jobs have no exit code. Exited: the shell exited by itself; TimedOut: the job ran past its
// timeout and its process tree was terminated; Cancelled: job_cancel terminated it.
const (
	JobRunning   = "running"
	JobExited    = "exited"
	JobTimedOut  = "timed_out"
	JobCancelled = "cancelled"
)

// ErrNoJob prefixes the agent's error for an unknown job ID.
const ErrNoJob = "no such job"

type JobArgs struct {
	ID string `json:"id"`
}

// JobReadArgs: the output from the given byte offsets of each stream, at most MaxBytes (0: 64 KiB; at most
// JobReadMaxBytes) per stream.
type JobReadArgs struct {
	ID           string `json:"id"`
	StdoutOffset int64  `json:"stdout_offset,omitempty"`
	StderrOffset int64  `json:"stderr_offset,omitempty"`
	MaxBytes     int    `json:"max_bytes,omitempty"`
}

// JobInfo describes a job. Times are RFC 3339 in the guest's offset; EndedAt is "" while it runs. StdoutBytes and
// StderrBytes are the bytes stored so far (offsets range over them); *Dropped the bytes beyond JobMaxOutputBytes
// that were discarded.
type JobInfo struct {
	ID            string `json:"id"`
	PID           int    `json:"pid"`
	Command       string `json:"command"`
	Shell         string `json:"shell"`
	Cwd           string `json:"cwd"`
	State         string `json:"state"`
	ExitCode      *int   `json:"exit_code"`
	StartedAt     string `json:"started_at"`
	EndedAt       string `json:"ended_at"`
	ElapsedMs     int64  `json:"elapsed_ms"`
	TimeoutMs     int    `json:"timeout_ms"`
	StdoutBytes   int64  `json:"stdout_bytes"`
	StderrBytes   int64  `json:"stderr_bytes"`
	StdoutDropped int64  `json:"stdout_dropped"`
	StderrDropped int64  `json:"stderr_dropped"`
}

// JobResult is a job's state with output read from the requested offsets: Stdout and Stderr are text (UTF-8, or
// decoded from the OEM code page), never ending inside a UTF-8 sequence while the job may still write; the next
// offsets to request are StdoutNext and StderrNext.
type JobResult struct {
	JobInfo
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	StdoutNext int64  `json:"stdout_next"`
	StderrNext int64  `json:"stderr_next"`
}

type JobListResult struct {
	Jobs []JobInfo `json:"jobs"`
}

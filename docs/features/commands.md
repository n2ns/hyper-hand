# 5. Commands

Part of [HyperHand features](../features.md). Section numbers such as 8.6 refer to the chapters listed there.

### 5.1 vm_exec

`vm_exec` runs `command` in the guest as the logged-on user (the agent's user), waits for it to exit and returns:

```json
{"exit_code": 0, "stdout": "WIN10\r\n", "stderr": "", "timed_out": false}
```

A non-zero exit code is not a tool error. An empty `command` or an unknown `shell` is `invalid_argument`; an agent that does not answer is `agent_required`; errors from the agent (start failure, cancellation) are `failed`. `vm_exec` is not for starting GUI programs, whose process would belong to the request: use `vm_launch` (see 7.2). For commands that run longer than a call should wait, use `background: true` (see 5.7).

### 5.2 Shells

- `shell` = `powershell` (default, also for an empty value): `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command` with `[Console]::OutputEncoding` set to UTF-8 before the command.
- `shell` = `cmd`: the command line is exactly `cmd.exe /d /s /c "<command>"` (AutoRun disabled, outer quotes stripped by `/s`).
- Any other value is `invalid_argument` (`shell: expected powershell or cmd, got "<value>"`).
- The process is started hidden, without a console window.

### 5.3 Working directory

`cwd` sets the working directory. When empty, the command inherits the agent's working directory.

### 5.4 Timeout

- `timeout_ms` defaults to 60000; zero or a negative value also means 60 s.
- For non-elevated commands, the shell is started suspended, assigned to a Windows Job Object and then resumed. On timeout the job's process tree is terminated, even if the shell has already exited, and the result has `timed_out: true`, with the exit code and whatever output was captured. Elevated commands use the separate path in 5.5.
- If the host disconnects while the command runs (see 1.4), the process tree is killed in the same way and no result is returned.
- After the process exits, the agent waits at most 5 s for its output pipes to close (for example when a child process still holds them).

### 5.5 Elevated execution (admin)

`admin` = `true` runs the command elevated. UAC may ask for consent or administrator credentials; waiting for it counts toward `timeout_ms`.

- A hidden, ordinary launcher process requests elevation of a one-shot worker. The launcher can be stopped even while UAC is waiting, leaving the agent available for the next request.
- The worker receives the command over a random, single-use local named pipe. It verifies the pipe server's PID and the original agent process's creation time. A late UAC approval after cancellation or timeout cannot retrieve the expired command.
- `cwd` defaults to the agent's working directory. `cmd` uses a temporary `%TEMP%\hh-admin-*.cmd` wrapper to select UTF-8 before a nested `cmd /d /s /c` parses the command. PowerShell uses a temporary `%TEMP%\hh-admin-*.ps1` file with a UTF-8 BOM, preserving support for long scripts. Both files are deleted afterwards. Explicit `exit N` is preserved; for PowerShell, a failed last statement otherwise returns exit code 1.
- The worker captures stdout/stderr and manages the command tree with a Windows Job Object. Pipe disconnection cancels the command; the original absolute deadline also terminates it. No second elevation prompt is needed to stop the command.
- A timeout before execution returns `timed_out: true`, exit code `-1` and empty output. Once execution starts, the agent allows up to 5 seconds for job cleanup and the captured result to return. Cancellation is reported as a tool error.

### 5.6 Output decoding

stdout and stderr are decoded independently.

- A leading UTF-8 BOM is removed.
- Valid UTF-8 is kept as is.
- Otherwise the bytes are decoded from the guest's OEM code page (the default for `cmd` output).

### 5.7 Background jobs: vm_exec background, vm_job

`vm_exec` with `background: true` starts the command as a job in the guest agent and returns at once; `vm_job` reads, waits for and cancels it. Jobs are meant for commands that run minutes (test runs, capability checks): the call does not hold the agent connection, so other tools on the VM keep working, and the result can be collected after the MCP connection, the task or the host itself was restarted.

- Start: `vm_exec {vm, command, shell, cwd, timeout_ms, background: true}` returns `{"vm": "Win10", "id": "job-3f2a9c01b7de", "pid": 4120, "command": "...", "shell": "powershell", "cwd": "C:\\", "state": "running", "exit_code": null, "started_at": "2026-10-10T21:00:00+07:00", "ended_at": "", "elapsed_ms": 0, "timeout_ms": 3600000, "stdout_bytes": 0, "stderr_bytes": 0, "stdout_dropped": 0, "stderr_dropped": 0, "next": "the command is still running: call vm_job with vm and job_id job-3f2a9c01b7de ..."}`; `next` names the `vm_job` call that reads the job. The command runs as the agent's user, with the same shells (5.2), working directory (5.3), Job Object and output decoding (5.6) as `vm_exec`. `timeout_ms` defaults to 3600000 (1 hour) and may be at most 86400000 (24 hours); when it passes, the job's whole process tree is terminated and the state is `timed_out`. `admin: true` together with `background` is `invalid_argument` (elevated jobs are not supported; run them with `vm_exec admin: true`). Starting a job is a write: it takes the VM's write ownership (1.6).
- Read: `vm_job {vm, job_id, stdout_offset, stderr_offset, max_bytes, wait_ms}` returns the job's fields as above plus `stdout` and `stderr`: the output from the given byte offsets (default 0), at most `max_bytes` per stream (default 65536, at most 1048576), and `stdout_next` / `stderr_next`, the offsets to pass next. A chunk never ends inside a UTF-8 character that later bytes complete; a `max_bytes` smaller than one character still returns that whole character, so reads always advance once its bytes are there (or the job has ended). Output that is not UTF-8 (the OEM code page, possibly double-byte) is cut after its last line break while the job may still write. `complete` is `true` when the job is no longer running and both streams were read to their end. `wait_ms` (0 to 60000, default 0) makes the host poll the agent every 500 ms until the job ends or new output can be returned; `waited_ms` says how long it waited. `vm_end_turn` of the same task cancels such a wait like a `vm_wait`, and the call then returns the last read (or the cancellation error when it came during the first read). Each poll is a short agent request, so other calls on the VM are not blocked. Reading needs no write ownership: any task can read any job by its ID.
- States: `running`; `exited` (the shell exited by itself; `exit_code` is its code); `timed_out`; `cancelled` (`vm_job cancel`). `exit_code` is `null` only while running. A job is finished when its shell has exited (the agent waits at most 5 s for children to release the output pipes). A child that the shell started and left running is then no longer tracked: it keeps running, and neither `cancel` nor the timeout reaches it any more. Start long-lived children in a way that the shell waits for them (`Start-Process -Wait`).
- Cancel: `vm_job {vm, job_id, cancel: true}` terminates the job's whole process tree (Job Object) and returns its final state once the shell has exited (at most 15 s), with `stdout`/`stderr` empty; read the remaining output afterwards with offsets. Cancelling a finished job returns its state unchanged. Cancel is a write and needs the VM's write ownership; `cancel` without `job_id` is `invalid_argument`.
- List: `vm_job {vm}` returns `{"vm": "Win10", "jobs": [...]}`, every job the agent keeps, oldest first, without output.
- Retention: the agent keeps at most 32 jobs; when a new one starts, jobs that ended more than 24 hours ago are dropped, then the oldest finished jobs while 32 are kept; with 32 jobs running, the start fails (`failed`, reason naming the limit). Each stream stores its first 16 MiB; everything after that limit is counted in `stdout_dropped` / `stderr_dropped` and discarded (a character cut at the limit is returned as its bytes).
- Restarts: jobs live in the agent process. Host restarts, MCP reconnects and `vm_end_turn` do not affect them (ending a task neither cancels its jobs nor releases them to anyone; they keep running to their timeout). When the agent itself restarts (`vm_update_agent`, sign-out, reboot), its job list is lost but the jobs' processes are not stopped; reading such a job is `no_job` (`next`: list the agent's jobs).
- Errors: an unknown `job_id` is `no_job`; an offset beyond the stored output, `wait_ms` or `max_bytes` out of range are `invalid_argument`. An older agent is refused with `agent_outdated` (8.6).

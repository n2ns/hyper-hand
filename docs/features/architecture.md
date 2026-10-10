# 1. Architecture

Part of [HyperHand features](../features.md). Section numbers such as 8.6 refer to the chapters listed there.

HyperHand consists of two Windows executables.

- `hyperhand.exe` (host) runs as an ordinary user tray program and serves MCP over Streamable HTTP. The same executable runs separately as `HyperHandService`, under the dedicated virtual account `NT SERVICE\HyperHandService`, to perform WMI (`root\virtualization\v2`) and PowerShell Hyper-V operations.
- `hyperhand-agent.exe` (guest agent) runs inside the VM in the logged-on user's desktop session, shows a tray icon and answers requests from the host over a Hyper-V socket.

### 1.1 Host side

- The MCP endpoint is `http://127.0.0.1:<port>/mcp`, default port 8770 (see 9.1). It listens on the loopback interface only.
- One MCP server instance (name `hyperhand`, version stamped at build time, `dev` for source builds) serves all HTTP sessions.
- Screen capture, mouse, keyboard, VM state and checkpoints use Hyper-V through the service and work without the guest agent (see 3, 4).
- Commands, files, clipboard, window focus and waiting go through the guest agent (see 5, 6, 7).
- The tray requests specific broker operations over a local named pipe whose ACL permits the configured owner, SYSTEM, administrators and the service account. There is no arbitrary host command execution broker operation. Host file reads and writes stay in the ordinary tray process.
- The MCP HTTP endpoint remains unauthenticated. Its callers can use all exposed tools through the tray; the pipe ACL is not MCP authentication.

### 1.2 Hyper-V socket

- The agent listens on the Hyper-V socket service GUID `3ce544e1-2645-4383-b332-fedf8a18736b`, accepting connections from the parent partition.
- The installer registers this GUID under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\<GUID>` with `ElementName` = `HyperHand`. Ordinary tray startup does not write this registration.
- The service dials the VM by its VM ID (`Msvm_ComputerSystem.Name`) and tunnels the connection to the tray. The guest service GUID is fixed; the broker does not accept an arbitrary guest socket service ID.

### 1.3 Connections and request ordering

- The host keeps one agent client per VM ID, holding at most one connection.
- Each client sends one request at a time and waits for its response; concurrent tool calls for the same VM queue on the client.
- A queued call can be cancelled or reach its context deadline without waiting for the active request to finish. It is not sent and does not interrupt the active request.
- Before reusing an idle connection the client probes it with a 1 ms read. A timeout means the connection is alive; EOF, any other error, or unexpected data marks it dead, and the client redials.
- If a request could not be sent, the client reconnects and sends it once more. The resend happens only if the request payload can be rewound (no payload, or a seekable source). A request that was sent is never resent, so a command cannot run twice.
- The client for a VM is closed and discarded after `vm_start`, `vm_shutdown`, `vm_turn_off`, `vm_save`, `vm_pause` and `vm_restore` (see 3).

### 1.4 Cancellation

- When an MCP call is cancelled, the host forces the connection's deadline, which aborts the pending read or write, and drops the connection.
- The agent reads frames on a separate goroutine. The host sends nothing while it waits for a response, so a read error while a request runs means the host went away; the agent then cancels that request's context.
- Cancellation stops `exec` (the process tree is killed, see 5.4), `hash_files`, `file_info` and `wait`. The agent does not send a response after the host disconnected.

### 1.5 Agent service loop

- The agent serves one host connection at a time. When it ends, the agent waits for the next one.
- If the Hyper-V socket listener cannot be created, or accepting fails, the agent retries after 1 s.
- The agent tray icon (the monitor icon, also the executable's icon) opens a menu on a left or right click: `等待宿主机连接` (waiting for the host) or `宿主机已连接` (host connected), and a `退出` (quit) item. Like the host tray (see 9.1), it is added at once at logon and retried every 5 seconds and whenever the taskbar is created; the agent serves the host whether or not the icon could be shown.

### 1.6 Task ownership

- Every tool accepts optional `task_id`. A dedicated persistent MCP session gets a default task; stateless HTTP requires an explicit ID (`task_required`). Scripts that reconnect and agents sharing a session must pass a unique, consistent ID on every call. An explicit task survives reconnects while this host process lives.
- When an MCP session is deleted (HTTP `DELETE`, as MCP clients do when they close), its current default task ends as soon as none of its calls is in flight and no `vm_end_turn` of it runs: its pending waits are cancelled, its mirror plans and observations dropped and its VM ownership released, and the task is forgotten (its ID, passed explicitly later, starts a fresh task). Its temporary checkpoints are left in place: nobody can end that task any more, so `vm_end_turn` with `all_temp` (or `vm_checkpoint_delete`) removes them. A client that opens one session per call therefore gets a new default task for each call that cannot hold anything across calls; to keep ownership, observations and temporary checkpoints across calls it must pass an explicit `task_id`. Explicit tasks are never ended by a session ending. Sessions have no idle timeout: a client that dies without deleting its session (a killed script) leaves that session and its default task alive, and its ownership stays until someone calls `vm_end_turn` with that task's ID (`vm_busy` names it and its idle time).
- Observations, pending waits and temporary checkpoints belong to that task. Resolved tool results include `task_id` and the task's `run_id`. Observations cannot be shared between tasks.
- The first write call reserves the VM for that task until successful `vm_end_turn` cleanup. Another task's write receives `vm_busy` with `owner_task_id`, `owner_idle_ms` (milliseconds since a call of the owner last started or returned; `0` while one is in flight) and `owner_in_flight` (the owner's calls in progress, on any VM); read-only calls remain available. `vm_status` reports the same facts as `owner` (see 3.5). There is no automatic idle takeover of explicit tasks: an owner that is abandoned (no call in flight, long idle, e.g. a crashed script) is released by calling `vm_end_turn` with its `task_id` and the `vm`, which deletes that task's temporary checkpoints on the VM like the owner's own cleanup would; `vm_busy`'s `next` says so. IDs express ownership, not authentication.
- `vm_end_turn` blocks new calls while cleanup runs (`task_busy`), cancels the task's waits and waits for its accepted calls before deleting temporary checkpoints and releasing ownership. With `vm`, only that VM's resources are cleaned; without it the task ends. Failed cleanup retains ownership for retry. An ended explicit ID rejects new work (`task_ended`); the next ordinary call in a default session gets a new task.

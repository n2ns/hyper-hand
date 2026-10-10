# 6. Files

Part of [HyperHand features](../features.md). Section numbers such as 8.6 refer to the chapters listed there.

`vm_push` and `vm_pull` need the agent (`agent_required` otherwise). Missing `host_path` or `guest_path` is `invalid_argument`. File contents are streamed in 1 MB buffers on both sides and are not held in memory.

### 6.1 vm_push

`vm_push` copies `host_path` (a file, or a directory recursively) into the guest at `guest_path`.

- Single file:
  - If `guest_path` ends in `\` or `/`, the file is written into that directory under its own name.
  - Otherwise `guest_path` is the destination file path.
- Directory: every file below `host_path` is written to `guest_path\<relative path>`. Only files are transferred; empty directories are not created. The host walk does not descend into symbolic links to directories.
- Parent directories in the guest are created as needed.
- Result: `{"copied": 12, "skipped": 30, "bytes": 48213760}` (files copied, unchanged files skipped, bytes sent).
- On failure the error's reason names the guest path and the field `copied` says how many files were copied before it; those files remain.

### 6.2 Unchanged-file skipping

Unless `force` is `true` (default `false`):

- The host asks the agent for the SHA-256 of every destination path (`hash_files`, in batches of 1000 paths). The agent reports an empty hash for a missing, directory or unreadable path.
- For each destination with a non-empty guest hash, the host computes the local file's SHA-256; if they match, the file is skipped.
- An agent that does not support `hash_files` (`unknown op`) receives every file.

With `force` = `true` no hashes are requested and every file is uploaded.

### 6.3 vm_pull

`vm_pull` copies `guest_path` (a file, or a directory recursively) to `host_path` on the host.

- The host first lists `guest_path` as a directory; if the listing fails, `guest_path` is treated as a file.
- Single file:
  - If `host_path` is an existing directory, or ends in `\` or `/`, the file is written into it under the guest file's name.
  - Otherwise `host_path` is the destination file path.
  - Parent directories on the host are created as needed.
- Directory: `host_path` is created and the tree is copied recursively. Empty guest directories are created on the host.
- Symbolic links, junctions (for example `Application Data`) and other irregular entries in guest directories are skipped.
- Reading a guest directory as a file fails with `<path> is a directory`.
- Result: `{"files": 3, "bytes": 10240, "host_path": "C:\\temp\\reports"}`.
- On failure the error's reason names the guest path and the field `files` says how many files were copied before it.

### 6.4 Temporary .hhpart files

Both directions write each file to a unique `.hyperhand-*.hhpart` temporary file in the destination directory and rename it over the destination only after the whole file has been received, replacing an existing file. On any error only that transfer's temporary file is deleted and the destination is left unchanged. In the guest, a short or failed transfer is never renamed into place.

### 6.5 Directory mirror

`vm_push` accepts `mode: "copy"` (default, the behavior in 6.1–6.2) or `mode: "mirror"`. Mirror accepts a source directory only and synchronizes its contents into the guest destination root; it does not add the source directory's basename. `phase: "plan"` is the default for mirror. Copy rejects `phase` and `plan_id`.

- **Plan:** completely scan both trees, hash regular files and return `status: "planned"`, `plan_id`, resolved `vm`, `host_path`, `guest_path`, `force`, `expires_at`, `summary` counts, ordered `changes` and `next`, which states that nothing was written yet and spells out the apply call (`phase: "apply"`, this `plan_id`, the same `vm`, `host_path`, `guest_path`, `force` value and `task_id`, before `expires_at`). Each change has `action` (`mkdir`, `copy`, `overwrite`, `skip`, `delete_file`, `delete_dir`) and relative `path`; `mkdir` of `.` creates a missing destination root. Filesystem names are data, not instructions. Plan does not modify either tree or claim VM write ownership.
- **Apply:** pass `phase: "apply"`, the returned `plan_id`, and the same paths, `force` and task identity. A plan is bound to the task's run and the VM ID, expires in 10 minutes and is consumed by an apply attempt. Only the newest 16 unexpired plans are retained server-wide; `vm_end_turn` discards that task's plans within its cleanup scope, and host restart loses them. An unavailable, mismatched or consumed plan returns `plan_stale`.
- Before transfer, apply rescans both trees and refuses content/type/existence drift. The host captures and verifies the planned changed files into a temporary payload. The guest receives the full payload, verifies its sizes/hashes and rescans the destination before mutations. Directories are created, changed files are replaced through unique `.hhpart` files and verified, and only then are planned extra files deleted and extra directories removed bottom-up when empty. A final full scan must match the source manifest. Empty source means removing every entry inside the target, preserving the root.
- `force: true` copies identical files too; it never expands the deletion scope. Matching paths are case-insensitive; existing spelling is preserved. File/directory type conflicts are refused, not automatically replaced. Mirror does not synchronize ACLs, timestamps, alternate data streams or other metadata.
- Roots must be local directories, not volume roots, UNC/device paths or paths containing reparse points. Entries and ancestors containing symbolic links, junctions or other reparse points are refused. The source must exist; the destination may be absent if its parent exists. Unsupported names, ambiguous case-colliding names, scan errors, more than 4096 entries or more than 1 MiB of manifest JSON per side stop planning; inventories are never silently truncated. Large deployments must be split into separate directory roots.
- Full ordinary-copy and mirror calls are serialized per VM, including across agent connection replacement. Apply is one streamed guest request. External programs and independent host clients can still change files: checks reduce this race, but the operation does not lock the entire directory or promise atomic replacement. A partial apply is not rolled back.
- Success is `status: "complete"` with `plan_id`, `vm`, `completed` changes and empty `pending`. A confirmed failure returns `isError` with `error: "mirror_partial"` (or `"plan_stale"` before mutation), `status`, `completed`, `failed: {path, reason}` and `pending`. If the response cannot be confirmed, `error: "mirror_unknown"`, `status: "unknown"` and `unknown` changes are returned; no completion is inferred. Create a new plan after a failure or uncertain outcome; never replay the consumed apply.
- Changed bytes use temporary disk space: one payload on the host, the received payload plus staged files on the guest, and a per-file replacement alongside the target. Handled errors/cancellation clean temporary files; abrupt process termination can leave temporary artifacts. A locked destination can fail without deleting later extras. Ordinary copy/pull keep their existing results. A guest without `mirror_scan`/`mirror_apply` returns `agent_outdated`; mirror never falls back to copy.

The same inventory limits also apply to the intermediate tree after copying but before deletion (the union of source entries and target-only entries). Planning refuses a union that would exceed those limits even if each side separately fits.

Because mirror apply plans are single-use, the `vm_push` tool no longer advertises `idempotentHint`, even though ordinary copy still has its previous semantics. Its static annotation remains destructive; mirror plan is read-only at execution time.

### 6.6 vm_file_info

`vm_file_info` reads facts about guest files and directories without copying or changing anything, for example to verify an installation. It needs the agent and no write ownership (read-only, idempotent).

- `paths` holds 1 to 64 guest paths; an empty list, more than 64 entries or an empty entry is `invalid_argument` before the agent is asked.
- The agent expands environment variables (`%APPDATA%`, `%USERPROFILE%`, ...) with `ExpandEnvironmentStrings` in its own process, which runs as the logged-on user; an unknown variable stays as written. A path that is not absolute after expansion gets an `error` and nothing else is read.
- Result: `{"files": [{"path": "%APPDATA%\App\app.dll", "resolved": "C:\Users\tester\AppData\Roaming\App\app.dll", "exists": true, "type": "file", "size": 123456, "sha256": "<lowercase hex>", "version": "1.2.3.4", "modified": "2026-10-11T09:05:07Z"}]}`, one entry per path in request order. `path` is as given, `resolved` is the expanded and cleaned path.
- `type` is `file` or `dir` and present only when `exists`. Files get `size` (bytes, also `0`), `sha256` and `modified` (RFC 3339, UTC, last write time); directories get `modified` only. Symbolic links and junctions are followed.
- `version` is the `ProductVersion` string of the file's version resource (`GetFileVersionInfo`/`VerQueryValue`; the resource's translations, then `040904b0` and `040904e4`); it is omitted for files without one.
- A missing path is `exists: false` with no other fields, not an error. A file that cannot be opened (locked, access denied) is `exists: true` with `size`, `modified` and an `error` string and no `sha256`; a file larger than 1 GiB is not hashed and gets an `error`. A path that cannot be checked for another reason (for example an invalid name) is `exists: false` with an `error`.
- The agent reads each file in 1 MB buffers; cancelling the call stops hashing. An older agent is refused with `agent_outdated` (8.6).

# hyperhand_client.py

A small official client for scripts and AI agents that call HyperHand without an MCP connection of their own. It is one file, Python 3.8 or later, standard library only. Copy it or import it from this directory.

It takes care of what every caller otherwise rebuilds:

- **Task ID.** Every call carries one stable `task_id`, so VM write ownership, observations and temporary checkpoints carry over between calls and reconnects. The module generates the ID per client object. The command line reads it from `--task`, `--task-file` or `HYPERHAND_TASK_ID`.
- **Arguments.** Arguments go in as `key=value`. A value that parses as JSON is used as JSON; anything else is a string, so Windows paths need no escaping. `@file.json` or `-` (stdin) take a JSON object.
- **UTF-8.** Arguments and output are UTF-8 on every console.
- **Errors.** An error result becomes `HyperHandError` (module) or exit code 1 with the error object printed (command line).
- **Images.** PNG images are saved to files; the path is in the result as `_image` (the last one) and all paths in order as `_images` (a `vm_batch` can return several).
- **Controls.** Control trees are parsed into dicts, and controls can be selected by type, name, AutomationID, value, state, action or a point.

## Command line

```text
python client/hyperhand_client.py [--vm VM] [--task ID | --task-file FILE] [--out DIR] [--find COND=VALUE ...] [--pretty] TOOL [key=value ... | @args.json | -]
python client/hyperhand_client.py tools                 # live tool list with input schemas
python client/hyperhand_client.py new-task deploy       # prints a fresh ID such as task-deploy-3f2a9c01
```

It prints one JSON object on stdout: the tool's result, or its error object.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | Success. |
| 1 | The tool refused. Read `error`, `reason` and `next`. |
| 2 | Usage, connection or protocol problem. The message is on stderr. |

### One piece of work, many calls

Use one task file per piece of work. All calls then share ownership and observations, and `vm_end_turn` with the same task file ends the work:

```text
python client/hyperhand_client.py --vm Win10 --task-file temp/hh-task vm_exec command=hostname cwd=C:\
python client/hyperhand_client.py --vm Win10 --task-file temp/hh-task vm_checkpoint label=before-install
python client/hyperhand_client.py --vm Win10 --task-file temp/hh-task --find control_type=Edit --find focused=true vm_observe handle=459626 controls=true
python client/hyperhand_client.py --vm Win10 --task-file temp/hh-task vm_end_turn
```

`--find` replaces the result's `controls` text with `matches`: a list of the matching controls with `index`, `control_type`, `name`, `automation_id`, `x`, `y`, `width`, `height`, `center`, `enabled`, `offscreen`, `focused`, `value`, `actions` and `state`.

Without a task, a call runs in its own MCP session's default task. That task ends when the call returns, so nothing carries over to the next call.

### Long commands

Start the command in the background and collect the result, even from another process later:

```text
python client/hyperhand_client.py --vm Win10 --task-file temp/hh-task vm_exec background=true timeout_ms=1800000 command="& C:\tools\check.ps1"
python client/hyperhand_client.py --vm Win10 vm_job job_id=job-3f2a9c01b7de wait_ms=30000 stdout_offset=0 stderr_offset=0
```

Repeat the second call, passing the previous `stdout_next` and `stderr_next` as the offsets, until `complete` is `true`. Reading a job needs no task.

### Several steps in one call

`vm_batch` runs steps in order and stops at the first failure; `${<step>.<path>}` passes a value of an earlier result. Put the steps in a JSON file:

```json
{"steps": [
  {"tool": "vm_launch", "args": {"path": "notepad.exe"}},
  {"tool": "vm_type", "args": {"handle": "${0.handle}", "text": "hello"}},
  {"tool": "vm_observe", "args": {"handle": "${0.handle}"}}
]}
```

```text
python client/hyperhand_client.py --vm Win10 --task-file temp/hh-task vm_batch @steps.json
```

The HTTP timeout is the sum of the steps' timeouts, so long steps need no `--timeout`.

## Module

```python
import sys; sys.path.insert(0, r"D:\_Dev\WorkSpace\HyperHand\client")
from hyperhand_client import HyperHand, HyperHandError

with HyperHand(vm="Win10") as hh:            # task_id: hh.task_id (generated); pass task_id=... to continue one
    r = hh.call("vm_observe", handle=459626, controls=True, max_size=1280)
    print(r["_image"], r["observation_id"])
    edit = hh.find_one(r, control_type="Edit", automation_id="1001")    # ValueError unless exactly one matches
    hh.call("vm_type", observation_id=r["observation_id"], index=edit["index"], text="acad.dwt\n")

    job = hh.call("vm_exec", background=True, command="& C:\\tools\\check.ps1", timeout_ms=1800000)
    out, offsets = "", {"stdout_offset": 0, "stderr_offset": 0}
    while True:
        j = hh.call("vm_job", job_id=job["id"], wait_ms=30000, **offsets)
        out += j["stdout"]
        offsets = {"stdout_offset": j["stdout_next"], "stderr_offset": j["stderr_next"]}
        if j["complete"]:
            break
    hh.end_turn()                            # release the VM, delete this task's temporary checkpoints
```

API summary:

| Function | Behavior |
|---|---|
| `HyperHand(vm=None, task_id=None, url=None, out_dir=None, timeout=120)` | Opens one MCP session on first use. `task_id=""` sends no task ID. `out_dir` defaults to `./temp/hyperhand`. The HTTP timeout grows to fit `timeout_ms` and `wait_ms` (summed over the steps of `vm_batch`). `close()`, or leaving the `with` block, deletes the session but does not end the task. |
| `call(tool, arguments=None, /, *, http_timeout=None, **kwargs)` | Tool arguments come from the optional positional dict and from keywords; every tool argument name works as a keyword, including `args` (`hh.call("vm_launch", path=..., args=["/nologo"])`). `http_timeout` (seconds) overrides the HTTP timeout. Returns the result dict. Raises `HyperHandError` (`code`, `reason`, `next`, `obj`) on a tool error and `TransportError` when the server cannot be reached. |
| `end_turn(vm=None, **kwargs)` | `vm_end_turn` for this task. |
| `tools()` | The live tool list. |
| `parse_controls(text_or_result)` | Parses a control tree into dicts. |
| `find_controls(tree, **conditions)` and `find_one(...)` | Select controls. Conditions are `control_type`, `name`, `name_contains`, `automation_id`, `value`, `value_contains`, `focused`, `enabled`, `action`, and `near=(x, y)`, a screen point inside the control. |

The server address is `http://127.0.0.1:8770/mcp` unless `url`, `--url` or `HYPERHAND_URL` says otherwise.

Tests: `python -m unittest discover -s client`. They use a stub MCP server and need no VM.

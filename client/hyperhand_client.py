#!/usr/bin/env python3
"""Minimal HyperHand client for scripts and AI agents: Python 3.8+, standard library only.

As a module::

    from hyperhand_client import HyperHand, HyperHandError
    with HyperHand(vm="Win10") as hh:                    # one MCP session, one stable task_id
        r = hh.call("vm_observe", handle=459626, controls=True)
        print(r["_image"])                                # the PNG was saved to a file
        i = hh.find_one(r, control_type="Edit", automation_id="cmdline")["index"]
        hh.call("vm_type", observation_id=r["observation_id"], index=i, text="_line\\n")
        hh.end_turn()                                     # release the VM, delete this task's temp checkpoints

As a command (one JSON object per call on stdout, UTF-8)::

    python hyperhand_client.py --vm Win10 --task-file .hh-task vm_exec command=hostname cwd=C:\\
    python hyperhand_client.py --vm Win10 --task-file .hh-task vm_exec @args.json
    python hyperhand_client.py --vm Win10 --task-file .hh-task --find control_type=Edit vm_observe handle=459626 controls=true
    python hyperhand_client.py tools

Arguments are key=value pairs (a value that parses as JSON is used as JSON, anything else is a string, so Windows
paths need no escaping), @file.json, or - (a JSON object on stdin). Exit code 0: result printed; 1: the tool returned
an error object (printed); 2: usage, connection or protocol error (message on stderr).
"""

import argparse
import base64
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
import uuid

__all__ = ["HyperHand", "HyperHandError", "parse_controls", "find_controls", "new_task_id"]

DEFAULT_URL = "http://127.0.0.1:8770/mcp"
PROTOCOL_VERSION = "2025-06-18"


class HyperHandError(Exception):
    """A tool's structured refusal: code ("vm_busy", "stale_observation", ...), reason, next and the other fields."""

    def __init__(self, tool, obj):
        self.tool = tool
        self.obj = obj
        self.code = obj.get("error", "failed")
        self.reason = obj.get("reason", "")
        self.next = obj.get("next", "")
        super().__init__(f"{tool}: {self.code}: {self.reason}" + (f" (next: {self.next})" if self.next else ""))


class TransportError(Exception):
    """The MCP server could not be reached, timed out or answered something that is not a tool result."""


class _SessionGone(TransportError):
    """The server does not know the session (it restarted): the request was not executed."""


# Minimum HTTP timeouts (seconds) of tools that can take minutes besides their timeout_ms / wait_ms.
_SLOW = {"vm_end_turn": 1800, "vm_checkpoint_delete": 1800, "vm_checkpoint": 600, "vm_restore": 600, "vm_start": 300,
         "vm_shutdown": 300, "vm_turn_off": 120, "vm_save": 420, "vm_pause": 120, "vm_install_agent": 300, "vm_update_agent": 180, "vm_push": 3600,
         "vm_pull": 3600}


def _needed_timeout(tool, a):
    """Seconds a call may take: its timeout_ms / wait_ms plus a margin, at least the tool's minimum in _SLOW; for
    vm_batch the sum over its steps."""
    if tool == "vm_batch":
        return sum(_needed_timeout(s.get("tool"), s.get("args") or {}) for s in a.get("steps") or [] if isinstance(s, dict))
    longest = max(int(a.get("timeout_ms") or 0), int(a.get("wait_ms") or 0)) / 1000
    return max(longest + 30, _SLOW.get(tool, 0))


def new_task_id(purpose=""):
    """A fresh task ID such as task-deploy-3f2a9c01 (task-3f2a9c01 without purpose); pass the same one on every call
    of one piece of work."""
    purpose = re.sub(r"[^A-Za-z0-9_.-]+", "-", purpose).strip("-")
    return f"task-{purpose}-{uuid.uuid4().hex[:8]}" if purpose else f"task-{uuid.uuid4().hex[:8]}"


# ---------------------------------------------------------------------------------------------------------------------
# Control trees: vm_observe / vm_find_controls render one control per line:
#   [12] Edit "File name:" id=1001 (848,710 318x15) focused value="acad.dwt" actions=[SetValue] state={...}

_GO_ESCAPES = {"a": "\a", "b": "\b", "f": "\f", "n": "\n", "r": "\r", "t": "\t", "v": "\v", "\\": "\\", '"': '"', "'": "'"}
_QUOTED = r'"(?:[^"\\]|\\.)*"'
# An unquoted id has no space, tab, newline or quote (the server quotes those) but may hold other characters.
_LINE = re.compile(r"^(?P<indent> *)\[(?P<index>\d+)\] (?P<type>\S+) (?P<name>" + _QUOTED + r")"
                   r"(?: id=(?P<id>" + _QUOTED + r"|[^ \t\n\"]+))? \((?P<x>-?\d+),(?P<y>-?\d+) (?P<w>-?\d+)x(?P<h>-?\d+)\)(?P<rest>.*)$",
                   re.DOTALL)
# After the rect come, in this order: state words, value="...", actions=[...], state={...}. The first " value=" is the
# value (state words contain no quotes); actions and state are looked for only after it.
_VALUE = re.compile(r" value=(" + _QUOTED + r")")
_TAIL = re.compile(r"^(?: actions=\[(?P<actions>[^\]]*)\])?(?: state=(?P<state>\{.*\}))?\s*$", re.DOTALL)


def _go_unquote(s):
    """Decode a Go %q string literal (the server renders names and values with it)."""
    body, out, i = s[1:-1], [], 0
    while i < len(body):
        c = body[i]
        if c != "\\":
            out.append(c)
            i += 1
            continue
        e = body[i + 1]
        if e in _GO_ESCAPES:
            out.append(_GO_ESCAPES[e])
            i += 2
        elif e in "xuU":
            n = {"x": 2, "u": 4, "U": 8}[e]
            out.append(chr(int(body[i + 2:i + 2 + n], 16)))
            i += 2 + n
        else:  # octal \NNN
            out.append(chr(int(body[i + 1:i + 4], 8)))
            i += 4
    return "".join(out)


def parse_controls(controls):
    """Parse a control tree (the text of a result's "controls", or a result dict) into a list of dicts:
    index, depth, control_type, name, automation_id, x, y, width, height, center (x, y), enabled, offscreen, focused,
    value (None when the control has none), actions, state, line."""
    if isinstance(controls, dict):
        controls = controls.get("controls") or ""
    nodes = []
    # Lines end only at "\n": an unquoted id or a name may hold other line-break characters.
    for line in controls.split("\n"):
        m = _LINE.match(line.rstrip("\r"))
        if not m:
            continue
        rest = m["rest"]
        vm = _VALUE.search(rest)
        head, tail = (rest[:vm.start()], rest[vm.end():]) if vm else (rest, rest)
        if not vm:  # no value: the state words end where actions or state begin
            cut = min([i for i in (rest.find(" actions=["), rest.find(" state={")) if i >= 0] or [len(rest)])
            head, tail = rest[:cut], rest[cut:]
        words = head.split()
        aid = m["id"]
        if aid and aid.startswith('"'):
            aid = _go_unquote(aid)
        x, y, w, h = (int(m[k]) for k in ("x", "y", "w", "h"))
        t = _TAIL.match(tail)
        am = t and t["actions"]
        sm = t and t["state"]
        nodes.append({
            "index": int(m["index"]), "depth": len(m["indent"]) // 2, "control_type": m["type"], "name": _go_unquote(m["name"]),
            "automation_id": aid or "", "x": x, "y": y, "width": w, "height": h, "center": (x + w // 2, y + h // 2),
            "enabled": "disabled" not in words, "offscreen": "offscreen" in words, "focused": "focused" in words,
            "value": _go_unquote(vm.group(1)) if vm else None,
            "actions": [a for a in am.split(",") if a] if am else [],
            "state": json.loads(sm) if sm else None, "line": line,
        })
    return nodes


def find_controls(controls, *, control_type=None, name=None, name_contains=None, automation_id=None, value=None,
                  value_contains=None, focused=None, enabled=None, near=None, action=None):
    """Controls of a tree (text, result dict or parse_controls list) matching every given condition (exact unless
    *_contains; near=(x, y) is a screen point inside the control's rect; action is one of its actions)."""
    nodes = controls if isinstance(controls, list) else parse_controls(controls)
    out = []
    for n in nodes:
        if control_type is not None and n["control_type"] != control_type:
            continue
        if name is not None and n["name"] != name:
            continue
        if name_contains is not None and name_contains not in n["name"]:
            continue
        if automation_id is not None and n["automation_id"] != automation_id:
            continue
        if value is not None and n["value"] != value:
            continue
        if value_contains is not None and (n["value"] is None or value_contains not in n["value"]):
            continue
        if focused is not None and n["focused"] != focused:
            continue
        if enabled is not None and n["enabled"] != enabled:
            continue
        if action is not None and action not in n["actions"]:
            continue
        if near is not None:
            px, py = near
            if not (n["x"] <= px < n["x"] + n["width"] and n["y"] <= py < n["y"] + n["height"]):
                continue
        out.append(n)
    return out


# ---------------------------------------------------------------------------------------------------------------------
# MCP over streamable HTTP


class HyperHand:
    """One MCP session to the local HyperHand server; every call carries this client's vm (unless given) and task_id.

    task_id defaults to a new ID per client object: calls of this object share VM write ownership, observations and
    temporary checkpoints, also across reconnects. Pass the same task_id to continue a task from another process;
    pass "" to send none (the MCP session's default task, which ends when close() deletes the session).
    Results are dicts; a PNG image item is saved under out_dir and its path put in result["_image"]. A tool error
    raises HyperHandError; transport problems raise TransportError.
    """

    def __init__(self, vm=None, task_id=None, url=None, out_dir=None, timeout=120):
        self.url = url or os.environ.get("HYPERHAND_URL", DEFAULT_URL)
        self.vm = vm
        self.task_id = new_task_id() if task_id is None else task_id
        self.out_dir = out_dir or os.path.join(os.getcwd(), "temp", "hyperhand")
        self.timeout = timeout
        self._session = None
        self._protocol = None
        self._next_id = 1

    # -- context manager: closes the MCP session only; ending the task (vm_end_turn) is explicit.
    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()

    def close(self):
        """Delete the MCP session. The task (ownership, temp checkpoints) stays for its task_id; see end_turn."""
        if self._session:
            req = urllib.request.Request(self.url, method="DELETE", headers=self._headers())
            try:
                urllib.request.urlopen(req, timeout=5).close()
            except (urllib.error.URLError, OSError):
                pass
            self._session = None

    def _headers(self):
        h = {"Accept": "application/json, text/event-stream", "Content-Type": "application/json"}
        if self._session:
            h["Mcp-Session-Id"] = self._session
        if self._protocol:
            h["MCP-Protocol-Version"] = self._protocol
        return h

    def _post(self, message, timeout):
        data = json.dumps(message, ensure_ascii=False).encode("utf-8")
        req = urllib.request.Request(self.url, data=data, method="POST", headers=self._headers())
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                sid = resp.headers.get("Mcp-Session-Id")
                if sid:
                    self._session = sid
                body = resp.read().decode("utf-8")
                ctype = resp.headers.get("Content-Type", "")
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8", "replace")[:500]
            if e.code == 404 and self._session and message.get("method") != "initialize":
                raise _SessionGone(f"HTTP 404 from {self.url}: {text}") from None
            raise TransportError(f"HTTP {e.code} from {self.url}: {text}") from None
        except (urllib.error.URLError, OSError) as e:  # includes timeouts while waiting for the answer
            raise TransportError(f"no answer from HyperHand at {self.url}: {e}; is the HyperHand tray/service running? "
                                 "A timed-out call may still have run: observe before repeating it") from None
        if "id" not in message:
            return None
        try:
            if ctype.startswith("text/event-stream"):
                payloads = []
                for event in re.split(r"\r?\n\r?\n", body):
                    # Only "\n" ends an SSE line here: JSON text may hold other line-break characters.
                    lines = [l.rstrip("\r")[5:].lstrip(" ") for l in event.split("\n") if l.startswith("data:")]
                    if lines:
                        payloads.append(json.loads("\n".join(lines)))
            else:
                payloads = [json.loads(body)] if body.strip() else []
        except ValueError as e:
            raise TransportError(f"unreadable MCP response from {self.url}: {e}: {body[:300]!r}") from None
        for p in payloads:
            if isinstance(p, dict) and p.get("id") == message["id"]:
                if "error" in p:
                    raise TransportError(f"MCP error: {json.dumps(p['error'], ensure_ascii=False)}")
                return p.get("result")
        raise TransportError(f"no MCP response for request {message['id']}")

    def _request(self, method, params, timeout):
        for attempt in range(2):
            if not self._session:
                self._open()
            msg = {"jsonrpc": "2.0", "id": self._next_id, "method": method, "params": params}
            self._next_id += 1
            try:
                result = self._post(msg, timeout)
            except _SessionGone:
                # The server restarted and forgot the session; the request did not run, so open a new one and retry.
                self._session = None
                if attempt:
                    raise
                continue
            if not isinstance(result, dict):
                raise TransportError(f"unexpected MCP result for {method}: {result!r}"[:500])
            return result

    def _open(self):
        self._protocol = None
        init = self._post({"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {
            "protocolVersion": PROTOCOL_VERSION, "capabilities": {}, "clientInfo": {"name": "hyperhand_client.py", "version": "1"}}},
            self.timeout)
        if not isinstance(init, dict):
            raise TransportError(f"unexpected initialize result: {init!r}"[:500])
        self._protocol = init.get("protocolVersion", PROTOCOL_VERSION)
        self._post({"jsonrpc": "2.0", "method": "notifications/initialized"}, self.timeout)

    def tools(self):
        """The live tool list (name, description, inputSchema, annotations)."""
        return self._request("tools/list", {}, self.timeout)["tools"]

    def call(self, tool, arguments=None, /, *, http_timeout=None, **kwargs):
        """Call a tool and return its JSON result as a dict (plus "_image": the saved PNG path, when there was one, and
        "_images": every saved PNG path in order, for a vm_batch with several images).

        Arguments come from an optional positional dict and keyword arguments, so every tool argument name works as a
        keyword (vm_launch args=[...] included); vm and task_id are added unless given. http_timeout (seconds) is the
        HTTP timeout; it defaults to the client's, raised to fit timeout_ms / wait_ms arguments of long calls (summed over
        the steps of a vm_batch)."""
        a = dict(arguments or {})
        a.update(kwargs)
        if self.vm is not None and tool != "vm_list":
            a.setdefault("vm", self.vm)
        if self.task_id:
            a.setdefault("task_id", self.task_id)
        if http_timeout is None:
            http_timeout = max(self.timeout, _needed_timeout(tool, a))
        return self._result(tool, self._request("tools/call", {"name": tool, "arguments": a}, http_timeout))

    def _result(self, tool, result):
        obj, images = None, []
        for item in result.get("content", []):
            if item.get("type") == "image":
                os.makedirs(self.out_dir, exist_ok=True)
                image = os.path.join(self.out_dir, f"{tool}-{time.strftime('%H%M%S')}-{uuid.uuid4().hex[:6]}.png")
                images.append(image)
                with open(image, "wb") as f:
                    f.write(base64.b64decode(item["data"]))
            elif item.get("type") == "text":
                try:
                    obj = json.loads(item["text"])
                except ValueError:
                    obj = {"text": item["text"]}
        if obj is None:
            obj = {}
        if images:
            obj["_image"] = images[-1]
            obj["_images"] = images
        if result.get("isError"):
            raise HyperHandError(tool, obj)
        return obj

    def find(self, result, **conditions):
        """find_controls on a result's control tree."""
        return find_controls(result, **conditions)

    def find_one(self, result, **conditions):
        """The single control matching conditions; ValueError (listing the candidates) when there is none or several."""
        found = find_controls(result, **conditions)
        if len(found) != 1:
            lines = "\n".join(n["line"] for n in found[:10])
            raise ValueError(f"{len(found)} controls match {conditions}" + (f":\n{lines}" if lines else ""))
        return found[0]

    def end_turn(self, vm=None, **kwargs):
        """vm_end_turn for this task: without vm the whole task ends (all its VMs), with vm only that VM is cleaned."""
        a = dict(kwargs)
        if vm is not None:
            a["vm"] = vm
        if self.task_id:
            a["task_id"] = self.task_id
        # Deleting checkpoints merges disks and can take minutes.
        return self._result("vm_end_turn", self._request("tools/call", {"name": "vm_end_turn", "arguments": a}, max(self.timeout, _SLOW["vm_end_turn"])))


# ---------------------------------------------------------------------------------------------------------------------
# Command line


def _parse_args(items):
    if len(items) == 1 and items[0] == "-":
        return json.loads(sys.stdin.buffer.read().decode("utf-8-sig"))
    if len(items) == 1 and items[0].startswith("@"):
        with open(items[0][1:], encoding="utf-8-sig") as f:
            return json.load(f)
    if len(items) == 1 and items[0].lstrip().startswith("{"):
        return json.loads(items[0])
    out = {}
    for it in items:
        k, sep, v = it.partition("=")
        if not sep or not k:
            raise ValueError(f"argument {it!r}: expected key=value, @file.json, - or a JSON object")
        try:
            out[k] = json.loads(v)
        except ValueError:
            out[k] = v
    return out


_FIND_KEYS = {"control_type", "name", "name_contains", "automation_id", "value", "value_contains", "focused", "enabled", "action"}


def _task_from_file(path, purpose):
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            tid = f.read().strip()
        if tid:
            return tid
    tid = new_task_id(purpose)
    d = os.path.dirname(os.path.abspath(path))
    os.makedirs(d, exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(tid + "\n")
    return tid


def main(argv=None):
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8")
        except (AttributeError, ValueError):
            pass
    p = argparse.ArgumentParser(description="Call a HyperHand tool; prints the result as one JSON object.",
                                epilog="Commands besides tool names: tools (list tools), new-task [purpose] (print a fresh task ID).")
    p.add_argument("--vm", default=os.environ.get("HYPERHAND_VM"), help="VM added to every call (env HYPERHAND_VM)")
    g = p.add_mutually_exclusive_group()
    g.add_argument("--task", help="task_id for this call (default: env HYPERHAND_TASK_ID unless --task-file is given)")
    g.add_argument("--task-file", help="read the task_id from this file, creating it with a new ID the first time")
    p.add_argument("--url", default=None, help=f"MCP endpoint (env HYPERHAND_URL, default {DEFAULT_URL})")
    p.add_argument("--out", default=os.environ.get("HYPERHAND_OUT"), help="directory for PNG images (default ./temp/hyperhand)")
    p.add_argument("--timeout", type=float, default=120, help="HTTP timeout in seconds (raised for long timeout_ms/wait_ms)")
    p.add_argument("--find", action="append", default=[], metavar="COND=VALUE",
                   help="instead of the whole tree, put the controls matching these conditions in result['matches'] "
                        "(control_type, name, name_contains, automation_id, value, value_contains, focused, enabled, action)")
    p.add_argument("--pretty", action="store_true", help="indent the JSON")
    p.add_argument("tool")
    p.add_argument("args", nargs="*")
    ns = p.parse_args(argv)

    if ns.tool == "new-task":
        print(new_task_id(ns.args[0] if ns.args else ""))
        return 0
    try:
        args = _parse_args(ns.args)
        conditions = {}
        for cond in ns.find:
            k, sep, v = cond.partition("=")
            if not sep or k not in _FIND_KEYS:
                raise ValueError(f"--find {cond!r}: expected one of {', '.join(sorted(_FIND_KEYS))}=VALUE")
            # Conditions compare with the parsed tree's strings; only focused and enabled are booleans.
            conditions[k] = {"true": True, "false": False}.get(v.lower(), v) if k in ("focused", "enabled") else v
    except (ValueError, OSError) as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    # Without --task/--task-file/HYPERHAND_TASK_ID the call runs in its session's default task, which ends with this
    # process: nothing (ownership, observations, temp checkpoints) carries over to the next call.
    if ns.task_file:
        task = _task_from_file(ns.task_file, ns.tool)
    else:
        task = ns.task or os.environ.get("HYPERHAND_TASK_ID") or ""
    hh = HyperHand(vm=ns.vm, task_id=task, url=ns.url, out_dir=ns.out, timeout=ns.timeout)
    try:
        if ns.tool == "tools":
            out = {"tools": [{"name": t["name"], "description": t.get("description", ""), "inputSchema": t.get("inputSchema")} for t in hh.tools()]}
            code = 0
        else:
            try:
                out, code = hh.call(ns.tool, args), 0
            except HyperHandError as e:
                out, code = e.obj, 1
            if conditions and code == 0 and isinstance(out.get("controls"), str):
                out["matches"] = [{k: v for k, v in n.items() if k != "line"} for n in find_controls(out, **conditions)]
                del out["controls"]
    except TransportError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    finally:
        hh.close()
    print(json.dumps(out, ensure_ascii=False, indent=2 if ns.pretty else None))
    return code


if __name__ == "__main__":
    sys.exit(main())

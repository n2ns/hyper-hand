"""Tests for hyperhand_client.py against a stub MCP server: python -m unittest discover -s client"""
import base64
import contextlib
import io
import json
import os
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import hyperhand_client as hc  # noqa: E402

PNG = b"\x89PNG\r\n\x1a\nfake"


class Stub(BaseHTTPRequestHandler):
    """A minimal streamable-HTTP MCP server: records calls, answers tools/call from Stub.reply."""
    calls, deleted, sse = [], [], False
    reply = None
    forget = False  # answer the next tools/call with 404, as a restarted server does for an unknown session
    delay = 0

    def log_message(self, *a):
        pass

    def do_DELETE(self):
        Stub.deleted.append(self.headers.get("Mcp-Session-Id"))
        self.send_response(204)
        self.end_headers()

    def do_POST(self):
        msg = json.loads(self.rfile.read(int(self.headers["Content-Length"])).decode("utf-8"))
        if "id" not in msg:
            self.send_response(202)
            self.end_headers()
            return
        if msg["method"] == "initialize":
            result = {"protocolVersion": hc.PROTOCOL_VERSION, "capabilities": {}, "serverInfo": {"name": "stub"}}
        elif msg["method"] == "tools/list":
            result = {"tools": [{"name": "vm_list", "inputSchema": {}}]}
        else:
            assert self.headers.get("Mcp-Session-Id") == "S1", "session header missing"
            if Stub.forget:
                Stub.forget = False
                self.send_response(404)
                self.send_header("Content-Length", "17")
                self.end_headers()
                self.wfile.write(b"session not found")
                return
            if Stub.delay:
                import time
                time.sleep(Stub.delay)
            Stub.calls.append(msg["params"])
            result = Stub.reply(msg["params"])
        body = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}, ensure_ascii=False).encode("utf-8")
        self.send_response(200)
        self.send_header("Mcp-Session-Id", "S1")
        if Stub.sse:
            body = b"event: message\ndata: " + body + b"\n\n"
            self.send_header("Content-Type", "text/event-stream")
        else:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def text(obj, is_error=False, image=False):
    content = [{"type": "image", "mimeType": "image/png", "data": base64.b64encode(PNG).decode()}] if image else []
    content.append({"type": "text", "text": json.dumps(obj, ensure_ascii=False)})
    return {"content": content, "isError": is_error}


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Stub)
        cls.server.handle_error = lambda *a: None  # the timeout test's client hangs up before the late answer
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.url = f"http://127.0.0.1:{cls.server.server_port}/mcp"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        Stub.calls, Stub.deleted, Stub.sse, Stub.forget, Stub.delay = [], [], False, False, 0
        self.out = tempfile.mkdtemp()

    def test_new_session_after_server_restart_and_timeouts(self):
        Stub.reply = lambda p: text({"ok": True})
        hh = hc.HyperHand(vm="Win10", url=self.url, out_dir=self.out)
        hh.call("vm_status")
        Stub.forget = True
        self.assertEqual(hh.call("vm_status"), {"ok": True})  # re-initialized and sent once more
        self.assertEqual(len(Stub.calls), 2)
        Stub.delay = 2
        with self.assertRaises(hc.TransportError):
            hh.call("vm_status", timeout=0.5)

    def test_call_adds_vm_and_stable_task_and_saves_image(self):
        for sse in (False, True):
            Stub.sse, Stub.calls = sse, []
            Stub.reply = lambda p: text({"ok": True, "名称": "管道"}, image=p["name"] == "vm_observe")
            with hc.HyperHand(vm="Win10", url=self.url, out_dir=self.out) as hh:
                r = hh.call("vm_observe", handle=5)
                hh.call("vm_list")
            self.assertEqual(r["名称"], "管道")
            with open(r["_image"], "rb") as f:
                self.assertEqual(f.read(), PNG)
            a, b = Stub.calls[0]["arguments"], Stub.calls[1]["arguments"]
            self.assertEqual(a["vm"], "Win10")
            self.assertNotIn("vm", b)  # vm_list takes no vm
            self.assertTrue(a["task_id"].startswith("task-") and a["task_id"] == b["task_id"])
            self.assertEqual(Stub.deleted, ["S1"] * len(Stub.deleted))
            self.assertTrue(Stub.deleted)
            Stub.deleted = []

    def test_tool_error_raises_with_fields(self):
        Stub.reply = lambda p: text({"error": "vm_busy", "reason": "owned", "next": "wait", "owner_task_id": "t1"}, is_error=True)
        hh = hc.HyperHand(vm="Win10", url=self.url, out_dir=self.out)
        with self.assertRaises(hc.HyperHandError) as cm:
            hh.call("vm_exec", command="hostname")
        self.assertEqual((cm.exception.code, cm.exception.next, cm.exception.obj["owner_task_id"]), ("vm_busy", "wait", "t1"))

    def test_empty_task_id_sends_none_and_long_calls_get_longer_timeouts(self):
        Stub.reply = lambda p: text({"ok": True})
        hh = hc.HyperHand(vm="Win10", task_id="", url=self.url, out_dir=self.out)
        hh.call("vm_job", job_id="job-1", wait_ms=60000)
        self.assertNotIn("task_id", Stub.calls[0]["arguments"])
        self.assertEqual(hh.end_turn(vm="Win10"), {"ok": True})

    def test_transport_error(self):
        hh = hc.HyperHand(url="http://127.0.0.1:9/mcp", timeout=2)
        with self.assertRaises(hc.TransportError):
            hh.call("vm_list")

    def test_cli(self):
        Stub.reply = lambda p: text({"echo": p["arguments"], "controls": TREE})
        args_file = os.path.join(self.out, "a.json")
        with open(args_file, "w", encoding="utf-8") as f:
            json.dump({"command": "dir", "cwd": "C:\\Users"}, f)
        task_file = os.path.join(self.out, "task")
        outputs = []
        for argv in (["--url", self.url, "--vm", "Win10", "--task-file", task_file, "vm_exec", r"cwd=C:\temp\x", "timeout_ms=5000", "flag=true"],
                     ["--url", self.url, "--task-file", task_file, "vm_exec", "@" + args_file],
                     ["--url", self.url, "--find", "control_type=Edit", "--find", "focused=true", "vm_observe", '{"handle": 7}']):
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                self.assertEqual(hc.main(argv), 0)
            outputs.append(json.loads(buf.getvalue()))
        first, second, third = outputs
        self.assertEqual(first["echo"]["cwd"], "C:\\temp\\x")
        self.assertEqual((first["echo"]["timeout_ms"], first["echo"]["flag"], first["echo"]["vm"]), (5000, True, "Win10"))
        self.assertEqual(second["echo"]["cwd"], "C:\\Users")
        self.assertEqual(first["echo"]["task_id"], second["echo"]["task_id"])  # reused from the task file
        self.assertNotIn("task_id", third["echo"])  # no task given: the session's default task
        self.assertNotIn("controls", third)
        self.assertEqual([m["index"] for m in third["matches"]], [1])

        # --find values are strings (an AutomationID of digits); an explicit task file beats HYPERHAND_TASK_ID.
        os.environ["HYPERHAND_TASK_ID"] = "task-from-env"
        try:
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                self.assertEqual(hc.main(["--url", self.url, "--task-file", task_file, "--find", "automation_id=1001", "vm_observe", "handle=7"]), 0)
            out = json.loads(buf.getvalue())
            self.assertEqual([m["index"] for m in out["matches"]], [1])
            self.assertEqual(out["echo"]["task_id"], first["echo"]["task_id"])
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                hc.main(["--url", self.url, "vm_observe", "handle=7"])
            self.assertEqual(json.loads(buf.getvalue())["echo"]["task_id"], "task-from-env")
        finally:
            del os.environ["HYPERHAND_TASK_ID"]

        Stub.reply = lambda p: text({"error": "no_window", "reason": "gone"}, is_error=True)
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            self.assertEqual(hc.main(["--url", self.url, "vm_observe", "handle=1"]), 1)
        self.assertEqual(json.loads(buf.getvalue())["error"], "no_window")
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(hc.main(["--url", self.url, "vm_observe", "oops"]), 2)


TREE = """[0] Window "Select template" (631,263 658x514)
  [1] Edit "File \\"name\\":" id=1001 (848,710 318x15) focused value="acad.dwt" actions=[SetValue] state={"read_only":false}
  [2] Button "Open" id="ID with space" (900,740 75x23) disabled actions=[Invoke]
  [3] Edit "\\u7ba1\\t道" (10,10 100x20) value=" disabled offscreen"
    [4] ListItem "Autodesk.AutoCAD.Windows.Data.CommandHistory" (-5,-20 511x20) offscreen
"""


class ControlsTest(unittest.TestCase):
    def test_parse(self):
        n = hc.parse_controls(TREE)
        self.assertEqual([c["index"] for c in n], [0, 1, 2, 3, 4])
        self.assertEqual(n[1]["name"], 'File "name":')
        self.assertEqual((n[1]["automation_id"], n[1]["value"], n[1]["focused"], n[1]["actions"], n[1]["state"]),
                         ("1001", "acad.dwt", True, ["SetValue"], {"read_only": False}))
        self.assertEqual((n[2]["automation_id"], n[2]["enabled"], n[2]["value"]), ("ID with space", False, None))
        self.assertEqual((n[3]["name"], n[3]["enabled"], n[3]["offscreen"], n[3]["value"]), ("管\t道", True, False, " disabled offscreen"))
        self.assertEqual((n[4]["x"], n[4]["y"], n[4]["depth"], n[4]["offscreen"], n[4]["center"]), (-5, -20, 2, True, (250, -10)))

    def test_tricky_lines(self):
        tree = ('[5] Edit "a" (1,2 3x4) value="x state={y actions=[Invoke] focused" state={"read_only":true}\n'
                '[6] Button "b" id=ID\u00a0nbsp (1,2 3x4) actions=[Invoke]\n'
                '[7] Pane "c" id=a\x0bb (1,2 3x4) offscreen state={"offscreen":true}\n')
        n = {c["index"]: c for c in hc.parse_controls(tree)}
        self.assertEqual((n[5]["value"], n[5]["actions"], n[5]["state"], n[5]["focused"]),
                         ("x state={y actions=[Invoke] focused", [], {"read_only": True}, False))
        self.assertEqual((n[6]["automation_id"], n[6]["actions"]), ("ID\u00a0nbsp", ["Invoke"]))
        self.assertEqual((n[7]["automation_id"], n[7]["offscreen"], n[7]["state"]), ("a\x0bb", True, {"offscreen": True}))

    def test_find(self):
        self.assertEqual([c["index"] for c in hc.find_controls(TREE, control_type="Edit")], [1, 3])
        self.assertEqual([c["index"] for c in hc.find_controls({"controls": TREE}, near=(900, 715))], [0, 1])
        self.assertEqual([c["index"] for c in hc.find_controls(TREE, enabled=False)], [2])
        self.assertEqual([c["index"] for c in hc.find_controls(TREE, name_contains="name", action="SetValue")], [1])
        hh = hc.HyperHand(url="http://127.0.0.1:9/mcp")
        self.assertEqual(hh.find_one(TREE, automation_id="1001")["index"], 1)
        with self.assertRaises(ValueError):
            hh.find_one(TREE, control_type="Edit")


if __name__ == "__main__":
    unittest.main()

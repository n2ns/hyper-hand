"""Helpers for acceptance runners (see SKILL.md): the run's task and evidence directory, the temporary checkpoint and
its cleanup, checks against independent state and generic guest fixtures. Import from a runner started at the
repository root:

    import sys; sys.path.insert(0, ".agents/skills/vm-acceptance")
    from acceptance import Run, png_size

    run = Run("Win10", "child-limit")
    run.checkpoint()
    try:
        ...
        run.check("long list omitted", omitted == 1201, {"omitted": omitted})
    finally:
        run.cleanup()
        run.save()

The checkpoint's ID is kept in the evidence directory per VM, so a runner restarted after a crash (or a separate
cleanup-only call) still restores it; on a later day pass the run's date: Run("Win10", "child-limit", day="20261024").

Standard library only, like client/hyperhand_client.py.
"""
import datetime
import json
import os
import struct
import sys
import time

sys.path.insert(0, "client")
from hyperhand_client import HyperHand, HyperHandError, TransportError, new_task_id  # noqa: E402


def png_size(path):
    """(width, height) from the PNG header of a saved screenshot, independent of the tool's reported size."""
    with open(path, "rb") as f:
        head = f.read(24)
    if head[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError(f"{path} is not a PNG")
    return struct.unpack(">II", head[16:24])


class Run:
    """One acceptance run: evidence directory build/<item>-<yyyymmdd>/, one task (task file there) and the results."""

    def __init__(self, vm, item, timeout=300, day=None):
        """day (yyyymmdd) selects an earlier run's evidence directory; the default is today's."""
        self.vm = vm
        self.dir = os.path.join("build", f"{item}-{day or f'{datetime.date.today():%Y%m%d}'}")
        os.makedirs(self.dir, exist_ok=True)
        task_file = os.path.join(self.dir, "task")
        if not os.path.exists(task_file):
            with open(task_file, "w") as f:
                f.write(new_task_id(item))
        with open(task_file) as f:
            self.task_id = f.read().strip()
        self.hh = HyperHand(vm=vm, task_id=self.task_id, out_dir=self.dir, timeout=timeout)
        self.results = {}
        # Per VM: one run can check several VMs with the same task.
        self.state_file = os.path.join(self.dir, "checkpoint-" + "".join(c if c.isalnum() or c in "-_." else "_" for c in vm) + ".json")
        self.checkpoint_id = None
        self.checkpoints_before = None
        if os.path.exists(self.state_file):  # a previous process of this run took the checkpoint
            with open(self.state_file) as f:
                state = json.load(f)
            self.checkpoint_id, self.checkpoints_before = state["id"], state["before"]

    def call(self, tool, **args):
        return self.hh.call(tool, **args)

    def check(self, name, cond, detail):
        """Records one check; detail holds the independent evidence it was decided on."""
        self.results[name] = {"pass": bool(cond), "detail": detail}
        print(("PASS " if cond else "FAIL ") + name, json.dumps(detail, ensure_ascii=False, default=str)[:500])
        return bool(cond)

    def save(self, name="results"):
        path = os.path.join(self.dir, name + ".json")
        with open(path, "w") as f:
            json.dump(self.results, f, ensure_ascii=False, indent=1, default=str)
        passed = sum(r["pass"] for r in self.results.values())
        print(f"passed {passed} of {len(self.results)}; {path}")
        return passed == len(self.results)

    # Checkpoint and cleanup (Manual verification: before and after a run).

    def checkpoint(self, label="acceptance"):
        """Takes the temporary checkpoint once per run; a restarted runner reuses the one already taken."""
        if self.checkpoint_id:
            return {"id": self.checkpoint_id}
        before = sorted(c["id"] for c in self.call("vm_checkpoints")["checkpoints"])
        r = self.call("vm_checkpoint", label=label)
        self.checkpoint_id, self.checkpoints_before = r["id"], before
        with open(self.state_file, "w") as f:
            json.dump({"id": self.checkpoint_id, "before": before}, f)
        return r

    def cleanup(self):
        """Restores the temporary checkpoint and ends the task on this VM, then checks that every checkpoint the run
        added is gone and that a new task can write. A host reinstall during the run gives the task a new run ID, so
        vm_end_turn no longer owns the run's temp checkpoint; all_temp removes it then. Errors become a failed check."""
        try:
            if self.checkpoint_id:
                self.call("vm_restore", id=self.checkpoint_id)
            end = self.call("vm_end_turn")
            left = [c["id"] for c in self.call("vm_checkpoints")["checkpoints"]]
            if self.checkpoint_id in left:
                end = self.call("vm_end_turn", vm=self.vm, all_temp=True)
                left = [c["id"] for c in self.call("vm_checkpoints")["checkpoints"]]
            # Without a checkpoint of this run there is no "before" listing to compare with.
            added = sorted(set(left) - set(self.checkpoints_before)) if self.checkpoints_before is not None else []
            # The write check uses the MCP session's default task ("" task ID), which ends when the session closes,
            # so it does not keep the VM's write ownership.
            with HyperHand(vm=self.vm, task_id="") as probe:
                host = probe.call("vm_exec", command="hostname")
        except HyperHandError as e:
            return self.check("cleanup", False, {"error": e.obj})
        except TransportError as e:  # the host service restarted or a call timed out: keep the evidence (save) going
            return self.check("cleanup", False, {"transport_error": str(e)})
        ok = self.check("cleanup", not added and not end["errors"] and not end["skipped"] and host["exit_code"] == 0,
                        {"end_turn": end, "checkpoints_added_and_left": added})
        if ok and os.path.exists(self.state_file):
            os.remove(self.state_file)
        return ok

    # Independent state in the guest.

    def powershell(self, command):
        return self.call("vm_exec", command=command, shell="powershell")["stdout"].strip()

    def guest_cursor(self):
        """The guest's cursor position in screen pixels, read in the guest (where a click really went)."""
        x, y = self.powershell("Add-Type -AssemblyName System.Windows.Forms; $p=[System.Windows.Forms.Cursor]::Position; \"$($p.X),$($p.Y)\"").split(",")
        return int(x), int(y)

    def process_name(self, pid):
        """The guest process's name without .exe, or "" when it is gone."""
        return self.powershell(f"(Get-Process -Id {pid} -ErrorAction SilentlyContinue).ProcessName")

    def foreground(self):
        """The listed foreground window, or None when vm_windows lists none (no foreground, or a window it leaves out,
        such as a shell flyout)."""
        ws = self.call("vm_windows")
        return next((w for w in ws["windows"] if w["handle"] == ws["foreground"]), None)

    def screen_size(self):
        """The guest screen's resolution from an original-size screenshot; check it instead of trusting notes."""
        return png_size(self.call("vm_observe", max_size=-1)["_image"])

    # Generic fixtures. Each returns what the checks need; stop what you start (stop_process) before cleanup.

    def list_fixture(self, counts=(150, 1200), title="HH List Fixture"):
        """A WinForms window with one Win32 ListBox per count, named list<i> with items list<i>-<n>. Win32 lists expose
        every item to UI Automation (unlike File Explorer's virtualized item view), so they exercise control-tree
        limits. Returns the vm_launch result (pid, handle)."""
        lines = ["Add-Type -AssemblyName System.Windows.Forms",
                 f"$f = New-Object Windows.Forms.Form; $f.Text = '{title}'; $f.Width = {40 + 320 * len(counts)}; $f.Height = 500"]
        for i, n in enumerate(counts):
            lines += [f"$l = New-Object Windows.Forms.ListBox; $l.Name = 'list{i}'; $l.Left = {10 + 320 * i}; $l.Top = 10; $l.Width = 300; $l.Height = 400",
                      f"1..{n} | ForEach-Object {{ [void]$l.Items.Add(\"list{i}-$_\") }}", "$f.Controls.Add($l)"]
        lines.append("[void]$f.ShowDialog()")
        path = "C:\\Users\\Public\\hh-list-fixture.ps1"
        self.call("vm_exec", shell="powershell", command=f"Set-Content -Encoding UTF8 -Path {path} -Value @'\n" + "\n".join(lines) + "\n'@")
        return self.call("vm_launch", path="C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
                         args=["-STA", "-WindowStyle", "Hidden", "-ExecutionPolicy", "Bypass", "-File", path])

    def open_start_flyout(self):
        """Opens the Start flyout with a raw Windows key press (Hyper-V keyboard, no checks). Which process hosts it
        depends on the Windows build (research/windows.md); vm_windows does not list it."""
        self.call("vm_key", keys="win")
        time.sleep(1.5)

    def stop_process(self, pid):
        self.call("vm_exec", shell="powershell", command=f"Stop-Process -Id {pid} -ErrorAction SilentlyContinue")

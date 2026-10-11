# Follow-up acceptance on Win10 (2026-10-11)

Acceptance items from the TODO that run on `Win10` without the user, done after the 0.3.0 release. Builds are named per section. Evidence is under ignored `build/<item>-20261011/`.

## Cold boot without a signed-in user

Build `dev-20261011-101346-7c8bdf7-dirty` (0.3.0 plus the English guest tray). Evidence: `build/coldboot-20261011/` (`accept_coldboot.py`, `results.json`, screenshot).

A temp checkpoint was taken first. Then automatic sign-in was turned off in the guest (`AutoAdminLogon` = `0` under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon`, set through `vm_exec admin`), and the VM was shut down with `vm_shutdown` (7.7 s).

- `vm_start` started the VM and, after 99.6 s, refused with `agent_required`: `VM Win10 is running, but its desktop is not usable: the guest agent did not answer within 1m30s: no user is signed in, or the agent is not installed. ...`. The VM stayed running.
- Two `vm_status` calls afterwards each answered in 5 s: `power: running`, `agent.state: not_answering`.
- `vm_doctor` reported every host check `ok`, `vm.power` `ok` and `guest.agent` `fail` with its suggestion.
- `vm_observe` returned a host screenshot of the Windows sign-in curtain with `agent: offline`.
- `vm_unlock` and `vm_exec` refused at once with `agent_required`.
- Restoring the checkpoint (`vm_restore`, then `vm_start` in 3.2 s) brought back the signed-in desktop. `AutoAdminLogon` read `1` again, and `vm_end_turn` deleted the temp checkpoint.

Signing a user in is out of scope (see the TODO's product decisions); HyperHand reports the state and keeps working for status, doctor and screenshots.

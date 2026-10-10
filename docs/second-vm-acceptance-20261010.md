# Second VM acceptance (2026-10-10)

Covers the "second VM" part of TODO section 3, Display and VM configurations. Non-default DPI, multiple monitors and negative coordinate origins remain open.

## Environment

- One host serves two VMs:
  - `Win10`: guest name `EXAMPLE-GUEST`, used for HyperHand.
  - `Win10-Example`: guest name `EXAMPLE-VM`, a copy of Win10 renamed for Example testing.
- Both run at 1920x1080 with origin (0, 0), and both have AutoCAD 2015 running.
- Host build `68424b1` (`hyperhand.exe` SHA-256 `3D376996EFB5…`).
- Guest agents run protocol 3 (`E206EE445356…`). Before its update, `Win10-Example` reported protocol 2: `vm_status` still answered and showed `session_error: agent_outdated`, and `vm_update_agent` replaced the agent.
- Calls were made with `client/hyperhand_client.py`.
- Evidence is in Example's ignored `temp/hyperhand/secondvm-20261010/`: `accept-log.json`, `accept2-log.json`, `accept3-log.json` and screenshots.

## Results

- **Diagnostics and identity.** `vm_doctor` reports every check `ok` on both VMs. `vm_status` names the right guest for each VM (`EXAMPLE-GUEST`, `EXAMPLE-VM`), each with its own `owner` and job list.
- **Screenshot coordinates.**
  - A whole-screen `vm_observe` returned a 1920x1080 PNG with origin (0, 0) and scale 1.
  - Observing AutoCAD's main window returned origin (0, 0) and 1920x1040, matching the window's visible rect clipped to the screen.
  - Each VM's control tree and `vm_find_controls` returned its own controls.
- **Observations are bound to their VM** (one task using both VMs):
  - `vm_click` with a Win10 observation on `Win10-Example` was refused with `stale_observation` (`observation … is of VM "Win10", not "Win10-Example"`).
  - `vm_find_controls` with that observation was refused the same way.
  - `diff_from` naming it was ignored, with `stale_risk` `diff_from ignored: observation … is of VM Win10`.
- **Concurrent calls.** Four clients, two per VM, called `vm_status` and `vm_observe` at once. Each VM's results named its own guest. `vm_status` sometimes reported `agent.state: busy` while another call held that VM's agent connection, as documented; it never reported the other VM.
- **Input targets on both VMs.**
  - Notepad was started on each VM and observed at `max_size: 480`, giving scale 0.466 and origin (0, 244).
  - Each edit control's centre was converted to image pixels and clicked through `vm_click`.
  - A different text, with Chinese characters, was typed into each VM's Notepad. The read-back value of each edit control held exactly its own text and not the other VM's (`typed into Win10 管道`, `typed into Win10-Example 管道`).
  - Both Notepads were closed without saving.
- **Freshness check observed in practice.**
  - On `Win10-Example`, AutoCAD came in front of Notepad between the observation and the click; its expanded command history covered the region, with Example plugin pipe messages on the command line. The click was refused with `stale_observation` (`screen content near (516, 648) changed`), and the click after a new observation succeeded.
  - In an earlier run, a Win10 click right after launching Notepad was refused the same way while the window was still drawing.
- **Abandoned explicit owner released in practice.** A crashed acceptance script left its explicit task owning both VMs. The next task's write got `vm_busy` with `owner_idle_ms` around 30000 and `owner_in_flight: 0`. `vm_end_turn {task_id: <owner>}` released both VMs, as the error's `next` says.

# HyperHand: Hyper-V VM Control for AI Agents

HyperHand lets an AI agent such as Claude Code or Codex see and operate a Windows virtual machine on Hyper-V, the way a person at the keyboard would: look at the screen, click, type, run commands, open programs, copy files in and out, and roll back to a checkpoint when something goes wrong. The VM needs no network connection, and the AI never needs the guest password. It works with any MCP client that supports Streamable HTTP.

> [!WARNING]
> HyperHand gives an AI full control of your virtual machines. Use VMs you can afford to lose, and keep checkpoints. Any program on your computer can reach HyperHand's local MCP endpoint, which has no password.

## What you can use it for

- **Test installers and builds in a clean Windows**: let the AI copy a build into the VM, install it, click through the setup, check the result and roll the VM back for the next run.
- **Automate desktop programs that have no API**: anything you can operate with a mouse and keyboard, the AI can operate too, reading the program's buttons and fields where the program exposes them and the pixels where it does not.
- **Reproduce problems reliably**: save a checkpoint before a risky step, try, and restore the exact same state as often as needed.
- **Keep risky work off your own computer**: commands, downloads and experiments run inside the VM, not on the host.

## Features

- **Sees what you would see**: the AI takes screenshots of the whole screen or of one window, and reads the names and values of buttons, text boxes and menus where the program exposes them.
- **Careful input**: before clicking or typing, HyperHand brings the right window to the front and checks that the target is visible, enabled and not covered by another window. If the screen changed since the AI last looked, it refuses and asks the AI to look again instead of clicking blindly.
- **Works where other tools stop**: the screen, mouse and keyboard go through Hyper-V itself, so they also work on the sign-in screen and on UAC prompts.
- **Commands and programs**: run PowerShell or cmd in the guest and get the output back; start programs; run long jobs in the background and check on them later.
- **File transfer**: copy files or whole folders in either direction. Unchanged files are skipped, and a folder can be made an exact copy of a build folder on the host after a preview of what will change.
- **Checkpoints**: create, restore, keep and delete checkpoints. Temporary ones are cleaned up when the AI ends its work, automatically with the optional hook in [docs/hooks](docs/hooks/).
- **Starts the VM to a usable desktop**: starting a VM waits until the desktop is ready and can unlock it with a password you store on the host, in Windows Credential Manager.
- **No guest network, no guest password for the AI**: the host talks to the guest over a Hyper-V socket, and the guest agent is copied in without the network.
- **No UAC prompts in everyday use**: administrator approval is needed only to install, update or uninstall HyperHand.

## How it works

HyperHand has two executables and three roles:

- **`hyperhand.exe`**, an ordinary tray program on the host. It serves MCP at `http://127.0.0.1:8770/mcp`.
- **`HyperHandService`**, the same executable running as a Windows service under its own dedicated account. It performs the Hyper-V operations, so you do not need to be a Hyper-V administrator.
- **`hyperhand-agent.exe`**, a small tray program inside the guest. It runs commands, starts programs, transfers files and reads windows and controls in the logged-on user's session.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/architecture-dark.svg">
  <img alt="HyperHand architecture: an MCP client talks to the ordinary host tray, which delegates Hyper-V control and the guest socket connection to HyperHandService" src="docs/images/architecture.svg">
</picture>

## Requirements

- A Windows 10 or 11 Pro or Enterprise host with Hyper-V, and administrator approval for installing, updating and uninstalling.
- A Windows guest with a user logged on. Sign the user in automatically, or store an unlock password in the HyperHand tray.
- Virtual Machine Connection in basic session mode, not enhanced session.
- An MCP client that supports Streamable HTTP, such as Claude Code, Codex or Cursor.

## Quick start

1. Download `hyperhand-X.Y.Z-windows-amd64.zip` from [Releases](https://github.com/n2ns/hyper-hand/releases), extract both executables to the same folder and run:

   ```powershell
   hyperhand.exe install
   ```

   Approve the UAC prompt once. HyperHand installs to `%ProgramFiles%\HyperHand`, and its tray icon appears.

2. Add HyperHand to your MCP client and start a new session:

   ```powershell
   claude mcp add --transport http hyperhand http://127.0.0.1:8770/mcp   # Claude Code
   codex mcp add hyperhand --url http://127.0.0.1:8770/mcp               # Codex
   ```

   Other clients: add a Streamable HTTP server with the URL `http://127.0.0.1:8770/mcp`.

3. Start the VM, log on and switch the guest keyboard to English. Then ask the AI to install the guest agent ("install the HyperHand agent in Win10"). The agent starts at every logon from then on.

4. Ask the AI to work in the VM, for example:

   - "In the Win10 VM, open Notepad, type a short note and save it to the desktop."
   - "Make a checkpoint of Win10, copy `D:\build\setup.exe` into it, install it and tell me whether the program starts. Then restore the checkpoint."
   - "Check the Windows version and free disk space of the Win10 VM."

The [user guide](docs/user-guide.md) covers each step in detail, optional guest setup and troubleshooting.

## Known limitations

- The guest keyboard must be in English mode while the agent is installed, because the install command is typed on the keyboard. Installing the agent by hand avoids this.
- HyperHand cannot sign a user in at the sign-in screen after a cold boot; use automatic sign-in. It can unlock a session that Windows locked later. The unlock password must be ASCII; store the PIN if the lock screen asks for one.
- Screenshots and input act on the VM console, so they do not reach a remote desktop or enhanced session.
- Commands that run as administrator need the guest's UAC set to elevate without prompting; otherwise the UAC prompt waits in the guest.
- Programs that draw their own controls show no buttons or fields to read; the AI then works from the screenshot alone.
- Deleting a checkpoint merges its disk changes, which can take minutes.

## Privacy and security

HyperHand sends no telemetry and connects to nothing on the internet. The host and the guest talk over Hyper-V sockets. The MCP endpoint listens only on `127.0.0.1` and has no authentication, so any program on your computer can use it to control your VMs. Unlock passwords are stored in Windows Credential Manager and are never returned to the AI. Details in [Privacy](docs/privacy.md).

## Uninstall

Uninstall the guest agent first, then the host: run `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe uninstall` inside each guest, then `hyperhand.exe uninstall` on the host, and remove the server from your MCP client. See [Uninstalling](docs/user-guide.md#uninstalling).

## Documentation

- [User guide](docs/user-guide.md): installation step by step, the tool list, updating, uninstalling and troubleshooting.
- [Features](docs/features.md): the exact behavior of every tool.
- [Building HyperHand](docs/building.md): building from source, testing, development installs and releasing.
- [Privacy](docs/privacy.md): what is stored and what goes over the wire.
- [Python client](client/README.md): calling HyperHand from scripts without an MCP client.
- [v0.2.0 acceptance](docs/acceptance-v0.2.0.md): tested environments and results.
- [Changelog](CHANGELOG.md)

## License

HyperHand is released under the [MIT License](LICENSE).

## Disclaimer

HyperHand is an independent project, not affiliated with or endorsed by Microsoft or Anthropic. Hyper-V and Windows are trademarks of Microsoft.

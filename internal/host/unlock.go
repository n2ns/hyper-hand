package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"hyperhand/internal/credential"
	"hyperhand/internal/proto"
)

// errNoPassword: the session is locked and no unlock password is stored for the VM.
var errNoPassword = errors.New("no unlock password is stored for this VM: set one in the HyperHand tray menu (the VM's submenu, Set unlock password...), or unlock it in VMConnect")

// agentStartTimeout is how long vm_start waits for the guest agent after the VM runs.
const agentStartTimeout = 90 * time.Second

// unlocker detects a locked guest session and unlocks it by typing the stored password on the Hyper-V keyboard.
type unlocker struct {
	input    sync.Locker // held by every HyperHand tool that sends keyboard or mouse input
	call     agentCall
	keys     func(vm, keys string) error
	typeText func(vm, text string) error
	password func(vm string) (string, bool, error)
	sleep    func(time.Duration)
}

func newUnlocker(call agentCall, b Backend, input sync.Locker) unlocker {
	return unlocker{
		input:    input,
		call:     call,
		keys:     b.PressKeys,
		typeText: b.TypeText,
		password: func(vm string) (string, bool, error) {
			_, p, ok, err := credential.Read(vm)
			return p, ok, err
		},
		sleep: time.Sleep,
	}
}

// state asks the agent for its session's lock state.
func (u unlocker) state(ctx context.Context, vm string) (proto.SessionStateResult, error) {
	var s proto.SessionStateResult
	if _, err := u.call(ctx, vm, proto.OpSessionState, nil, nil, &s); err != nil {
		if strings.HasPrefix(err.Error(), "unknown op") {
			return s, errors.New("the guest agent is too old to report whether the session is locked; run vm_update_agent")
		}
		return s, err
	}
	return s, nil
}

// waitAgent pings the agent until it answers or timeout passes.
func (u unlocker) waitAgent(ctx context.Context, vm string, timeout time.Duration) (proto.PingResult, error) {
	var err error
	for end := time.Now().Add(timeout); ; u.sleep(2 * time.Second) {
		var p proto.PingResult
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = u.call(pctx, vm, proto.OpPing, nil, nil, &p)
		cancel()
		if err == nil {
			return p, nil
		}
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		if !time.Now().Before(end) {
			return p, err
		}
	}
}

// Session states unlock returns.
const (
	sessionUnlocked  = "unlocked"   // the session was locked and the stored password unlocked it
	sessionNotLocked = "not_locked" // the session was not locked; nothing was typed
)

// unlock unlocks vm's session with the stored password and returns sessionUnlocked or sessionNotLocked. It types the
// password only while the agent reports the session locked and no UAC prompt open, and types it once: a wrong password
// is reported, not retried, so that the account is not locked out.
func (u unlocker) unlock(ctx context.Context, vm string) (string, error) {
	s, err := u.state(ctx, vm)
	if err != nil {
		return "", err
	}
	if !s.Locked {
		return sessionNotLocked, nil
	}
	pw, ok, err := u.password(vm)
	if err != nil {
		return "", fmt.Errorf("read the stored unlock password: %w", err)
	}
	if !ok {
		return "", errNoPassword
	}
	for _, r := range pw {
		if r > 127 {
			return "", errors.New("the stored unlock password contains non-ASCII characters, which the Hyper-V keyboard cannot type")
		}
	}
	if !s.Console {
		return "", errNotConsole
	}
	// Ctrl dismisses the lock screen curtain without typing a character.
	if err := u.keys(vm, "ctrl"); err != nil {
		return "", err
	}
	u.sleep(1500 * time.Millisecond)
	if s, err = u.state(ctx, vm); err != nil {
		return "", err
	}
	if !s.Locked {
		return sessionNotLocked, nil
	}
	if err := u.typePassword(ctx, vm, pw); err != nil {
		return "", err
	}
	for i := 0; i < 15; i++ {
		u.sleep(time.Second)
		if s, err = u.state(ctx, vm); err == nil && !s.Locked {
			return sessionUnlocked, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", errors.New("the session is still locked 15 s after the stored password was typed; it was not retried, so that the account is not locked out. Check the stored password (or PIN) and the screen with vm_observe")
}

// Session states takeConsole returns.
const (
	sessionMoved          = "moved"           // tscon moved the agent's session to the console
	sessionAlreadyConsole = "already_console" // the agent's session was the console session; nothing ran
)

// tsconCommand moves the session of the elevated PowerShell that runs it, which is the agent's session, to the console.
const tsconCommand = `tscon.exe ((Get-Process -Id $PID).SessionId) /dest:console; exit $LASTEXITCODE`

// takeConsole moves vm's agent session from an enhanced or remote desktop connection (or a disconnected state) back to
// the VM console with an elevated tscon, then waits up to 10 s until the agent reports the console session. It returns
// sessionAlreadyConsole without running anything when the session is the console session.
func (u unlocker) takeConsole(ctx context.Context, vm string) (string, proto.SessionStateResult, error) {
	s, err := u.state(ctx, vm)
	if err != nil {
		return "", s, err
	}
	if s.Console {
		return sessionAlreadyConsole, s, nil
	}
	var r proto.ExecResult
	if _, err := u.call(ctx, vm, proto.OpExec, proto.ExecArgs{Command: tsconCommand, TimeoutMs: 30000, Admin: true}, nil, &r); err != nil {
		return "", s, err
	}
	if r.ElevationPending {
		return "", s, refuse(codeElevationTimeout, "the UAC prompt is in the remote session, where Hyper-V input cannot reach it: a person must approve it in the enhanced or remote desktop viewer, or sign the user out there and sign in on the VM console; then call vm_status", nil, "tscon did not run: elevation did not complete within 30 s")
	}
	if r.TimedOut {
		return "", s, refuse(codeFailed, "call vm_status; if the session is still not the console, call vm_doctor", map[string]any{"timed_out": true, "stdout": r.Stdout, "stderr": r.Stderr}, "tscon did not finish within 30 s and was terminated")
	}
	if r.ExitCode != 0 {
		return "", s, refuse(codeFailed, "call vm_doctor and vm_status; the session may belong to another user or have been signed out", map[string]any{"exit_code": r.ExitCode, "stdout": r.Stdout, "stderr": r.Stderr}, "tscon exited with %d", r.ExitCode)
	}
	// Right after the move the agent can report the session locked for a moment, so a locked console session is
	// polled further and reported locked only if the last reading still says so.
	for i := 0; i < 10; i++ {
		if s, err = u.state(ctx, vm); err == nil && s.Console && !s.Locked {
			return sessionMoved, s, nil
		}
		if ctx.Err() != nil {
			return "", s, ctx.Err()
		}
		u.sleep(time.Second)
	}
	if err != nil {
		return "", s, err
	}
	if s.Console {
		return sessionMoved, s, nil
	}
	return "", s, errors.New("tscon exited 0 but the agent still reports a session other than the console 10 s later; call vm_doctor and vm_observe")
}

// typePassword types pw and Enter into the password box. It holds the input lock, so that no other HyperHand tool sends
// input meanwhile, and checks the password box right before typing: Ctrl+A (which selects anything left in the box,
// so the password replaces it) goes first, as it types no character wherever it lands.
func (u unlocker) typePassword(ctx context.Context, vm, pw string) error {
	u.input.Lock()
	defer u.input.Unlock()
	if err := u.keys(vm, "ctrl+a"); err != nil {
		return err
	}
	// Bounded: the agent may be busy with another request, and the input lock blocks every input tool meanwhile.
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	s, err := u.state(sctx, vm)
	cancel()
	if err != nil {
		return fmt.Errorf("the password was not typed: %w", err)
	}
	if err := passwordBoxReady(s); err != nil {
		return fmt.Errorf("the password was not typed: %w", err)
	}
	if err := u.typeText(vm, pw); err != nil {
		return errors.New("typing the password failed") // the error could quote the password
	}
	return u.keys(vm, "enter")
}

var errNotConsole = errors.New("the agent's session is not the VM console session (an enhanced session, remote desktop or another user's session), so the Hyper-V keyboard would not type into it; the password was not typed. Call vm_take_console first")

// passwordBoxReady checks, right before the password is typed, that keystrokes go to the sign-in screen of the agent's
// own session: Windows reports it locked, it is the console session, LogonUI runs in it, input goes to the secure
// desktop (not to the user's programs) and no UAC prompt is open there.
func passwordBoxReady(s proto.SessionStateResult) error {
	switch {
	case !s.Locked:
		return errors.New("the session is not locked")
	case !s.Console:
		return errNotConsole
	case s.Consent:
		return errors.New("a UAC prompt is open in the guest")
	case !s.LogonUI:
		return errors.New("the sign-in screen (LogonUI) is not running in the session")
	case !s.SecureDesktop:
		return errors.New("keyboard input still goes to the user's desktop, not to the sign-in screen's password box")
	}
	return nil
}

// relockDelay: Windows can lock a session right after an automatic sign-in, after the agent already answered.
const relockDelay = 3 * time.Second

// readyResult describes a usable desktop: the agent that answered and whether ready had to unlock the session.
type readyResult struct {
	Agent    proto.PingResult
	Unlocked bool // the session was locked and has been unlocked
}

// checkProtocol refuses an agent whose protocol is older than proto.Protocol.
func checkProtocol(p proto.PingResult) error {
	if p.Protocol < proto.Protocol {
		return refuse(codeAgentOutdated, "call vm_update_agent", map[string]any{"agent_protocol": p.Protocol, "host_protocol": proto.Protocol},
			"the guest agent %s speaks protocol %d; this host needs %d", p.Version, p.Protocol, proto.Protocol)
	}
	return nil
}

// ready waits for the agent of a running VM and unlocks its session if needed. The result describes a usable desktop;
// an error (agent_required, agent_outdated, session_unusable) says why the desktop is not usable.
func (u unlocker) ready(ctx context.Context, vm string, agentTimeout time.Duration) (readyResult, error) {
	var r readyResult
	p, err := u.waitAgent(ctx, vm, agentTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		return r, refuse(codeAgentRequired, "call vm_install_agent if the agent is not installed, else call vm_status later", nil,
			"the guest agent did not answer within %v: no user is signed in, or the agent is not installed. HyperHand can unlock a locked session only while the agent runs in it: %v", agentTimeout, err)
	}
	if err := checkProtocol(p); err != nil {
		return r, err
	}
	r.Agent = p
	s, err := u.state(ctx, vm)
	if err != nil {
		return r, err
	}
	if !s.Locked {
		u.sleep(relockDelay)
		if s, err = u.state(ctx, vm); err != nil {
			return r, err
		}
	}
	if !s.Locked {
		return r, nil
	}
	if _, err := u.unlock(ctx, vm); err != nil {
		return r, refuse(codeSessionUnusable, "fix the cause, then call vm_start again", map[string]any{"locked": true},
			"the session is locked: %v", err)
	}
	r.Unlocked = true
	return r, nil
}

// lockedInput serializes the keyboard and mouse input of HyperHand tools with the unlock sequence.
type lockedInput struct {
	Backend
	mu sync.Locker
}

func (b lockedInput) Click(vm string, x, y, button, count int, modifiers []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Backend.Click(vm, x, y, button, count, modifiers)
}

func (b lockedInput) Drag(vm string, x1, y1, x2, y2 int, modifiers []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Backend.Drag(vm, x1, y1, x2, y2, modifiers)
}

func (b lockedInput) Scroll(vm string, x, y, delta int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Backend.Scroll(vm, x, y, delta)
}

func (b lockedInput) PressKeys(vm, keys string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Backend.PressKeys(vm, keys)
}

func (b lockedInput) TypeText(vm, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Backend.TypeText(vm, text)
}

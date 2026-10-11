package host

import (
	"context"
	"fmt"
	"time"

	"hyperhand/internal/hyperv"
)

// shutdownTimeout is how long vm_shutdown waits for the VM to be off.
const shutdownTimeout = 3 * time.Minute

// waitOff polls the VM's state every 2 seconds until it is Off. It never turns the VM off itself.
func waitOff(ctx context.Context, find func() (hyperv.VM, error), sleep func(time.Duration), timeout time.Duration) error {
	end := time.Now().Add(timeout)
	for {
		v, err := find()
		if err != nil {
			return err
		}
		if v.State == "Off" {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("stopped waiting for VM %s to shut down; the shutdown may still be in progress: %w", v.Name, ctx.Err())
		}
		if !time.Now().Before(end) {
			// Observed on Win10 with an unsaved document: Windows had begun signing the user out, the agent had exited
			// and the sign-in screen took no input; only turning the VM off recovered it.
			return refuse(codeFailed, "call vm_observe to see the guest screen; if a program or Windows asks what to do, answer with raw vm_key or vm_click; if the guest is stuck (agent not answering, sign-in screen ignoring input), only vm_turn_off or vm_restore recovers it, losing unsaved work",
				map[string]any{"vm": v.Name, "state": powerState(v.State)},
				"VM %s is still %s %v after the shutdown request and was not turned off: a program in the guest may be blocking shutdown, or the guest ignored the request. Windows may already be signing the user out, so the agent may have exited and the desktop may not accept input", v.Name, powerState(v.State), timeout)
		}
		sleep(2 * time.Second)
	}
}

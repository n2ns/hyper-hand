package host

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"hyperhand/internal/hyperv"
)

func testTask(t *testing.T, d *deps, id string) *taskState {
	t.Helper()
	task, err := d.resolveTask(&mcp.CallToolRequest{}, id, false)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestExternalMutationWaitsForInputDispatch(t *testing.T) {
	d := &deps{obs: newObservationStore(), input: &sync.Mutex{}}
	d.input.Lock() // an action has validated its observation and is dispatching
	starting, dispatched, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(starting)
		end := d.beginExternalMutation(context.Background(), "CAD", true)
		close(dispatched)
		<-release
		end()
		close(done)
	}()
	<-starting
	select {
	case <-dispatched:
		d.input.Unlock()
		close(release)
		<-done
		t.Fatal("external operation passed an in-flight input dispatch")
	case <-time.After(20 * time.Millisecond):
	}
	before := d.obs.version("CAD")
	if before.Revision != 0 || before.Epoch != 0 || before.Busy != 0 {
		d.input.Unlock()
		close(release)
		<-done
		t.Fatalf("external mutation advanced during input dispatch: %+v", before)
	}
	d.input.Unlock()
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("external operation did not start after input dispatch")
	}
	during := d.obs.version("CAD")
	if during.Busy != 1 || during.Epoch == 0 {
		t.Errorf("external mutation not marked: %+v", during)
	}
	close(release)
	<-done
	after := d.obs.version("CAD")
	if after.Busy != 0 || after.Epoch <= during.Epoch {
		t.Errorf("external operation did not retire its observations: %+v", after)
	}
}

func TestTaskEndWaitsForInflightAndPreservesOtherVM(t *testing.T) {
	d := &deps{obs: newObservationStore(), tasks: newTaskRegistry()}
	a := testTask(t, d, "a")
	b := testTask(t, d, "b")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.tasks.claim(a, "CAD"); err != nil {
		t.Fatal(err)
	}
	_, completeWrite, _ := a.enter(ctx, "CAD", false)
	waitA, completeWaitA, _ := a.enter(ctx, "CAD", true)
	waitOtherVM, completeWaitOther, _ := a.enter(ctx, "Other", true)
	defer completeWaitOther()
	waitB, completeWaitB, _ := b.enter(ctx, "CAD", true)
	defer completeWaitB()
	ended := make(chan error, 1)
	go func() {
		n, err := a.beginEnd(ctx, "cad")
		if err == nil && n != 1 {
			t.Errorf("cancelled %d waits", n)
		}
		ended <- err
	}()
	select {
	case <-waitA.Done():
	case <-ctx.Done():
		t.Fatal("own wait not cancelled")
	}
	completeWaitA()
	if waitB.Err() != nil || waitOtherVM.Err() != nil {
		t.Fatal("unrelated wait cancelled")
	}
	if err := d.tasks.claim(b, "CAD"); err == nil {
		t.Fatal("released lease before inflight write completed")
	}
	if _, _, err := a.enter(ctx, "CAD", false); err == nil {
		t.Fatal("accepted write during cleanup")
	}
	_, doneOther, err := a.enter(ctx, "Other", false)
	if err != nil {
		t.Fatal("scoped cleanup blocked another VM:", err)
	}
	doneOther()
	select {
	case <-ended:
		t.Fatal("cleanup passed inflight write")
	default:
	}
	completeWrite()
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	d.tasks.release(a, "CAD")
	a.finishEnd("CAD", true)
	if err := d.tasks.claim(b, "CAD"); err != nil {
		t.Fatal(err)
	}
	if a.ended {
		t.Fatal("VM-scoped cleanup ended entire task")
	}
}

func TestTaskEndCancellationKeepsOwnership(t *testing.T) {
	d := &deps{obs: newObservationStore(), tasks: newTaskRegistry()}
	a, b := testTask(t, d, "a"), testTask(t, d, "b")
	d.tasks.claim(a, "CAD")
	_, done, _ := a.enter(context.Background(), "CAD", false)
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.beginEnd(ctx, ""); err == nil {
		t.Fatal("ended during inflight write")
	}
	if err := d.tasks.claim(b, "CAD"); err == nil {
		t.Fatal("cancelled cleanup released ownership")
	}
	_, done2, err := a.enter(context.Background(), "CAD", false)
	if err != nil {
		t.Fatal("owner cannot retry after cancellation:", err)
	}
	done2()
}

func TestTaskCheckpointCleanupIsolationAndFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "CAD", ID: "cad", State: "Running"}, {Name: "Other", ID: "other", State: "Running"}}}
	cs, d := connectTools(t, ctx, b)
	d.tasks = newTaskRegistry()
	lookup := callRefused(t, ctx, cs, "vm_launch", map[string]any{"task_id": "lookup", "vm": "missing", "path": "notepad.exe"})
	if lookup["error"] != codeInvalidArgument || lookup["next"] != "call vm_list and pass one of its names as vm" || len(d.tasks.owners) != 0 {
		t.Fatalf("VM lookup contract changed or lookup acquired ownership: %v", lookup)
	}
	var ca, cb createdCheckpoint
	callJSON(t, ctx, cs, "vm_checkpoint", map[string]any{"task_id": "a", "vm": "CAD", "label": "a"}, &ca)
	callJSON(t, ctx, cs, "vm_checkpoint", map[string]any{"task_id": "b", "vm": "Other", "label": "b"}, &cb)
	var out endTurnOut
	refused := callRefused(t, ctx, cs, "vm_end_turn", map[string]any{"task_id": "a", "vm": "Other", "all_temp": true})
	if refused["error"] != "vm_busy" || len(b.deleted) != 0 {
		t.Fatalf("all_temp touched another task: %v %v", refused, b.deleted)
	}
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"task_id": "a"}, &out)
	if len(out.DeletedCheckpoints) != 1 || out.DeletedCheckpoints[0] != ca.Name {
		t.Fatalf("wrong cleanup: %+v", out)
	}
	if _, _, err := b.node(cb.ID); err != nil {
		t.Fatal("other task checkpoint deleted")
	}
	if d.tasks.tasks["b"].ended {
		t.Fatal("other task ended")
	}
	b.listCheckpointsErr = errors.New("temporary listing failure")
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"task_id": "b", "vm": "Other"}, &out)
	if len(out.Errors) != 1 {
		t.Fatalf("missing cleanup failure: %+v", out)
	}
	refused = callRefused(t, ctx, cs, "vm_checkpoint", map[string]any{"task_id": "c", "vm": "Other", "label": "c"})
	if refused["error"] != "vm_busy" {
		t.Fatalf("cleanup failure released ownership: %v", refused)
	}
	b.listCheckpointsErr = nil
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"task_id": "b", "vm": "Other"}, &out)
	if len(out.DeletedCheckpoints) != 1 || out.DeletedCheckpoints[0] != cb.Name {
		t.Fatalf("scoped cleanup: %+v", out)
	}
	if d.tasks.tasks["b"].ended {
		t.Fatal("scoped cleanup ended task")
	}
}

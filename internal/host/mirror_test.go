package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
	"hyperhand/internal/mirror"
	"hyperhand/internal/proto"
)

// mirrorBackend uses real disk inventories and the mirror engine over the wire,
// without requiring Hyper-V. Faults are injected after receiving complete frames.
type mirrorBackend struct {
	Backend
	oldAgent  bool
	loseReply bool
	applies   atomic.Int32
}

func (b *mirrorBackend) Find(name string) (hyperv.VM, error) {
	if name == "" {
		name = "CAD"
	}
	id := "A"
	if name == "other-vm" {
		id = "B"
	}
	return hyperv.VM{ID: id, Name: name, State: "Running"}, nil
}

func (b *mirrorBackend) Dial(_ context.Context, _ string) (net.Conn, error) {
	client, guest := net.Pipe()
	go func() {
		defer guest.Close()
		for {
			var req proto.Request
			payload, err := proto.ReadFrame(guest, &req)
			if err != nil {
				return
			}
			var result any
			switch req.Op {
			case proto.OpPing:
				result = proto.PingResult{Version: "test", Protocol: proto.Protocol}
			case proto.OpMirrorScan:
				if b.oldAgent {
					err = fmt.Errorf("unknown op %q", req.Op)
					break
				}
				var a proto.PathArgs
				if err = json.Unmarshal(req.Args, &a); err == nil {
					result, err = mirror.Scan(context.Background(), a.Path, true)
				}
			case proto.OpMirrorApply:
				b.applies.Add(1)
				var a proto.MirrorApplyArgs
				if err = json.Unmarshal(req.Args, &a); err == nil {
					result = mirror.Apply(context.Background(), a.Path, a.Source, a.Target, a.Force, bytes.NewReader(payload))
				}
				if b.loseReply {
					return
				}
			case proto.OpHashFiles:
				var a proto.PathsArgs
				if err = json.Unmarshal(req.Args, &a); err == nil {
					r := proto.HashesResult{}
					for _, p := range a.Paths {
						h, _ := hashFile(p)
						r.Hashes = append(r.Hashes, h)
					}
					result = r
				}
			case proto.OpWriteFile:
				var a proto.PathArgs
				if err = json.Unmarshal(req.Args, &a); err == nil {
					err = os.MkdirAll(filepath.Dir(a.Path), 0755)
				}
				if err == nil {
					err = os.WriteFile(a.Path, payload, 0644)
				}
			default:
				err = fmt.Errorf("unexpected op %q", req.Op)
			}
			response := proto.Response{}
			if err != nil {
				response.Error = err.Error()
			} else {
				response.Result, _ = json.Marshal(result)
			}
			if proto.WriteFrame(guest, response, nil) != nil {
				return
			}
		}
	}()
	return client, nil
}

func mirrorSession(t *testing.T, b *mirrorBackend) (context.Context, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	m := &Manager{Backend: b}
	t.Cleanup(func() { m.Drop("A"); m.Drop("B") })
	st, ct := mcp.NewInMemoryTransports()
	ss, err := NewServer(m).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mirror-test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return ctx, cs
}

func mirrorFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func mirrorSnapshot(t *testing.T, root string) mirror.Manifest {
	t.Helper()
	m, err := mirror.Scan(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mirrorArgs(source, target, task string) map[string]any {
	return map[string]any{"vm": "CAD", "host_path": source, "guest_path": target, "task_id": task, "mode": "mirror"}
}

func planMirror(t *testing.T, ctx context.Context, cs *mcp.ClientSession, args map[string]any) map[string]any {
	t.Helper()
	var plan map[string]any
	callJSON(t, ctx, cs, "vm_push", args, &plan)
	if plan["status"] != "planned" || plan["plan_id"] == "" || plan["plan_id"] == nil {
		t.Fatalf("invalid plan: %v", plan)
	}
	if next, _ := plan["next"].(string); !strings.Contains(next, "nothing was written") || !strings.Contains(next, "phase apply") ||
		!strings.Contains(next, plan["plan_id"].(string)) || !strings.Contains(next, plan["expires_at"].(string)) || !strings.Contains(next, fmt.Sprintf("force (%t)", plan["force"])) {
		t.Fatalf("plan next does not describe the apply call: %v", plan["next"])
	}
	args["phase"], args["plan_id"] = "apply", plan["plan_id"]
	return plan
}

func TestMirrorMCPPlanApplyAndOrdinaryCopy(t *testing.T) {
	b := &mirrorBackend{}
	ctx, cs := mirrorSession(t, b)
	source, target := t.TempDir(), t.TempDir()
	mirrorFile(t, source, "new.dll", "new")
	mirrorFile(t, source, "changed.dll", "new version")
	mirrorFile(t, source, "same.dll", "same")
	mirrorFile(t, target, "changed.dll", "old version")
	mirrorFile(t, target, "same.dll", "same")
	mirrorFile(t, target, "obsolete/old.dll", "obsolete")
	if err := os.MkdirAll(filepath.Join(source, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	before := mirrorSnapshot(t, target)
	args := mirrorArgs(source, target, "deployer")
	plan := planMirror(t, ctx, cs, args)
	if !mirror.Equal(before, mirrorSnapshot(t, target)) || b.applies.Load() != 0 {
		t.Fatal("planning modified destination")
	}
	var changes []mirror.Change
	encoded, _ := json.Marshal(plan["changes"])
	if err := json.Unmarshal(encoded, &changes); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range changes {
		seen[c.Action+":"+c.Path] = true
	}
	for _, want := range []string{"copy:new.dll", "overwrite:changed.dll", "skip:same.dll", "mkdir:empty", "delete_file:obsolete/old.dll", "delete_dir:obsolete"} {
		if !seen[want] {
			t.Errorf("plan lacks %s: %v", want, changes)
		}
	}
	var out map[string]any
	callJSON(t, ctx, cs, "vm_push", args, &out)
	if out["status"] != "complete" || !mirror.Equal(mirrorSnapshot(t, source), mirrorSnapshot(t, target)) {
		t.Fatalf("mirror did not converge: %v", out)
	}
	refused := callRefused(t, ctx, cs, "vm_push", args)
	if refused["error"] != "plan_stale" || b.applies.Load() != 1 {
		t.Fatalf("consumed plan replayed: %v", refused)
	}
	mirrorFile(t, target, "keep-extra.dll", "keep")
	callJSON(t, ctx, cs, "vm_push", map[string]any{"vm": "CAD", "task_id": "deployer", "host_path": source, "guest_path": target}, &out)
	if _, err := os.Stat(filepath.Join(target, "keep-extra.dll")); err != nil {
		t.Fatalf("ordinary copy deleted extra file: %v", err)
	}
}

func TestMirrorMCPPlanDoesNotClaimWriter(t *testing.T) {
	ctx, cs := mirrorSession(t, &mirrorBackend{})
	source, target := t.TempDir(), t.TempDir()
	mirrorFile(t, source, "new.dll", "new")
	args := mirrorArgs(source, target, "planner")
	planMirror(t, ctx, cs, args)
	var out map[string]any
	callJSON(t, ctx, cs, "vm_push", map[string]any{"task_id": "writer", "vm": "CAD", "host_path": source, "guest_path": t.TempDir()}, &out)
	// A second read-only plan is allowed while another task owns the VM.
	planMirror(t, ctx, cs, mirrorArgs(source, target, "reader"))
	refused := callRefused(t, ctx, cs, "vm_push", args)
	if refused["error"] != "vm_busy" {
		t.Fatalf("apply bypassed existing writer: %v", refused)
	}
	if len(mirrorSnapshot(t, target).Entries) != 0 {
		t.Fatal("plan or refused apply wrote destination")
	}
}

func TestMirrorMCPRejectsStaleAndReboundPlans(t *testing.T) {
	for _, scenario := range []string{"source drift", "target drift", "force mismatch", "source mismatch", "target mismatch", "different task", "different VM"} {
		t.Run(scenario, func(t *testing.T) {
			b := &mirrorBackend{}
			ctx, cs := mirrorSession(t, b)
			source, target := t.TempDir(), t.TempDir()
			mirrorFile(t, source, "new.dll", "new")
			mirrorFile(t, target, "old.dll", "must survive")
			args := mirrorArgs(source, target, "owner")
			planMirror(t, ctx, cs, args)
			switch scenario {
			case "source drift":
				mirrorFile(t, source, "new.dll", "changed")
			case "target drift":
				mirrorFile(t, target, "appeared.dll", "external")
			case "force mismatch":
				args["force"] = true
			case "source mismatch":
				args["host_path"] = t.TempDir()
			case "target mismatch":
				args["guest_path"] = t.TempDir()
			case "different task":
				args["task_id"] = "other"
			case "different VM":
				args["vm"] = "other-vm"
			}
			before := mirrorSnapshot(t, target)
			out := callRefused(t, ctx, cs, "vm_push", args)
			if out["error"] != "plan_stale" || out["status"] != "plan_stale" || b.applies.Load() != 0 || !mirror.Equal(before, mirrorSnapshot(t, target)) {
				t.Fatalf("unsafe stale apply: %v", out)
			}
		})
	}
}

func TestMirrorMCPInvalidArguments(t *testing.T) {
	for _, extra := range []map[string]any{{"mode": "unknown"}, {"mode": "copy", "phase": "plan"}, {"mode": "copy", "plan_id": "x"}, {"phase": "unknown"}, {"phase": "apply"}, {"phase": "plan", "plan_id": "x"}} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			ctx, cs := mirrorSession(t, &mirrorBackend{})
			args := mirrorArgs(t.TempDir(), t.TempDir(), "invalid")
			for k, v := range extra {
				args[k] = v
			}
			out := callRefused(t, ctx, cs, "vm_push", args)
			if out["error"] != "invalid_argument" {
				t.Fatalf("unexpected refusal: %v", out)
			}
		})
	}
}

func TestMirrorMCPOldAgentAndLostResponse(t *testing.T) {
	t.Run("old agent", func(t *testing.T) {
		ctx, cs := mirrorSession(t, &mirrorBackend{oldAgent: true})
		out := callRefused(t, ctx, cs, "vm_push", mirrorArgs(t.TempDir(), t.TempDir(), "deploy"))
		if out["error"] != "agent_outdated" || !strings.Contains(fmt.Sprint(out["next"]), "vm_update_agent") {
			t.Fatalf("missing upgrade instruction: %v", out)
		}
	})
	t.Run("lost response is not replayed", func(t *testing.T) {
		b := &mirrorBackend{loseReply: true}
		ctx, cs := mirrorSession(t, b)
		source, target := t.TempDir(), t.TempDir()
		mirrorFile(t, source, "new.dll", "new")
		mirrorFile(t, target, "old.dll", "obsolete")
		args := mirrorArgs(source, target, "deploy")
		planMirror(t, ctx, cs, args)
		out := callRefused(t, ctx, cs, "vm_push", args)
		if out["error"] != "mirror_unknown" || out["status"] != "unknown" || b.applies.Load() != 1 {
			t.Fatalf("lost response not reported honestly: %v", out)
		}
		if !mirror.Equal(mirrorSnapshot(t, source), mirrorSnapshot(t, target)) {
			t.Fatal("fault did not occur after guest mutation")
		}
		out = callRefused(t, ctx, cs, "vm_push", args)
		if out["error"] != "plan_stale" || b.applies.Load() != 1 {
			t.Fatalf("lost apply replayed: %v", out)
		}
	})
}

func TestMirrorTransferGateSurvivesDropAndCancellation(t *testing.T) {
	m := &Manager{Backend: &mirrorBackend{}}
	unlock, err := m.lockTransfer(context.Background(), "CAD")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := m.Client("CAD"); err != nil {
		t.Fatal(err)
	}
	m.Drop("A")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if release, err := m.lockTransfer(ctx, "CAD"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("Drop replaced the held transfer gate: %v", err)
	}
	// The lock is per VM; another VM remains usable while CAD is held.
	release, err := m.lockTransfer(context.Background(), "other-vm")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestMirrorPlanCacheExpiryBindingAndBound(t *testing.T) {
	s := &mirrorPlans{}
	for i := 0; i < maxMirrorPlans+1; i++ {
		s.add(&mirrorPlan{ID: fmt.Sprint(i), owner: "task", vmID: "A", ExpiresAt: time.Now().Add(time.Minute)})
	}
	if len(s.plans) != maxMirrorPlans || s.take("0", "task", "A") != nil {
		t.Fatal("abandoned plans are not bounded")
	}
	if s.take("1", "other", "A") != nil || s.take("1", "task", "B") != nil || s.take("1", "task", "A") == nil {
		t.Fatal("incorrect task/VM binding")
	}
	s.add(&mirrorPlan{ID: "expired", owner: "task", vmID: "A", ExpiresAt: time.Now().Add(-time.Second)})
	if s.take("expired", "task", "A") != nil {
		t.Fatal("expired plan accepted")
	}
}

func TestMirrorMCPScopedEndTurnDiscardsOnlyMatchingPlans(t *testing.T) {
	ctx, cs := mirrorSession(t, &mirrorBackend{})
	source := t.TempDir()
	mirrorFile(t, source, "new.dll", "new")
	ended := mirrorArgs(source, t.TempDir(), "owner")
	otherVM := mirrorArgs(source, t.TempDir(), "owner")
	otherVM["vm"] = "other-vm"
	otherTask := mirrorArgs(source, t.TempDir(), "other-task")
	for _, args := range []map[string]any{ended, otherVM, otherTask} {
		planMirror(t, ctx, cs, args)
	}
	var out map[string]any
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"task_id": "owner", "vm": "CAD"}, &out)
	refused := callRefused(t, ctx, cs, "vm_push", ended)
	if refused["error"] != "plan_stale" {
		t.Fatalf("scoped cleanup retained its plan: %v", refused)
	}
	// Release ownership acquired by the refused write before checking isolation.
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"task_id": "owner", "vm": "CAD"}, &out)
	for _, args := range []map[string]any{otherVM, otherTask} {
		callJSON(t, ctx, cs, "vm_push", args, &out)
		if out["status"] != "complete" {
			t.Fatalf("scoped cleanup invalidated an unrelated plan: %v", out)
		}
	}
}

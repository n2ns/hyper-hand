package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/mirror"
	"hyperhand/internal/proto"
)

const mirrorPlanTTL = 10 * time.Minute
const maxMirrorPlans = 16

type mirrorPlan struct {
	ID        string          `json:"plan_id"`
	VM        string          `json:"vm"`
	HostPath  string          `json:"host_path"`
	GuestPath string          `json:"guest_path"`
	Force     bool            `json:"force"`
	ExpiresAt time.Time       `json:"expires_at"`
	Changes   []mirror.Change `json:"changes"`
	Summary   map[string]int  `json:"summary"`
	owner     string
	vmID      string
	source    mirror.Manifest
	target    mirror.Manifest
}

// Plans contain manifests only. Bound memory even if a client abandons its task.
type mirrorPlans struct {
	mu    sync.Mutex
	plans []*mirrorPlan
}

func (s *mirrorPlans) add(p *mirrorPlan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := make([]*mirrorPlan, 0, maxMirrorPlans)
	for _, old := range s.plans {
		if time.Now().Before(old.ExpiresAt) {
			active = append(active, old)
		}
	}
	if len(active) >= maxMirrorPlans {
		active = active[len(active)-maxMirrorPlans+1:]
	}
	s.plans = append(active, p)
}

func (s *mirrorPlans) take(id, owner, vmID string) *mirrorPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.plans {
		if p.ID != id || p.owner != owner || p.vmID != vmID {
			continue
		}
		s.plans = append(s.plans[:i:i], s.plans[i+1:]...)
		if time.Now().Before(p.ExpiresAt) {
			return p
		}
		return nil
	}
	return nil
}

func (s *mirrorPlans) discard(owner, vm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]*mirrorPlan, 0, len(s.plans))
	for _, p := range s.plans {
		if p.owner != owner || (vm != "" && !strings.EqualFold(vm, p.VM)) {
			kept = append(kept, p)
		}
	}
	s.plans = kept
}

func validatePushMode(in pushIn) error {
	if in.Mode != "" && in.Mode != "copy" && in.Mode != "mirror" {
		return refuse(codeInvalidArgument, "pass mode copy or mirror", nil, "unknown push mode %q", in.Mode)
	}
	if in.Mode != "mirror" {
		if in.Phase != "" || in.PlanID != "" {
			return refuse(codeInvalidArgument, "use mode mirror for phase and plan_id", nil, "copy does not accept phase or plan_id")
		}
		return nil
	}
	if in.Phase != "" && in.Phase != "plan" && in.Phase != "apply" {
		return refuse(codeInvalidArgument, "pass phase plan or apply", nil, "unknown mirror phase %q", in.Phase)
	}
	if (in.Phase == "apply") != (in.PlanID != "") {
		return refuse(codeInvalidArgument, "plan without plan_id, then apply with the returned plan_id", nil, "plan_id is required only for mirror apply")
	}
	return nil
}

// Unlike Client.gate this covers the whole transfer, not one wire request. It
// also survives Manager.Drop after a lifecycle operation replaces the client.
func (m *Manager) lockTransfer(ctx context.Context, vm string) (func(), error) {
	v, err := m.backend().Find(vm)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.transfers == nil {
		m.transfers = map[string]chan struct{}{}
	}
	gate := m.transfers[v.ID]
	if gate == nil {
		gate = make(chan struct{}, 1)
		m.transfers[v.ID] = gate
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case gate <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		<-gate
		return nil, err
	}
	return func() { <-gate }, nil
}

const mirrorNext = "call vm_push with mode mirror and phase plan again; inspect its changes before applying the new plan"

func staleMirror(reason string) error {
	return refuse("plan_stale", mirrorNext, map[string]any{"status": "plan_stale"}, "%s", reason)
}

func (d *deps) pushMirror(ctx context.Context, in pushIn) (*mcp.CallToolResult, error) {
	v, err := d.raw.Find(in.VM)
	if err != nil {
		return nil, vmErr(err)
	}
	hostPath, err := filepath.Abs(in.HostPath)
	if err != nil {
		return nil, refuse(codeInvalidArgument, "pass a local host directory", nil, "%v", err)
	}
	guestPath := filepath.Clean(in.GuestPath)
	c, err := d.m.Client(v.Name)
	if err != nil {
		return nil, vmErr(err)
	}
	if in.Phase != "apply" {
		source, err := mirror.Scan(ctx, hostPath, false)
		if err != nil {
			return nil, refuse(codeInvalidArgument, "fix the source directory before planning mirror", nil, "source: %v", err)
		}
		var target mirror.Manifest
		if _, err := c.Call(ctx, proto.OpMirrorScan, proto.PathArgs{Path: guestPath}, nil, &target); err != nil {
			return nil, agentErr(err)
		}
		changes, err := mirror.Diff(source, target, in.Force)
		if err != nil {
			return nil, refuse(codeInvalidArgument, "resolve the reported directory conflict before planning mirror", nil, "%v", err)
		}
		p := &mirrorPlan{ID: "mirror-" + newRunID(), VM: v.Name, vmID: v.ID, HostPath: hostPath,
			GuestPath: guestPath, Force: in.Force, ExpiresAt: time.Now().UTC().Add(mirrorPlanTTL),
			Changes: changes, Summary: map[string]int{}, owner: d.taskRunID(ctx), source: source, target: target}
		for _, change := range changes {
			p.Summary[change.Action]++
		}
		d.mirrors.add(p)
		// Nothing is written by a plan; next spells out the apply call so that the caller does not stop here.
		next := fmt.Sprintf("nothing was written yet: inspect changes, then call vm_push with mode mirror, phase apply, plan_id %s and the same vm, host_path, guest_path, force (%t) and task_id before %s (the plan is single-use)",
			p.ID, p.Force, p.ExpiresAt.Format(time.RFC3339Nano))
		return jsonResult(struct {
			Status string `json:"status"`
			*mirrorPlan
			Next string `json:"next"`
		}{"planned", p, next})
	}

	// A consumed plan is never replayed, even after a failed or lost response.
	p := d.mirrors.take(in.PlanID, d.taskRunID(ctx), v.ID)
	if p == nil {
		return nil, staleMirror("mirror plan is missing, expired, consumed or belongs to another task or VM")
	}
	if !strings.EqualFold(hostPath, p.HostPath) || !strings.EqualFold(guestPath, p.GuestPath) || in.Force != p.Force {
		return nil, staleMirror("paths or force differ from the mirror plan")
	}
	var target mirror.Manifest
	if _, err := c.Call(ctx, proto.OpMirrorScan, proto.PathArgs{Path: p.GuestPath}, nil, &target); err != nil {
		return nil, agentErr(err)
	}
	if !mirror.Equal(target, p.target) {
		return nil, staleMirror("destination changed since planning")
	}

	// Capture validated bytes before sending. The guest rechecks the destination
	// after it receives this stream, and does not delete until copies verify.
	f, err := os.CreateTemp("", "hyperhand-mirror-*.hhpart")
	if err != nil {
		return nil, err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if err := mirror.WritePayload(ctx, f, p.HostPath, p.source, p.Changes); err != nil {
		return nil, staleMirror(fmt.Sprintf("cannot capture planned source: %v", err))
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	var result mirror.Result
	args := proto.MirrorApplyArgs{Path: p.GuestPath, Source: p.source, Target: p.target, Force: p.Force}
	if _, err := c.CallIO(ctx, proto.OpMirrorApply, args, f, st.Size(), nil, &result); err != nil {
		if strings.HasPrefix(err.Error(), "unknown op") {
			return nil, agentErr(err)
		}
		return nil, refuse("mirror_unknown", mirrorNext, map[string]any{
			"status": "unknown", "plan_id": p.ID, "unknown": p.Changes,
		}, "mirror response was not confirmed: %v; destination may have changed", err)
	}
	if result.Status != "complete" {
		code := "mirror_partial"
		if result.Status == "plan_stale" {
			code = "plan_stale"
		} else if result.Status != "partial" {
			return nil, refuse("mirror_unknown", mirrorNext, map[string]any{"status": "unknown", "plan_id": p.ID, "unknown": p.Changes}, "unrecognized mirror result status %q", result.Status)
		}
		fields := map[string]any{}
		b, _ := json.Marshal(result)
		_ = json.Unmarshal(b, &fields)
		fields["plan_id"] = p.ID
		return nil, refuse(code, mirrorNext, fields, "mirror did not complete; inspect failed, completed and pending")
	}
	return jsonResult(struct {
		mirror.Result
		PlanID string `json:"plan_id"`
		VM     string `json:"vm"`
	}{result, p.ID, v.Name})
}

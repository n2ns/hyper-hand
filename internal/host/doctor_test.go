package host

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

// doctorFake answers the agent ops the doctor sends.
type doctorFake struct {
	ping    proto.PingResult
	pingErr error
	session proto.SessionStateResult
	windows proto.WindowsResult
	ops     []string
}

func (f *doctorFake) call(_ context.Context, _, op string, _ any, _ []byte, result any) ([]byte, error) {
	f.ops = append(f.ops, op)
	var r any
	switch op {
	case proto.OpPing:
		if f.pingErr != nil {
			return nil, f.pingErr
		}
		r = f.ping
	case proto.OpSessionState:
		r = f.session
	case proto.OpListWindows:
		r = f.windows
	default:
		return nil, errors.New("unexpected op " + op)
	}
	b, _ := json.Marshal(r)
	return nil, json.Unmarshal(b, result)
}

func fixedCheck(c doctorCheck) func() doctorCheck { return func() doctorCheck { return c } }

func TestDoctorReportShape(t *testing.T) {
	f := &doctorFake{
		ping:    proto.PingResult{Version: "0.3.0", Protocol: proto.Protocol, Hostname: "PC", User: `PC\tok`},
		session: proto.SessionStateResult{Console: true},
		windows: proto.WindowsResult{AgentIntegrity: "medium"},
	}
	dr := doctor{
		host: []func() doctorCheck{
			fixedCheck(doctorCheck{Check: "host.service", Status: doctorOK, Detail: "running"}),
			fixedCheck(doctorCheck{Check: "host.pipe", Status: doctorFail, Detail: "refused", Suggestion: "Start-Service HyperHandService"}),
		},
		find: func(string) (hyperv.VM, error) { return hyperv.VM{Name: "Win10", State: "Running"}, nil },
		call: f.call,
	}
	b, err := json.Marshal(map[string]any{"checks": dr.run(context.Background(), "")})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Checks []struct {
			Check, Status, Detail, Suggestion string
		} `json:"checks"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"host.service:ok", "host.pipe:fail", "vm.power:ok", "guest.agent:ok", "guest.session:ok", "guest.integrity:ok"}
	if len(out.Checks) != len(want) {
		t.Fatalf("checks %s", b)
	}
	for i, c := range out.Checks {
		if c.Check+":"+c.Status != want[i] {
			t.Errorf("check %d = %s:%s, want %s", i, c.Check, c.Status, want[i])
		}
		if c.Status != doctorOK && c.Suggestion == "" {
			t.Errorf("%s has no suggestion", c.Check)
		}
		if c.Detail == "" {
			t.Errorf("%s has no detail", c.Check)
		}
	}
}

func TestDoctorGuestFailures(t *testing.T) {
	statuses := func(cs []doctorCheck) map[string]doctorCheck {
		m := map[string]doctorCheck{}
		for _, c := range cs {
			m[c.Check] = c
		}
		return m
	}
	// VM off: no agent checks, a suggestion to start it.
	dr := doctor{find: func(string) (hyperv.VM, error) { return hyperv.VM{Name: "Win10", State: "Off"}, nil }, call: (&doctorFake{}).call}
	cs := dr.run(context.Background(), "Win10")
	if len(cs) != 1 || cs[0].Check != "vm.power" || cs[0].Status != doctorFail || cs[0].Suggestion != `call vm_start with vm "Win10"` {
		t.Errorf("off: %+v", cs)
	}
	// Unknown VM.
	dr.find = func(string) (hyperv.VM, error) { return hyperv.VM{}, errors.New(`VM "x" not found`) }
	if cs := dr.run(context.Background(), "x"); len(cs) != 1 || cs[0].Status != doctorFail || cs[0].Suggestion == "" {
		t.Errorf("unknown: %+v", cs)
	}
	// Agent not answering: no session or integrity checks.
	running := func(string) (hyperv.VM, error) { return hyperv.VM{Name: "Win10", State: "Running"}, nil }
	f := &doctorFake{pingErr: errors.New("connect to agent: refused")}
	dr = doctor{find: running, call: f.call}
	m := statuses(dr.run(context.Background(), ""))
	if m["guest.agent"].Status != doctorFail || m["guest.agent"].Suggestion == "" {
		t.Errorf("no agent: %+v", m)
	}
	if _, ok := m["guest.session"]; ok || len(f.ops) != 1 {
		t.Errorf("session queried without an agent: %v %v", m, f.ops)
	}
	// Outdated agent, locked non-console session, unknown integrity.
	f = &doctorFake{ping: proto.PingResult{Version: "0.2.0", Protocol: proto.Protocol - 1}, session: proto.SessionStateResult{Locked: true}}
	dr = doctor{find: running, call: f.call}
	m = statuses(dr.run(context.Background(), ""))
	if a := m["guest.agent"]; a.Status != doctorFail || a.Suggestion != "call vm_update_agent" {
		t.Errorf("outdated: %+v", a)
	}
	if s := m["guest.session"]; s.Status != doctorFail || s.Suggestion == "" {
		t.Errorf("non-console: %+v", s)
	}
	if i := m["guest.integrity"]; i.Status != doctorWarn {
		t.Errorf("integrity: %+v", i)
	}
	// Locked console session is a warning naming vm_unlock; a UAC prompt too.
	f = &doctorFake{ping: proto.PingResult{Protocol: proto.Protocol}, session: proto.SessionStateResult{Console: true, Locked: true}, windows: proto.WindowsResult{AgentIntegrity: "high"}}
	dr = doctor{find: running, call: f.call}
	if s := statuses(dr.run(context.Background(), ""))["guest.session"]; s.Status != doctorWarn || s.Suggestion != "call vm_unlock (needs the unlock password stored in the HyperHand tray)" {
		t.Errorf("locked: %+v", s)
	}
	f.session = proto.SessionStateResult{Console: true, Consent: true}
	if s := statuses(dr.run(context.Background(), ""))["guest.session"]; s.Status != doctorWarn || s.Detail == "" {
		t.Errorf("UAC: %+v", s)
	}
}

// The real host checks run without touching anything and always return a status; their outcome depends on the machine.
func TestDoctorHostChecksReturnStatus(t *testing.T) {
	for _, f := range newDoctor(&vmToolsBackend{}, nil).host {
		c := f()
		if c.Check == "" || c.Detail == "" || (c.Status != doctorOK && c.Status != doctorWarn && c.Status != doctorFail) {
			t.Errorf("host check %+v", c)
		}
		if c.Status != doctorOK && c.Suggestion == "" {
			t.Errorf("%s has no suggestion", c.Check)
		}
	}
	MCPURL = "http://127.0.0.1:8770/mcp"
	defer func() { MCPURL = "" }()
	if c := checkMCP(); c.Status != doctorOK || c.Detail != "serving http://127.0.0.1:8770/mcp" {
		t.Errorf("mcp: %+v", c)
	}
}

// host.enhanced_session warns only when the guest session is actually off the console.
func TestDoctorEnhancedSessionWarnsOnlyWhenSessionTaken(t *testing.T) {
	allowed := doctorCheck{Check: "host.enhanced_session", Status: doctorWarn, Detail: "allowed", Suggestion: "turn it off"}
	for _, c := range []struct {
		name    string
		state   string
		session proto.SessionStateResult
		want    string
	}{
		{"console", "Running", proto.SessionStateResult{Console: true}, doctorOK},
		{"taken", "Running", proto.SessionStateResult{}, doctorWarn},
		{"vm off", "Off", proto.SessionStateResult{}, doctorOK},
	} {
		f := &doctorFake{ping: proto.PingResult{Protocol: proto.Protocol}, session: c.session, windows: proto.WindowsResult{AgentIntegrity: "medium"}}
		dr := doctor{
			host: []func() doctorCheck{fixedCheck(allowed)},
			find: func(string) (hyperv.VM, error) { return hyperv.VM{Name: "Win10", State: c.state}, nil },
			call: f.call,
		}
		got := dr.run(context.Background(), "Win10")[0]
		if got.Check != "host.enhanced_session" || got.Status != c.want || (c.want == doctorOK && got.Suggestion != "") {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

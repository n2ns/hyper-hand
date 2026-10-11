package broker

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"
)

func rawFrame(header string, size uint64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint32(b[:4], uint32(len(header)))
	binary.BigEndian.PutUint64(b[4:], size)
	return append(b, header...)
}

func TestUntrustedHeaderBounds(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{"oversized header", rawFrame(strings.Repeat(" ", maxHeader+1), 0)},
		{"oversized payload", rawFrame(`{"op":"copy","guest_path":"C:\\test"}`, maxCopy+1)},
		{"uint64 overflow", rawFrame(`{"op":"copy"}`, ^uint64(0))},
		{"truncated header", rawFrame(`{"op":"list"}`, 0)[:14]},
		{"host path injection", rawFrame(`{"op":"copy","guest_path":"C:\\test","host_path":"C:\\secret"}`, 0)},
		{"arbitrary service injection", rawFrame(`{"op":"dial","vm":"x","service_id":"x"}`, 0)},
		{"trailing document", rawFrame(`{"op":"list"}{"op":"stop"}`, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r request
			if _, err := readHeader(bytes.NewReader(tt.frame), &r, maxCopy); err == nil {
				t.Fatal("accepted unsafe header")
			}
		})
	}
}

func TestOperationBoundary(t *testing.T) {
	tests := []struct {
		r    request
		size int64
	}{
		{request{Op: "exec", Text: "cmd"}, 0},
		{request{Op: "list"}, 1},
		{request{Op: "copy", GuestPath: "C:\\test"}, maxCopy + 1},
		{request{Op: "copy"}, 0},
		{request{Op: "checkpoint_restore"}, 0},
		{request{Op: "checkpoint_delete"}, 0},
		{request{Op: "checkpoint_delete", Name: "x"}, 1},
		{request{Op: "click", Button: 4}, 0},
		{request{Op: "drag", X: -1}, 0},
	}
	for _, tt := range tests {
		if err := validateRequest(tt.r, tt.size); err == nil {
			t.Fatalf("accepted %#v", tt)
		}
	}
}

func TestCheckpointOperationsAccepted(t *testing.T) {
	const id = "c85ca8fb-dfc1-4aa3-8f36-532949376cd2"
	for _, op := range []string{"checkpoint_create", "checkpoint_restore", "checkpoint_delete", "checkpoint_rename"} {
		if err := validateRequest(request{Op: op, VM: "Win10", Name: "run-20261010-0812-7f3a-temp-step1", ID: id}, 0); err != nil {
			t.Errorf("%s: %v", op, err)
		}
		if operationTimeout(op) != 15*time.Minute {
			t.Errorf("%s timeout %v", op, operationTimeout(op))
		}
	}
	// Operations on an existing checkpoint take its GUID, never a name alone; create and rename need a name.
	for _, op := range []string{"checkpoint_restore", "checkpoint_delete", "checkpoint_rename"} {
		if err := validateRequest(request{Op: op, VM: "Win10", Name: "x", ID: "not-a-guid"}, 0); err == nil {
			t.Errorf("%s accepted a non-GUID id", op)
		}
	}
	for _, op := range []string{"checkpoint_create", "checkpoint_rename"} {
		if err := validateRequest(request{Op: op, VM: "Win10", ID: id}, 0); err == nil {
			t.Errorf("%s accepted an empty name", op)
		}
	}
	// checkpoint_type takes exactly one of the Set-VM -CheckpointType values in name.
	for _, name := range []string{"Standard", "Production", "ProductionOnly", "Disabled"} {
		if err := validateRequest(request{Op: "checkpoint_type", VM: "Win10", Name: name}, 0); err != nil {
			t.Errorf("checkpoint_type %s: %v", name, err)
		}
	}
	for _, name := range []string{"", "standard", "Standard; Stop-Computer"} {
		if err := validateRequest(request{Op: "checkpoint_type", VM: "Win10", Name: name}, 0); err == nil {
			t.Errorf("checkpoint_type accepted %q", name)
		}
	}
	if operationTimeout("checkpoint_type") != time.Minute {
		t.Errorf("checkpoint_type timeout %v", operationTimeout("checkpoint_type"))
	}
	// checkpoint_delete carries subtree; the field survives the header round trip.
	var b bytes.Buffer
	if err := writeFrame(&b, request{Op: "checkpoint_delete", VM: "Win10", ID: id, Subtree: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	var r request
	if _, err := readHeader(&b, &r, maxCopy); err != nil || !r.Subtree || r.ID != id || validateRequest(r, 0) != nil {
		t.Errorf("checkpoint_delete round trip: %+v, %v", r, err)
	}
	for _, g := range []string{id, "C85CA8FB-DFC1-4AA3-8F36-532949376CD2"} {
		if !isGUID(g) {
			t.Errorf("isGUID(%q) = false", g)
		}
	}
	for _, g := range []string{"", "c85ca8fb-dfc1-4aa3-8f36-532949376cd", "c85ca8fbxdfc1-4aa3-8f36-532949376cd2", "{c85ca8fb-dfc1-4aa3-8f36-532949376cd2}"} {
		if isGUID(g) {
			t.Errorf("isGUID(%q) = true", g)
		}
	}
}

func TestPayloadRemainsStreaming(t *testing.T) {
	var b bytes.Buffer
	if err := writeFrame(&b, request{Op: "copy", GuestPath: "C:\\guest.bin"}, 7, strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
	var r request
	n, err := readHeader(&b, &r, maxCopy)
	if err != nil || n != 7 || b.Len() != 7 {
		t.Fatalf("header consumed payload: size=%d remaining=%d error=%v", n, b.Len(), err)
	}
	got, err := io.ReadAll(&b)
	if err != nil || string(got) != "payload" {
		t.Fatalf("payload: %q %v", got, err)
	}
}

func TestTruncatedCopyDoesNotCompleteFrame(t *testing.T) {
	var b bytes.Buffer
	if err := writeFrame(&b, request{Op: "copy", GuestPath: "C:\\guest.bin"}, 10, strings.NewReader("short")); err == nil {
		t.Fatal("accepted truncated source")
	}
}

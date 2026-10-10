// Package broker isolates privileged Hyper-V operations behind a local service.
package broker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"hyperhand/internal/hyperv"
)

const (
	ServiceName   = "HyperHandService"
	PipePath      = `\\.\pipe\HyperHandService`
	maxHeader     = 64 << 10
	maxCopy       = 1 << 30
	maxScreenshot = 128 << 20
)

type Config struct {
	OwnerSID string `json:"owner_sid"`
}

type request struct {
	Op        string   `json:"op"`
	VM        string   `json:"vm,omitempty"`
	Name      string   `json:"name,omitempty"`
	Text      string   `json:"text,omitempty"`
	GuestPath string   `json:"guest_path,omitempty"`
	X         int      `json:"x,omitempty"`
	Y         int      `json:"y,omitempty"`
	X2        int      `json:"x2,omitempty"`
	Y2        int      `json:"y2,omitempty"`
	Button    int      `json:"button,omitempty"`
	Delta     int      `json:"delta,omitempty"`
	Count     int      `json:"count,omitempty"`
	Modifiers []string `json:"modifiers,omitempty"`
	ID        string   `json:"id,omitempty"`      // checkpoint GUID for checkpoint_restore, checkpoint_delete, checkpoint_rename
	Subtree   bool     `json:"subtree,omitempty"` // checkpoint_delete: the checkpoint and all its descendants
}

type response struct {
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Width  int             `json:"width,omitempty"`
	Height int             `json:"height,omitempty"`
}

func operationTimeout(op string) time.Duration {
	switch op {
	case "copy", "checkpoint_create", "checkpoint_restore", "checkpoint_delete", "checkpoint_rename":
		return 15 * time.Minute
	case "start", "stop", "pause", "checkpoints":
		return time.Minute
	case "save":
		return 6 * time.Minute
	case "screenshot", "click", "drag", "scroll", "keys", "text":
		return 30 * time.Second
	default:
		return 15 * time.Second
	}
}

func readHeader(r io.Reader, dst any, limit uint64) (int64, error) {
	var prefix [12]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return 0, err
	}
	n, size := binary.BigEndian.Uint32(prefix[:4]), binary.BigEndian.Uint64(prefix[4:])
	if n == 0 || n > maxHeader {
		return 0, errors.New("invalid broker header length")
	}
	if size > limit {
		return 0, errors.New("broker payload exceeds limit")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return 0, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return 0, errors.New("trailing broker header data")
	}
	return int64(size), nil
}

func writeFrame(w io.Writer, header any, size int64, src io.Reader) error {
	if size < 0 || size > maxCopy {
		return errors.New("invalid broker payload length")
	}
	b, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(b) == 0 || len(b) > maxHeader {
		return errors.New("broker header exceeds limit")
	}
	var prefix [12]byte
	binary.BigEndian.PutUint32(prefix[:4], uint32(len(b)))
	binary.BigEndian.PutUint64(prefix[4:], uint64(size))
	if _, err := io.Copy(w, bytes.NewReader(append(prefix[:], b...))); err != nil {
		return err
	}
	if size != 0 {
		if src == nil {
			return errors.New("missing broker payload")
		}
		_, err = io.CopyN(w, src, size)
	}
	return err
}

func validateRequest(r request, size int64) error {
	if size < 0 || size > maxCopy {
		return errors.New("invalid broker payload")
	}
	if r.Op != "copy" && size != 0 {
		return errors.New("unexpected broker payload")
	}
	switch r.Op {
	case "list", "find", "start", "stop", "save", "pause", "shutdown", "checkpoints", "checkpoint_create", "checkpoint_restore", "checkpoint_delete", "checkpoint_rename", "screenshot", "keys", "text", "dial":
	case "click":
		if r.Button < 1 || r.Button > 3 {
			return errors.New("invalid mouse button")
		}
		if r.Count < 1 || r.Count > 3 {
			return errors.New("invalid click count")
		}
		if err := hyperv.ValidateModifiers(r.Modifiers); err != nil {
			return err
		}
	case "drag":
		if err := hyperv.ValidateModifiers(r.Modifiers); err != nil {
			return err
		}
	case "scroll":
	case "copy":
		if r.GuestPath == "" {
			return errors.New("guest path required")
		}
	default:
		return fmt.Errorf("unsupported broker operation %q", r.Op)
	}
	if (r.Op == "checkpoint_create" || r.Op == "checkpoint_rename") && r.Name == "" {
		return errors.New("checkpoint name required")
	}
	if (r.Op == "checkpoint_restore" || r.Op == "checkpoint_delete" || r.Op == "checkpoint_rename") && !isGUID(r.ID) {
		return errors.New("checkpoint id must be a GUID")
	}
	if r.Op == "click" || r.Op == "drag" || r.Op == "scroll" {
		if r.X < 0 || r.Y < 0 || r.X > 65535 || r.Y > 65535 || r.X2 < 0 || r.Y2 < 0 || r.X2 > 65535 || r.Y2 > 65535 {
			return errors.New("invalid screen coordinates")
		}
	}
	if r.Delta < -10000 || r.Delta > 10000 {
		return errors.New("invalid scroll delta")
	}
	return nil
}

// isGUID reports whether s is a GUID in the 8-4-4-4-12 hexadecimal form (as Get-VMSnapshot prints Id).
func isGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

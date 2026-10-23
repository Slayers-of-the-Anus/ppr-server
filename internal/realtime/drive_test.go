package realtime

import (
	"testing"
	"time"
)

func TestDriveRequiresFirmwareCapabilityAndValidAction(t *testing.T) {
	h := testServoHub()
	c := testServoCamera(h)
	a := &AdminClient{Username: "operator"}
	now := time.Now()
	if _, ok := h.prepareDriveCommandLocked(c, a, Message{"action": "forward"}, now); ok {
		t.Fatal("legacy servo-only firmware must not allow motion")
	}
	if _, ok := h.prepareDriveCommandLocked(c, a, Message{"action": "stop"}, now); !ok {
		t.Fatal("STOP must remain available even without a drive-ready report")
	}
	c.Drive.Available = true
	if _, ok := h.prepareDriveCommandLocked(c, a, Message{"action": "turn"}, now); ok {
		t.Fatal("unsupported direction accepted")
	}
	command, ok := h.prepareDriveCommandLocked(c, a, Message{"action": "forward"}, now)
	if !ok || command["expiresAt"] != now.Add(400*time.Millisecond).UnixMilli() {
		t.Fatalf("missing bounded motion lease: %+v", command)
	}
	if c.Drive.Direction == "forward" {
		t.Fatal("command must not masquerade as a confirmed motor state")
	}
}

func TestDriveOwnershipAndEmergencyStop(t *testing.T) {
	h := testServoHub()
	c := &Camera{ID: "phone_01", Drive: DriveState{Available: true, Direction: "stop"}}
	a, b := &AdminClient{Username: "a"}, &AdminClient{Username: "b"}
	now := time.Now()
	if _, ok := h.prepareDriveCommandLocked(c, a, Message{"action": "forward"}, now); !ok {
		t.Fatal("first operator rejected")
	}
	if _, ok := h.prepareDriveCommandLocked(c, a, Message{"action": "forward"}, now.Add(time.Millisecond)); ok {
		t.Fatal("unbounded heartbeat accepted")
	}
	if _, ok := h.prepareDriveCommandLocked(c, b, Message{"action": "backward"}, now.Add(150*time.Millisecond)); ok {
		t.Fatal("second viewer took over an active movement")
	}
	if _, ok := h.prepareDriveCommandLocked(c, b, Message{"action": "stop"}, now.Add(160*time.Millisecond)); !ok {
		t.Fatal("second viewer must be allowed to stop")
	}
	if c.DriveOwner != nil {
		t.Fatal("STOP did not release ownership")
	}
	if _, ok := h.prepareDriveCommandLocked(c, b, Message{"action": "backward"}, now.Add(170*time.Millisecond)); !ok {
		t.Fatal("new operator rejected after STOP")
	}
	if _, ok := h.prepareDriveCommandLocked(c, a, Message{"action": "forward"}, now.Add(time.Second)); !ok {
		t.Fatal("expired operator lease was not released")
	}
}

func TestDrivePublicStateStopsOnSignalingLoss(t *testing.T) {
	h := testServoHub()
	c := &Camera{Drive: DriveState{Available: true, Direction: "forward"}}
	s := h.publicDriveStateLocked(c, false)
	if s["available"] != false || s["direction"] != "stop" {
		t.Fatalf("offline drive shown as moving: %+v", s)
	}
}

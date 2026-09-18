package realtime

import (
	"testing"
	"time"

	"phonepilot/server/internal/config"
)

func testServoHub() *Hub {
	return &Hub{cfg: config.Config{
		ServoMinAngle: 0, ServoMaxAngle: 180, ServoCenterAngle: 90,
		ServoDefaultStep: 5, ServoCommandInterval: 0,
	}}
}

func testServoCamera(h *Hub) *Camera {
	servo := h.defaultServoStateLocked()
	servo.Available = true
	return &Camera{ID: "phone_01", Servo: servo}
}

func TestServoCommandsAndValidation(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want int
		ok   bool
	}{
		{name: "left", msg: Message{"action": "left", "step": 40}, want: 85, ok: true},
		{name: "right", msg: Message{"action": "right", "step": 40}, want: 95, ok: true},
		{name: "absolute set", msg: Message{"action": "set", "angle": 130}, want: 130, ok: true},
		{name: "set clamps low", msg: Message{"action": "set", "angle": -20}, want: 0, ok: true},
		{name: "set clamps high", msg: Message{"action": "set", "angle": 250}, want: 180, ok: true},
		{name: "invalid angle", msg: Message{"action": "set", "angle": "bad"}, ok: false},
		{name: "fractional angle", msg: Message{"action": "set", "angle": 95.5}, ok: false},
		{name: "invalid action", msg: Message{"action": "spin"}, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := testServoHub()
			camera := testServoCamera(h)
			command, ok := h.prepareServoCommandLocked(camera, "admin", tt.msg, time.Now())
			if ok != tt.ok {
				t.Fatalf("ok=%v want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if got := intNumber(command["angle"]); got != tt.want {
				t.Fatalf("angle=%d want %d", got, tt.want)
			}
			if got := intNumber(command["step"]); got != 5 {
				t.Fatalf("step=%d; browser step must be ignored", got)
			}
			if camera.Servo.Angle != 90 {
				t.Fatalf("confirmed angle changed before ACK: %d", camera.Servo.Angle)
			}
			if camera.Servo.TargetAngle != tt.want {
				t.Fatalf("target=%d want %d", camera.Servo.TargetAngle, tt.want)
			}
		})
	}
}

func TestServoPendingUsesLatestTargetForRapidCommands(t *testing.T) {
	h := testServoHub()
	camera := testServoCamera(h)
	for i := 0; i < 3; i++ {
		if _, ok := h.prepareServoCommandLocked(camera, "admin", Message{"action": "right"}, time.Now()); !ok {
			t.Fatal("right rejected")
		}
	}
	if camera.Servo.Angle != 90 {
		t.Fatalf("confirmed=%d want 90", camera.Servo.Angle)
	}
	if camera.Servo.TargetAngle != 105 || !camera.Servo.Pending {
		t.Fatalf("state=%+v", camera.Servo)
	}

	// ACK for the first in-flight movement must not erase the newer target.
	h.applyServoStateLocked(camera, Message{"available": true, "angle": 95, "min": 0, "max": 180})
	if camera.Servo.Angle != 95 || camera.Servo.TargetAngle != 105 || !camera.Servo.Pending {
		t.Fatalf("after intermediate ACK: %+v", camera.Servo)
	}
	h.applyServoStateLocked(camera, Message{"available": true, "angle": 105, "min": 0, "max": 180})
	if camera.Servo.Angle != 105 || camera.Servo.TargetAngle != 105 || camera.Servo.Pending {
		t.Fatalf("after final ACK: %+v", camera.Servo)
	}
}

func TestServoUnavailableAndErrorClearPending(t *testing.T) {
	h := testServoHub()
	camera := testServoCamera(h)
	if _, ok := h.prepareServoCommandLocked(camera, "admin", Message{"action": "right"}, time.Now()); !ok {
		t.Fatal("command rejected")
	}
	h.applyServoErrorLocked(camera)
	if camera.Servo.Available || camera.Servo.Pending || camera.Servo.TargetAngle != camera.Servo.Angle {
		t.Fatalf("error state=%+v", camera.Servo)
	}
	if _, ok := h.prepareServoCommandLocked(camera, "admin", Message{"action": "left"}, time.Now()); ok {
		t.Fatal("command accepted while unavailable")
	}

	camera.Servo.Available = true
	if _, ok := h.prepareServoCommandLocked(camera, "admin", Message{"action": "right"}, time.Now()); !ok {
		t.Fatal("command rejected")
	}
	h.applyServoStateLocked(camera, Message{"available": false, "angle": 90, "min": 0, "max": 180})
	if camera.Servo.Available || camera.Servo.Pending {
		t.Fatalf("unavailable state=%+v", camera.Servo)
	}
}

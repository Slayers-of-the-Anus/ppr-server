package realtime

import "time"

const driveLease = 700 * time.Millisecond

func validDriveAction(action string) bool {
	return action == "forward" || action == "backward" || action == "stop"
}

// A movement is a short lease refreshed by the operator, never a latched command.
func (h *Hub) prepareDriveCommandLocked(c *Camera, admin *AdminClient, msg Message, now time.Time) (Message, bool) {
	action := str(msg["action"])
	if c == nil || !validDriveAction(action) || (action != "stop" && !c.Drive.Available) {
		return nil, false
	}
	if action != "stop" {
		if c.DriveOwner != nil && c.DriveOwner != admin && now.Sub(c.LastDriveAt) < driveLease {
			return nil, false
		}
		if c.DriveOwner == admin && now.Sub(c.LastDriveAt) < 80*time.Millisecond {
			return nil, false
		}
		c.DriveOwner, c.LastDriveAt = admin, now
	} else {
		c.DriveOwner = nil
	}
	return Message{"type": "drive_command", "cameraId": c.ID, "action": action,
		"expiresAt": now.Add(400 * time.Millisecond).UnixMilli(), "requestedBy": admin.Username}, true
}

func (h *Hub) handleDriveCommandLocked(admin *AdminClient, msg Message) {
	c := h.cameras[str(msg["cameraId"])]
	if c == nil || c.Conn == nil || c.Conn.IsClosed() {
		return
	}
	command, ok := h.prepareDriveCommandLocked(c, admin, msg, time.Now())
	if ok && !h.sendToCameraLocked(c, command) {
		c.DriveOwner = nil
	}
}

func (h *Hub) stopDriveLocked(c *Camera) {
	h.sendToCameraLocked(c, Message{"type": "drive_command", "cameraId": c.ID, "action": "stop"})
	c.DriveOwner = nil
}

func (h *Hub) applyDriveStateLocked(c *Camera, msg Message) {
	available := boolean(msg["available"])
	direction := str(msg["direction"])
	if !validDriveAction(direction) || !available {
		direction = "stop"
	}
	next := DriveState{Available: available, Direction: direction}
	if c.Drive == next {
		return
	}
	c.Drive = next
	h.broadcastDriveStateLocked(c)
}

func (h *Hub) publicDriveStateLocked(c *Camera, online bool) Message {
	direction := c.Drive.Direction
	if !online || !c.Drive.Available || !validDriveAction(direction) {
		direction = "stop"
	}
	return Message{"available": c.Drive.Available && online, "direction": direction}
}

func (h *Hub) broadcastDriveStateLocked(c *Camera) {
	online := c.Conn != nil && !c.Conn.IsClosed()
	h.broadcastLocked(Message{"type": "drive_state", "cameraId": c.ID, "drive": h.publicDriveStateLocked(c, online)})
}

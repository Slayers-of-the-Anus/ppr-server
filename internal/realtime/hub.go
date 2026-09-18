package realtime

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"phonepilot/server/internal/config"
	"phonepilot/server/internal/ws"
)

func NewHub(cfg config.Config) *Hub {
	h := &Hub{
		cfg:      cfg,
		cameras:  map[string]*Camera{},
		admins:   map[*AdminClient]struct{}{},
		sessions: map[string]*ViewerSession{},
		stop:     make(chan struct{}),
	}
	go h.pingLoop()
	return h
}

func (h *Hub) Close() {
	select {
	case <-h.stop:
		return
	default:
		close(h.stop)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for a := range h.admins {
		a.Conn.Close(ws.CloseGoingAway, "server-shutdown")
	}
	for _, c := range h.cameras {
		if c.DisconnectTimer != nil {
			c.DisconnectTimer.Stop()
		}
		c.Conn.Close(ws.CloseGoingAway, "server-shutdown")
	}
}

func (h *Hub) HandleAdmin(conn *WSConn, identity AdminIdentity) {
	admin := &AdminClient{Conn: conn, Username: identity.Username, ViewerID: identity.ViewerID}
	conn.Underlying().SetReadLimit(256 * 1024)
	conn.Underlying().SetPongHandler(func(string) error { conn.MarkAlive(); return nil })

	h.mu.Lock()
	h.admins[admin] = struct{}{}
	requestedActive := ""
	if validSessionID(identity.ActiveSessionID) {
		requestedActive = identity.ActiveSessionID
	}
	resumed := ""
	for id, s := range h.sessions {
		if s.Username != admin.Username || s.ViewerID != admin.ViewerID {
			continue
		}
		if requestedActive != "" && id == requestedActive {
			stopTimer(&s.ViewerGraceTimer)
			s.Admin = admin
			resumed = id
			h.flushAdminQueueLocked(s)
			h.notifyRoleLocked(s)
			if s.Role == "relay" && s.MediaState != "live" {
				h.startRelayIfPossibleLocked(s)
			}
		} else {
			h.closeViewerSessionLocked(id, "viewer-reloaded", true, true)
		}
	}
	ready := Message{
		"type": "admin_ready", "cameras": h.cameraListLocked(), "iceServers": h.cfg.IceServers,
		"maxViewersPerCamera": h.cfg.MaxViewersPerCamera, "viewerId": admin.ViewerID,
		"resumedSessionId": nullableString(resumed), "topology": "browser-relay",
	}
	conn.WriteJSON(ready)
	if resumed != "" {
		if s := h.sessions[resumed]; s != nil {
			h.publishViewerListLocked(s.CameraID)
		}
	}
	h.mu.Unlock()

	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		h.mu.Lock()
		switch str(msg["type"]) {
		case "webrtc_start":
			h.startWatchingLocked(admin, msg)
		case "servo_command":
			h.handleServoCommandLocked(admin, msg)
		case "webrtc_answer", "webrtc_ice", "webrtc_stop", "webrtc_state", "webrtc_media":
			h.relayAdminMessageLocked(admin, msg)
		case "relay_offer", "relay_answer", "relay_ice", "relay_error":
			h.relayBrowserMeshMessageLocked(admin, msg)
		}
		h.mu.Unlock()
	}

	h.mu.Lock()
	delete(h.admins, admin)
	affected := map[string]struct{}{}
	for _, s := range h.sessions {
		if s.Admin != admin {
			continue
		}
		s.Admin = nil
		affected[s.CameraID] = struct{}{}
		h.armViewerGraceLocked(s)
	}
	for cameraID := range affected {
		h.publishViewerListLocked(cameraID)
	}
	if len(affected) == 0 {
		h.broadcastListLocked()
	}
	h.mu.Unlock()
	conn.Close(ws.CloseNormalClosure, "closed")
}

func (h *Hub) HandleCamera(conn *WSConn, id, name string) {
	conn.Underlying().SetReadLimit(256 * 1024)
	var camera *Camera

	h.mu.Lock()
	previous := h.cameras[id]
	wasInGrace := previous != nil && previous.DisconnectTimer != nil
	if previous != nil && previous.DisconnectTimer != nil {
		previous.DisconnectTimer.Stop()
		previous.DisconnectTimer = nil
	}
	if previous != nil && previous.Conn != nil && !previous.Conn.IsClosed() && previous.Conn != conn {
		stale := !previous.Conn.MarkForPing() || time.Since(previous.LastSeen) > 20*time.Second
		previous.Conn.MarkAlive()
		if !stale {
			log.Printf("[camera] rejected duplicate live cameraId=%s", id)
			h.mu.Unlock()
			conn.Close(4009, "camera-id-in-use")
			return
		}
		previous.Conn.Close(ws.CloseNormalClosure, "replaced-stale-connection")
	}
	if previous == nil {
		camera = &Camera{ID: id, Name: name, LastSeen: time.Now(), Servo: h.defaultServoStateLocked()}
		h.cameras[id] = camera
	} else {
		camera = previous
	}
	camera.Name = name
	camera.Conn = conn
	camera.LastSeen = time.Now()
	camera.DisconnectTimer = nil
	camera.IntentionalDisconnect = false
	if !wasInGrace {
		camera.Available = false
		h.applyServoErrorLocked(camera)
	}
	conn.Underlying().SetPongHandler(func(string) error {
		conn.MarkAlive()
		h.mu.Lock()
		if camera.Conn == conn {
			camera.LastSeen = time.Now()
		}
		h.mu.Unlock()
		return nil
	})
	log.Printf("[camera] %s signaling connected", id)
	conn.WriteJSON(Message{"type": "camera_ready", "cameraId": id, "iceServers": h.cfg.IceServers, "serverTime": time.Now().UnixMilli(), "maxViewers": h.cfg.MaxViewersPerCamera, "topology": "browser-relay"})
	conn.WriteJSON(Message{"type": "robot_ready", "robotId": id, "iceServers": h.cfg.IceServers, "serverTime": time.Now().UnixMilli(), "maxViewers": h.cfg.MaxViewersPerCamera, "topology": "browser-relay"})
	h.publishViewerListLocked(id)
	h.mu.Unlock()

	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		h.mu.Lock()
		if camera.Conn != conn {
			h.mu.Unlock()
			break
		}
		camera.LastSeen = time.Now()
		switch str(msg["type"]) {
		case "camera_status":
			camera.Available = boolean(msg["available"])
			camera.Width = intNumber(msg["width"])
			camera.Height = intNumber(msg["height"])
			camera.FPS = intNumber(msg["fps"])
			if !camera.Available {
				h.closeSessionsForCameraLocked(id, "camera-unavailable")
			}
			h.broadcastListLocked()
			if camera.Available && h.sourceForCameraLocked(id) == nil && len(h.sessionsForCameraLocked(id)) > 0 {
				h.promoteRelayToSourceLocked(id)
			}
		case "camera_disconnect":
			camera.IntentionalDisconnect = true
			camera.Available = false
			h.applyServoErrorLocked(camera)
			h.closeSessionsForCameraLocked(id, "camera-disconnected")
			h.broadcastListLocked()
		case "servo_state":
			h.applyServoStateLocked(camera, msg)
			h.broadcastServoStateLocked(camera)
		case "servo_error":
			h.applyServoErrorLocked(camera)
			code := trunc(strDefault(msg["code"], "SERVO_ERROR"), 80)
			detail := trunc(str(msg["detail"]), 300)
			log.Printf("[servo] %s code=%s detail=%s", id, code, detail)
			h.broadcastLocked(Message{"type": "servo_error", "cameraId": id, "code": code, "detail": detail})
			h.broadcastServoStateLocked(camera)
		case "webrtc_offer", "webrtc_ice", "webrtc_error", "webrtc_stop", "webrtc_state":
			h.relayCameraMessageLocked(camera, msg)
		}
		h.mu.Unlock()
	}

	h.mu.Lock()
	if camera.Conn == conn {
		camera.Conn = nil
		camera.LastSeen = time.Now()
		if camera.IntentionalDisconnect {
			camera.Available = false
			h.closeSessionsForCameraLocked(id, "camera-disconnected")
			h.broadcastListLocked()
			log.Printf("[camera] %s disconnected intentionally", id)
		} else {
			log.Printf("[camera] %s signaling disconnected; grace %s", id, h.cfg.CameraDisconnectGrace)
			camera.DisconnectTimer = time.AfterFunc(h.cfg.CameraDisconnectGrace, func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				if h.cameras[id] != camera || (camera.Conn != nil && !camera.Conn.IsClosed()) {
					return
				}
				camera.DisconnectTimer = nil
				camera.Available = false
				h.applyServoErrorLocked(camera)
				h.closeSessionsForCameraLocked(id, "camera-signaling-timeout")
				h.broadcastListLocked()
				log.Printf("[camera] %s signaling grace expired", id)
			})
			h.broadcastListLocked()
		}
	}
	h.mu.Unlock()
	conn.Close(ws.CloseNormalClosure, "closed")
}

func (h *Hub) startWatchingLocked(admin *AdminClient, msg Message) {
	cameraID := strings.TrimSpace(str(msg["cameraId"]))
	sid := str(msg["sessionId"])
	camera := h.cameras[cameraID]
	if !validSessionID(sid) {
		admin.Conn.WriteJSON(Message{"type": "webrtc_error", "code": "INVALID_SESSION", "sessionId": sid})
		return
	}
	if camera == nil || camera.Conn == nil || camera.Conn.IsClosed() || !camera.Available {
		admin.Conn.WriteJSON(Message{"type": "webrtc_error", "code": "CAMERA_OFFLINE", "sessionId": sid, "cameraId": cameraID, "detail": "Camera is not ready"})
		return
	}
	if collision := h.sessions[sid]; collision != nil && (collision.Username != admin.Username || collision.ViewerID != admin.ViewerID) {
		admin.Conn.WriteJSON(Message{"type": "webrtc_error", "code": "SESSION_ID_IN_USE", "sessionId": sid, "cameraId": cameraID, "detail": "Session id is already in use"})
		return
	}
	h.closeSessionsForViewerLocked(admin.Username, admin.ViewerID, "replaced-by-viewer", sid)
	if h.sessions[sid] != nil {
		h.closeViewerSessionLocked(sid, "viewer-restarted-session", false, true)
	}
	if len(h.sessionsForCameraLocked(cameraID)) >= h.cfg.MaxViewersPerCamera {
		admin.Conn.WriteJSON(Message{"type": "webrtc_error", "code": "CAMERA_VIEWER_LIMIT", "sessionId": sid, "cameraId": cameraID, "detail": fmt.Sprintf("Maximum %d viewers", h.cfg.MaxViewersPerCamera)})
		return
	}
	role := "relay"
	if h.sourceForCameraLocked(cameraID) == nil {
		role = "source"
	}
	now := time.Now()
	s := &ViewerSession{
		SessionID: sid, CameraID: cameraID, Admin: admin, Username: admin.Username, ViewerID: admin.ViewerID,
		Role: role, State: "active", ViewerState: "starting", CameraState: "n/a", MediaState: "waiting",
		CreatedAt: now, LastSeen: now, PendingCritical: map[string]Message{},
	}
	if role == "source" {
		s.CameraState = "starting"
	}
	h.sessions[sid] = s
	if role == "source" {
		camera.SourceSessionID = sid
	}
	h.notifyRoleLocked(s)
	h.publishViewerListLocked(cameraID)
	if role == "source" {
		if !h.sendCameraStartLocked(s) {
			h.closeViewerSessionLocked(sid, "camera-signaling-send-failed", true, true)
		}
	} else if !h.startRelayIfPossibleLocked(s) {
		h.queueToAdminLocked(s, Message{"type": "relay_wait", "sessionId": sid, "cameraId": cameraID, "sourceUsername": sourceUsername(h.sourceForCameraLocked(cameraID))})
	}
}

func (h *Hub) relayAdminMessageLocked(admin *AdminClient, msg Message) {
	sid := str(msg["sessionId"])
	s := h.sessions[sid]
	if s == nil || s.Admin != admin || s.ViewerID != admin.ViewerID {
		return
	}
	s.LastSeen = time.Now()
	switch str(msg["type"]) {
	case "webrtc_state":
		s.ViewerState = normalizedState(msg["state"])
		h.publishViewerListLocked(s.CameraID)
		return
	case "webrtc_media":
		s.MediaState = normalizedState(msg["state"])
		if s.MediaState == "live" {
			stopTimer(&s.NegotiationTimer)
		}
		h.publishViewerListLocked(s.CameraID)
		if s.Role == "source" && s.MediaState == "live" {
			h.startPendingRelaysLocked(s.CameraID)
		}
		return
	case "webrtc_stop":
		h.closeViewerSessionLocked(sid, safeReason(msg["reason"], "viewer-stopped"), true, true)
		return
	}
	if s.Role != "source" {
		return
	}
	camera := h.cameras[s.CameraID]
	switch str(msg["type"]) {
	case "webrtc_answer":
		sdp := str(msg["sdp"])
		if sdp != "" && len(sdp) <= 200000 {
			h.sendToCameraLocked(camera, Message{"type": "webrtc_answer", "sessionId": sid, "sdp": sdp})
		}
	case "webrtc_ice":
		if msg["candidate"] != nil {
			h.sendToCameraLocked(camera, Message{"type": "webrtc_ice", "sessionId": sid, "candidate": msg["candidate"]})
		}
	}
}

func (h *Hub) relayBrowserMeshMessageLocked(admin *AdminClient, msg Message) {
	sid := str(msg["sessionId"])
	relay := h.sessions[sid]
	if relay == nil || relay.Role != "relay" {
		return
	}
	source := h.sourceForCameraLocked(relay.CameraID)
	if source == nil {
		return
	}
	fromSource := source.Admin == admin
	fromTarget := relay.Admin == admin
	if !fromSource && !fromTarget {
		return
	}
	typeName := str(msg["type"])
	if fromSource && typeName == "relay_offer" {
		sdp := str(msg["sdp"])
		if sdp != "" && len(sdp) <= 200000 {
			h.queueToAdminLocked(relay, Message{"type": "relay_offer", "sessionId": sid, "cameraId": relay.CameraID, "sourceUsername": source.Username, "sdp": sdp})
		}
	} else if fromTarget && typeName == "relay_answer" {
		sdp := str(msg["sdp"])
		if sdp != "" && len(sdp) <= 200000 {
			h.queueToAdminLocked(source, Message{"type": "relay_answer", "sessionId": sid, "cameraId": relay.CameraID, "sdp": sdp})
		}
	} else if typeName == "relay_ice" && msg["candidate"] != nil {
		payload := Message{"type": "relay_ice", "sessionId": sid, "cameraId": relay.CameraID, "candidate": msg["candidate"]}
		if fromSource {
			h.queueToAdminLocked(relay, payload)
		} else {
			h.queueToAdminLocked(source, payload)
		}
	} else if typeName == "relay_error" {
		detail := trunc(str(msg["detail"]), 500)
		log.Printf("[relay] %s error: %s", sid, detail)
		h.queueToAdminLocked(relay, Message{"type": "webrtc_error", "sessionId": sid, "cameraId": relay.CameraID, "code": "RELAY_ERROR", "detail": detail})
		h.closeViewerSessionLocked(sid, "relay-error", false, true)
	}
}

func (h *Hub) relayCameraMessageLocked(camera *Camera, msg Message) {
	sid := str(msg["sessionId"])
	source := h.sessions[sid]
	if source == nil || source.Role != "source" || source.CameraID != camera.ID || camera.SourceSessionID != sid {
		return
	}
	source.LastSeen = time.Now()
	switch str(msg["type"]) {
	case "webrtc_offer":
		sdp := str(msg["sdp"])
		if sdp != "" && len(sdp) <= 200000 {
			h.queueToAdminLocked(source, Message{"type": "webrtc_offer", "sessionId": sid, "cameraId": camera.ID, "sdp": sdp})
		}
	case "webrtc_ice":
		if msg["candidate"] != nil {
			h.queueToAdminLocked(source, Message{"type": "webrtc_ice", "sessionId": sid, "cameraId": camera.ID, "candidate": msg["candidate"]})
		}
	case "webrtc_state":
		source.CameraState = normalizedState(msg["state"])
		h.queueToAdminLocked(source, Message{"type": "webrtc_camera_state", "sessionId": sid, "cameraId": camera.ID, "state": source.CameraState})
		h.publishViewerListLocked(camera.ID)
	case "webrtc_error":
		code := trunc(strDefault(msg["code"], "ANDROID_WEBRTC_ERROR"), 80)
		detail := trunc(str(msg["detail"]), 500)
		log.Printf("[android-webrtc] %s code=%s detail=%s", sid, code, detail)
		h.queueToAdminLocked(source, Message{"type": "webrtc_error", "sessionId": sid, "cameraId": camera.ID, "code": code, "detail": detail})
		h.closeViewerSessionLocked(sid, "android-"+strings.ToLower(code), false, true)
	case "webrtc_stop":
		h.closeViewerSessionLocked(sid, safeReason(msg["reason"], "camera-stopped"), true, true)
	}
}

func (h *Hub) handleServoCommandLocked(admin *AdminClient, msg Message) {
	cameraID := strings.TrimSpace(str(msg["cameraId"]))
	camera := h.cameras[cameraID]
	if camera == nil || camera.Conn == nil || camera.Conn.IsClosed() {
		return
	}
	now := time.Now()
	if !admin.LastServoAt.IsZero() && now.Sub(admin.LastServoAt) < h.cfg.ServoCommandInterval {
		return
	}
	admin.LastServoAt = now
	command, ok := h.prepareServoCommandLocked(camera, admin.Username, msg, now)
	if !ok {
		return
	}
	if !h.sendToCameraLocked(camera, command) {
		camera.Servo.Pending = false
		camera.Servo.TargetAngle = camera.Servo.Angle
		return
	}
	log.Printf("[servo] camera=%s requested=%d by=%s", cameraID, camera.Servo.TargetAngle, admin.Username)
	h.broadcastServoStateLocked(camera)
}

func (h *Hub) prepareServoCommandLocked(camera *Camera, requestedBy string, msg Message, now time.Time) (Message, bool) {
	if camera == nil || !camera.Servo.Available {
		return nil, false
	}
	action := strings.ToLower(str(msg["action"]))
	step := h.cfg.ServoDefaultStep
	angle := camera.Servo.Angle
	if camera.Servo.Pending {
		angle = camera.Servo.TargetAngle
	}
	switch action {
	case "left":
		angle -= step
	case "right":
		angle += step
	case "center":
		angle = h.cfg.ServoCenterAngle
	case "set":
		if !integerNumber(msg["angle"]) {
			return nil, false
		}
		angle = intNumber(msg["angle"])
	default:
		return nil, false
	}
	lo := max(h.cfg.ServoMinAngle, camera.Servo.Min)
	hi := min(h.cfg.ServoMaxAngle, camera.Servo.Max)
	if lo > hi {
		lo, hi = h.cfg.ServoMinAngle, h.cfg.ServoMaxAngle
	}
	angle = clamp(angle, lo, hi)
	camera.Servo.TargetAngle = angle
	camera.Servo.Pending = angle != camera.Servo.Angle
	camera.Servo.Step = step
	camera.Servo.LastCommandBy = requestedBy
	camera.Servo.LastCommandAt = now
	return Message{"type": "servo_command", "cameraId": camera.ID, "action": action, "angle": angle, "step": step, "requestedBy": requestedBy}, true
}

func (h *Hub) applyServoStateLocked(camera *Camera, msg Message) {
	available := msg["available"] == nil || booleanDefault(msg["available"], true)
	camera.Servo.Available = available
	if finiteNumber(msg["min"]) {
		camera.Servo.Min = clamp(intNumber(msg["min"]), h.cfg.ServoMinAngle, h.cfg.ServoMaxAngle)
	}
	if finiteNumber(msg["max"]) {
		camera.Servo.Max = clamp(intNumber(msg["max"]), camera.Servo.Min, h.cfg.ServoMaxAngle)
	}
	if finiteNumber(msg["angle"]) {
		camera.Servo.Angle = h.clampAngleLocked(intNumber(msg["angle"]))
		if !camera.Servo.Pending || camera.Servo.Angle == camera.Servo.TargetAngle {
			camera.Servo.TargetAngle = camera.Servo.Angle
			camera.Servo.Pending = false
		}
	}
	if !available {
		camera.Servo.Pending = false
		camera.Servo.TargetAngle = camera.Servo.Angle
	}
	if available {
		log.Printf("[servo] camera=%s confirmed=%d", camera.ID, camera.Servo.Angle)
	}
}

func (h *Hub) applyServoErrorLocked(camera *Camera) {
	camera.Servo.Available = false
	camera.Servo.Pending = false
	camera.Servo.TargetAngle = camera.Servo.Angle
}

func (h *Hub) sendCameraStartLocked(s *ViewerSession) bool {
	camera := h.cameras[s.CameraID]
	if camera == nil || !camera.Available || camera.Conn == nil || camera.Conn.IsClosed() {
		return false
	}
	width, height, fps := camera.Width, camera.Height, camera.FPS
	if width == 0 {
		width = 1280
	}
	if height == 0 {
		height = 720
	}
	if fps == 0 {
		fps = 30
	}
	ok := camera.Conn.WriteJSON(Message{
		"type": "webrtc_start", "sessionId": s.SessionID, "viewer": Message{"username": s.Username}, "iceServers": h.cfg.IceServers,
		"video": Message{"width": width, "height": height, "fps": fps},
	})
	if ok {
		s.CameraState = "starting"
		h.armNegotiationTimeoutLocked(s)
	}
	return ok
}

func (h *Hub) startRelayIfPossibleLocked(relay *ViewerSession) bool {
	if relay == nil || relay.Role != "relay" || relay.State == "closed" {
		return false
	}
	source := h.sourceForCameraLocked(relay.CameraID)
	if source == nil || source.MediaState != "live" || source.Admin == nil || source.Admin.Conn.IsClosed() {
		return false
	}
	if relay.Admin == nil || relay.Admin.Conn.IsClosed() {
		return false
	}
	if relay.RelaySourceSessionID == source.SessionID {
		return true
	}
	relay.RelaySourceSessionID = source.SessionID
	relay.ViewerState = "starting"
	relay.MediaState = "waiting"
	h.queueToAdminLocked(relay, Message{"type": "relay_wait", "sessionId": relay.SessionID, "cameraId": relay.CameraID, "sourceUsername": source.Username})
	h.queueToAdminLocked(source, Message{"type": "relay_start", "sessionId": relay.SessionID, "cameraId": relay.CameraID, "target": Message{"username": relay.Username, "viewerId": relay.ViewerID}, "iceServers": h.cfg.IceServers})
	h.armNegotiationTimeoutLocked(relay)
	return true
}

func (h *Hub) startPendingRelaysLocked(cameraID string) {
	for _, s := range h.sessionsForCameraLocked(cameraID) {
		if s.Role == "relay" && s.MediaState != "live" {
			h.startRelayIfPossibleLocked(s)
		}
	}
}

func (h *Hub) promoteRelayToSourceLocked(cameraID string) {
	camera := h.cameras[cameraID]
	if camera == nil || h.sourceForCameraLocked(cameraID) != nil {
		return
	}
	candidates := []*ViewerSession{}
	for _, s := range h.sessionsForCameraLocked(cameraID) {
		if s.Role == "relay" {
			candidates = append(candidates, s)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CreatedAt.Before(candidates[j].CreatedAt) })
	if len(candidates) == 0 {
		camera.SourceSessionID = ""
		h.publishViewerListLocked(cameraID)
		return
	}
	promoted := candidates[0]
	for _, s := range candidates {
		if s.Admin != nil && !s.Admin.Conn.IsClosed() {
			promoted = s
			break
		}
	}
	for _, s := range candidates {
		stopTimer(&s.NegotiationTimer)
		s.MediaState = "waiting"
		s.ViewerState = "starting"
		s.RelaySourceSessionID = ""
		h.queueToAdminLocked(s, Message{"type": "relay_reset", "sessionId": s.SessionID, "cameraId": cameraID, "reason": "source-changed"})
	}
	promoted.Role = "source"
	promoted.CameraState = "starting"
	camera.SourceSessionID = promoted.SessionID
	h.queueToAdminLocked(promoted, Message{"type": "watch_role", "sessionId": promoted.SessionID, "cameraId": cameraID, "role": "source", "sourceUsername": promoted.Username})
	if !h.sendCameraStartLocked(promoted) {
		h.queueToAdminLocked(promoted, Message{"type": "webrtc_error", "sessionId": promoted.SessionID, "cameraId": cameraID, "code": "CAMERA_OFFLINE", "detail": "Camera signaling is not available"})
	}
	h.publishViewerListLocked(cameraID)
}

func (h *Hub) closeViewerSessionLocked(sid, reason string, notify, rebalance bool) bool {
	s := h.sessions[sid]
	if s == nil {
		return false
	}
	delete(h.sessions, sid)
	stopTimer(&s.NegotiationTimer)
	stopTimer(&s.ViewerGraceTimer)
	s.State = "closed"
	camera := h.cameras[s.CameraID]
	wasSource := s.Role == "source" && camera != nil && camera.SourceSessionID == sid
	if notify {
		h.queueToAdminLocked(s, Message{"type": "webrtc_stop", "sessionId": sid, "cameraId": s.CameraID, "reason": reason})
	}
	if wasSource {
		camera.SourceSessionID = ""
		h.sendToCameraLocked(camera, Message{"type": "webrtc_stop", "sessionId": sid, "reason": reason})
		for _, relay := range h.sessionsForCameraLocked(s.CameraID) {
			if relay.Role == "relay" {
				h.queueToAdminLocked(relay, Message{"type": "relay_reset", "sessionId": relay.SessionID, "cameraId": relay.CameraID, "reason": "source-closed"})
			}
		}
	} else if source := h.sourceForCameraLocked(s.CameraID); source != nil {
		h.queueToAdminLocked(source, Message{"type": "relay_stop", "sessionId": sid, "cameraId": s.CameraID, "reason": reason})
	}
	h.publishViewerListLocked(s.CameraID)
	if wasSource && rebalance {
		h.promoteRelayToSourceLocked(s.CameraID)
	}
	return true
}

func (h *Hub) closeSessionsForViewerLocked(username, viewerID, reason, except string) {
	ids := []string{}
	for id, s := range h.sessions {
		if s.Username == username && s.ViewerID == viewerID && id != except {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		h.closeViewerSessionLocked(id, reason, true, true)
	}
}

func (h *Hub) closeSessionsForCameraLocked(cameraID, reason string) {
	ids := []string{}
	for _, s := range h.sessionsForCameraLocked(cameraID) {
		ids = append(ids, s.SessionID)
	}
	for _, id := range ids {
		h.closeViewerSessionLocked(id, reason, true, false)
	}
	if c := h.cameras[cameraID]; c != nil {
		c.SourceSessionID = ""
	}
	h.publishViewerListLocked(cameraID)
}

func (h *Hub) armNegotiationTimeoutLocked(s *ViewerSession) {
	stopTimer(&s.NegotiationTimer)
	sid := s.SessionID
	s.NegotiationTimer = time.AfterFunc(h.cfg.NegotiationTimeout, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		current := h.sessions[sid]
		if current == nil || current != s || current.MediaState == "live" {
			return
		}
		log.Printf("[webrtc] negotiation timeout %s role=%s viewer=%s", sid, s.Role, s.Username)
		h.closeViewerSessionLocked(sid, "negotiation-timeout", true, true)
	})
}

func (h *Hub) armViewerGraceLocked(s *ViewerSession) {
	stopTimer(&s.ViewerGraceTimer)
	sid := s.SessionID
	s.ViewerGraceTimer = time.AfterFunc(h.cfg.ViewerSignalingGrace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		current := h.sessions[sid]
		if current == nil || current != s || (current.Admin != nil && !current.Admin.Conn.IsClosed()) {
			return
		}
		log.Printf("[viewer] signaling grace expired %s/%s", s.Username, s.ViewerID)
		h.closeViewerSessionLocked(sid, "viewer-signaling-timeout", true, true)
	})
}

func (h *Hub) notifyRoleLocked(s *ViewerSession) {
	h.queueToAdminLocked(s, Message{"type": "watch_role", "sessionId": s.SessionID, "cameraId": s.CameraID, "role": s.Role, "sourceUsername": sourceUsername(h.sourceForCameraLocked(s.CameraID))})
}

func (h *Hub) queueToAdminLocked(s *ViewerSession, payload Message) bool {
	if s.Admin != nil && s.Admin.Conn.WriteJSON(payload) {
		return true
	}
	typeName := str(payload["type"])
	if typeName == "webrtc_ice" || typeName == "relay_ice" {
		if len(s.PendingIce) >= 192 {
			s.PendingIce = s.PendingIce[1:]
		}
		s.PendingIce = append(s.PendingIce, cloneMessage(payload))
		return false
	}
	key := typeName + ":" + str(payload["sessionId"])
	if _, exists := s.PendingCritical[key]; !exists {
		s.PendingCriticalOrder = append(s.PendingCriticalOrder, key)
	}
	s.PendingCritical[key] = cloneMessage(payload)
	for len(s.PendingCriticalOrder) > 32 {
		old := s.PendingCriticalOrder[0]
		s.PendingCriticalOrder = s.PendingCriticalOrder[1:]
		delete(s.PendingCritical, old)
	}
	return false
}

func (h *Hub) flushAdminQueueLocked(s *ViewerSession) {
	if s.Admin == nil || s.Admin.Conn.IsClosed() {
		return
	}
	for _, key := range s.PendingCriticalOrder {
		if p := s.PendingCritical[key]; p != nil {
			if !s.Admin.Conn.WriteJSON(p) {
				return
			}
		}
	}
	s.PendingCritical = map[string]Message{}
	s.PendingCriticalOrder = nil
	for len(s.PendingIce) > 0 {
		if !s.Admin.Conn.WriteJSON(s.PendingIce[0]) {
			return
		}
		s.PendingIce = s.PendingIce[1:]
	}
}

func (h *Hub) sessionsForCameraLocked(cameraID string) []*ViewerSession {
	out := []*ViewerSession{}
	for _, s := range h.sessions {
		if s.CameraID == cameraID && s.State != "closed" {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (h *Hub) sourceForCameraLocked(cameraID string) *ViewerSession {
	camera := h.cameras[cameraID]
	if camera == nil || camera.SourceSessionID == "" {
		return nil
	}
	s := h.sessions[camera.SourceSessionID]
	if s != nil && s.Role == "source" && s.CameraID == cameraID && s.State != "closed" {
		return s
	}
	return nil
}

func (h *Hub) viewerInfoForCameraLocked(cameraID string) []Message {
	out := []Message{}
	for _, s := range h.sessionsForCameraLocked(cameraID) {
		sig := "grace"
		if s.Admin != nil && !s.Admin.Conn.IsClosed() {
			sig = "online"
		}
		out = append(out, Message{"sessionId": s.SessionID, "username": s.Username, "role": s.Role, "viewerState": s.ViewerState, "mediaState": s.MediaState, "signaling": sig})
	}
	return out
}

func (h *Hub) cameraStateLocked(c *Camera) Message {
	viewers := h.viewerInfoForCameraLocked(c.ID)
	signalingOnline := c.Conn != nil && !c.Conn.IsClosed()
	inGrace := c.DisconnectTimer != nil
	signaling := "offline"
	if signalingOnline {
		signaling = "connected"
	} else if inGrace {
		signaling = "grace"
	}
	var width, height, fps any
	if c.Width > 0 {
		width = c.Width
	}
	if c.Height > 0 {
		height = c.Height
	}
	if c.FPS > 0 {
		fps = c.FPS
	}
	return Message{
		"id": c.ID, "name": c.Name, "online": signalingOnline || inGrace, "signaling": signaling, "available": c.Available,
		"width": width, "height": height, "fps": fps, "lastSeen": c.LastSeen.UnixMilli(), "viewers": viewers, "viewerCount": len(viewers),
		"maxViewers": h.cfg.MaxViewersPerCamera, "sourceUsername": sourceUsername(h.sourceForCameraLocked(c.ID)), "servo": h.publicServoStateLocked(c, signalingOnline),
	}
}

func (h *Hub) cameraListLocked() []Message {
	out := make([]Message, 0, len(h.cameras))
	for _, c := range h.cameras {
		out = append(out, h.cameraStateLocked(c))
	}
	sort.Slice(out, func(i, j int) bool {
		oi := boolean(out[i]["online"])
		oj := boolean(out[j]["online"])
		if oi != oj {
			return oi
		}
		return str(out[i]["name"]) < str(out[j]["name"])
	})
	return out
}

func (h *Hub) publishViewerListLocked(cameraID string) {
	camera := h.cameras[cameraID]
	if camera == nil {
		return
	}
	viewers := h.viewerInfoForCameraLocked(cameraID)
	h.sendToCameraLocked(camera, Message{"type": "viewer_list", "viewers": viewers, "count": len(viewers), "max": h.cfg.MaxViewersPerCamera})
	h.broadcastListLocked()
}

func (h *Hub) broadcastListLocked() {
	h.broadcastLocked(Message{"type": "camera_list", "cameras": h.cameraListLocked()})
}
func (h *Hub) broadcastLocked(payload Message) {
	for a := range h.admins {
		a.Conn.WriteJSON(payload)
	}
}
func (h *Hub) sendToCameraLocked(c *Camera, payload Message) bool {
	if c == nil || c.Conn == nil {
		return false
	}
	return c.Conn.WriteJSON(payload)
}

func (h *Hub) defaultServoStateLocked() ServoState {
	return ServoState{Angle: h.cfg.ServoCenterAngle, TargetAngle: h.cfg.ServoCenterAngle, Min: h.cfg.ServoMinAngle, Max: h.cfg.ServoMaxAngle, Step: h.cfg.ServoDefaultStep}
}
func (h *Hub) publicServoStateLocked(c *Camera, online bool) Message {
	s := c.Servo
	return Message{"available": s.Available && online, "angle": s.Angle, "targetAngle": s.TargetAngle, "pending": s.Pending, "min": s.Min, "max": s.Max, "step": s.Step, "lastCommandBy": nullableString(s.LastCommandBy)}
}
func (h *Hub) broadcastServoStateLocked(c *Camera) {
	online := c.Conn != nil && !c.Conn.IsClosed()
	h.broadcastLocked(Message{"type": "servo_state", "cameraId": c.ID, "servo": h.publicServoStateLocked(c, online)})
	h.broadcastListLocked()
}
func (h *Hub) clampAngleLocked(v int) int { return clamp(v, h.cfg.ServoMinAngle, h.cfg.ServoMaxAngle) }

func (h *Hub) pingLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.mu.Lock()
			for a := range h.admins {
				if !a.Conn.MarkForPing() {
					a.Conn.Close(ws.CloseGoingAway, "ping-timeout")
					continue
				}
				if !a.Conn.Ping() {
					a.Conn.Close(ws.CloseGoingAway, "ping-failed")
				}
			}
			for _, c := range h.cameras {
				if c.Conn == nil || c.Conn.IsClosed() {
					continue
				}
				if !c.Conn.MarkForPing() {
					c.Conn.Close(ws.CloseGoingAway, "ping-timeout")
					continue
				}
				if !c.Conn.Ping() {
					c.Conn.Close(ws.CloseGoingAway, "ping-failed")
				}
			}
			h.mu.Unlock()
		}
	}
}

func cloneMessage(m Message) Message {
	b, _ := json.Marshal(m)
	var out Message
	_ = json.Unmarshal(b, &out)
	return out
}
func stopTimer(t **time.Timer) {
	if *t != nil {
		(*t).Stop()
		*t = nil
	}
}
func validSessionID(s string) bool {
	if len(s) < 12 || len(s) > 96 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
func normalizedState(v any) string { s := strings.ToLower(trunc(str(v), 32)); return s }
func safeReason(v any, fallback string) string {
	s := str(v)
	if s == "" {
		s = fallback
	}
	return trunc(s, 96)
}
func sourceUsername(s *ViewerSession) any {
	if s == nil {
		return nil
	}
	return s.Username
}
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
func strDefault(v any, d string) string {
	s := str(v)
	if s == "" {
		return d
	}
	return s
}
func boolean(v any) bool { b, _ := v.(bool); return b }
func booleanDefault(v any, d bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return d
}
func intNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(math.Round(n))
	case float32:
		return int(math.Round(float64(n)))
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
func finiteNumber(v any) bool {
	switch n := v.(type) {
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		x := float64(n)
		return !math.IsNaN(x) && !math.IsInf(x, 0)
	case int, int64, json.Number:
		return true
	default:
		return false
	}
}
func integerNumber(v any) bool {
	switch n := v.(type) {
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n
	case float32:
		x := float64(n)
		return !math.IsNaN(x) && !math.IsInf(x, 0) && math.Trunc(x) == x
	case int, int64:
		return true
	case json.Number:
		_, err := n.Int64()
		return err == nil
	default:
		return false
	}
}
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

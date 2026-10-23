# HTTP and WebSocket surface

## HTTP

- `GET /` redirects to login/admin.
- `GET /login` login page.
- `GET /admin` authenticated admin page.
- `GET /camera-test` optional browser diagnostics sender.
- `GET /healthz` process liveness.
- `POST /api/login` creates an in-memory authenticated session.
- `POST /api/logout` destroys it.
- `GET /api/me` returns the current username.
- `GET /api/webrtc-config` returns ICE configuration and viewer limit.
- `POST /api/ws-token` creates a one-minute, one-use token for `/ws/admin`.
- `GET /api/network/info` returns `{"v":2,"serverId":"<UUID>"}` on both listeners.
- `GET /api/network/lan-probe?nonce=<base64url>` returns `{"v":2,"serverId":"<UUID>","nonce":"<same nonce>"}` only for a verified peer on the LAN listener.

## WebSocket endpoints

### `/ws/camera`

Query parameters: `cameraId`, `name`.

The compatibility alias `/ws/robot` accepts `robotId` as well.

When `PHONEPILOT_CAMERA_SECRET_B64URL` is set, the server first sends `{"type":"camera_auth_challenge","v":2,"nonce":"<16 random bytes, unpadded Base64URL>"}`. Within five seconds the phone must answer `{"type":"camera_auth","v":2,"cameraId":"phone_01","proof":"<unpadded Base64URL>"}`. The proof is HMAC-SHA256 with the decoded 32-byte secret over UTF-8 `nonce + "\n" + cameraId`. The URL cameraId and authenticated cameraId must match. Only then does the existing `camera_ready` arrive and registration occur.

After `camera_ready`, Android sends `{"type":"network_state","v":2,"transport":"wifi"}` with `wifi`, `cellular`, or `other`. The server responds `{"type":"network_path","v":2,"path":"lan_wifi","verified":true}`. On the LAN listener, `lan_wifi` requires a peer in the interface subnet and `wifi`; otherwise the result is `unknown`. The main listener maps `cellular` to `mobile`, `wifi` to `remote_wifi`, and all other values to `unknown`. The server ignores forwarded headers for this decision.

Server sends `servo_command` with a validated absolute `angle`. Camera sends authoritative `servo_state {available,angle,min,max}` after ESP32 status/ACK, or `servo_error {code,detail}`. Servo errors do not close camera or WebRTC sessions.

### `/ws/admin`

Query parameter: `token`. The token must have been created by an authenticated browser session and is consumed at upgrade time.

Each `camera_list.cameras[]` and `admin_ready.cameras[]` entry includes `"networkPath":{"path":"lan_wifi","verified":true,"changedAt":1730000000000}`. A closed signaling socket changes this to `offline` immediately, including during grace. The other camera, servo, and WebRTC fields remain present.

Browser sends `servo_command {cameraId,action}` for `left/right`, or adds `angle` for `set`. Server uses configured `SERVO_STEP`, clamps to configured/device limits, and publishes:

```json
{
  "type": "servo_state",
  "cameraId": "phone_01",
  "servo": {
    "available": true,
    "angle": 90,
    "targetAngle": 95,
    "pending": true,
    "min": 0,
    "max": 180,
    "step": 5
  }
}
```

`angle` is confirmed. `targetAngle` is the newest requested absolute target. Intermediate ACKs update `angle` without clearing a newer pending target.

## Robot drive (L298N)

Browser → server: `{"type":"drive_command","cameraId":"phone_01","action":"forward"}`. Actions are `forward`, `backward`, `stop`. Movement buttons and W/S or ↑/↓ refresh the command every 150 ms while held. Release, lost focus, hidden page, camera selection change and logout send STOP. Drive commands never enter a reconnect queue.

Server → phone uses the same fields plus `expiresAt` (server Unix milliseconds, 400 ms after acceptance) and `requestedBy`. Android uses the `camera_ready.serverTime` clock offset and discards expired movement commands. STOP is always accepted. An active operator owns a 700 ms lease; other viewers may stop the robot, but may not take over its direction until the lease is released/expired. Admin disconnect sends STOP. Losing camera signaling immediately disables published drive availability even during grace.

Phone → server: `{"type":"drive_state","available":true,"direction":"stop"}` based on ESP32 `DRIVE_STATE`. Server → browser: `{"type":"drive_state","cameraId":"phone_01","drive":{"available":true,"direction":"stop"}}`. Each camera-list entry also includes `drive`. Drive availability is independent of servo availability; old firmware lacking `DRIVE_STATE` cannot enable movement. Firmware disables the bridges after 700 ms without fresh movement commands.

## State model

The server holds camera presence, viewer sessions, admin sessions and one-use tokens in memory. This is appropriate for one server instance. Before running multiple replicas, move shared session/presence coordination to a shared store and define ownership of camera WebSockets.

## Media topology

The server never handles RTP/video payloads. It only forwards signaling messages. Android establishes one WebRTC connection to the SOURCE browser; additional browsers receive a peer-to-peer relay from SOURCE.

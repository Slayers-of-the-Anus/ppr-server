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

## WebSocket endpoints

### `/ws/camera`

Query parameters: `cameraId`, `name`.

The compatibility alias `/ws/robot` accepts `robotId` as well.

Server sends `servo_command` with a validated absolute `angle`. Camera sends authoritative `servo_state {available,angle,min,max}` after ESP32 status/ACK, or `servo_error {code,detail}`. Servo errors do not close camera or WebRTC sessions.

### `/ws/admin`

Query parameter: `token`. The token must have been created by an authenticated browser session and is consumed at upgrade time.

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

## State model

The server holds camera presence, viewer sessions, admin sessions and one-use tokens in memory. This is appropriate for one server instance. Before running multiple replicas, move shared session/presence coordination to a shared store and define ownership of camera WebSockets.

## Media topology

The server never handles RTP/video payloads. It only forwards signaling messages. Android establishes one WebRTC connection to the SOURCE browser; additional browsers receive a peer-to-peer relay from SOURCE.

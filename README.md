# PhonePilot Go Server

Standalone signaling/control server for PhonePilot Robotics. This is a Go rewrite of the previous Node.js/Express `server.js` implementation.

## What the server owns

- browser login and in-memory sessions;
- web admin UI;
- one-time WebSocket tokens for admin tabs;
- camera registry and connection state;
- WebRTC signaling between Android camera and SOURCE browser;
- browser-to-browser relay signaling for additional viewers;
- SOURCE failover;
- short signaling grace windows;
- servo command validation, pending/confirmed state and rate limiting;
- STUN/TURN configuration distribution.

It does **not** decode, proxy or store the video stream. Video remains peer-to-peer WebRTC.

## Requirements

- Go 1.23+
- optional TURN server for difficult NAT/CGNAT networks

No Node.js/npm runtime is required.

## Local run

```bash
cp .env.example .env
go run ./cmd/server
```

Then open `http://localhost:8080`.

Default development login is `admin / ChangeMe123!`. Change all passwords before exposing the service to a network.

## Build

```bash
go build -o bin/phonepilot-server ./cmd/server
```

Windows:

```powershell
go build -o bin\phonepilot-server.exe .\cmd\server
```

## Docker

```bash
docker build -t phonepilot-server .
docker run --rm -p 8080:8080 --env-file .env phonepilot-server
```

## Important configuration

See `.env.example`. The most important production settings are admin passwords, TURN credentials, `COOKIE_SECURE=true` behind HTTPS, viewer limits, and signaling timeouts.

Servo limits and button increment are server-owned: `SERVO_MIN_ANGLE`, `SERVO_MAX_ANGLE`, `SERVO_STEP`, and `SERVO_COMMAND_INTERVAL_MS`. The browser cannot override `SERVO_STEP`.

## Compatibility

The rewrite preserves the existing public HTTP/WebSocket contract used by the Android application and browser UI:

- `/ws/camera` and legacy `/ws/robot`
- `/ws/admin`
- `/api/login`, `/api/logout`, `/api/me`, `/api/ws-token`, `/api/webrtc-config`
- the v0.7 browser-relay topology and message names
- `servo_command`, `servo_state`, and `servo_error`, with additive `targetAngle`/`pending` fields in public servo state

See `docs/MIGRATION_FROM_NODE.md` and the repository-level `../docs/PROTOCOL.md`.

## Source layout

```text
cmd/server/                 executable entry point
internal/config/            env/.env parsing and typed configuration
internal/httpapi/           HTTP API, auth/session store, WebSocket upgrade
internal/realtime/          camera/viewer hub and signaling state machine
internal/ws/                minimal RFC6455 WebSocket transport (no third-party dependency)
internal/webassets/         embedded admin/login/camera-test frontend
```

The current session/token state is intentionally in memory, matching the practical behavior of the previous Express memory store. If horizontal scaling is introduced later, move sessions/presence to Redis or another shared state layer first.

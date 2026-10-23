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
docker run --rm -p 127.0.0.1:8080:8080 -e PHONEPILOT_MAIN_BIND=0.0.0.0 -v phonepilot-data:/app/data --env-file .env phonepilot-server
```

This Docker command is for local legacy use only. The loopback host port mapping keeps it off the public interface. Use the network setup below for LAN discovery and external access.

## Important configuration

See `.env.example`. The most important production settings are admin passwords, TURN credentials, `COOKIE_SECURE=true` behind HTTPS, viewer limits, and signaling timeouts.

Servo limits and button increment are server-owned: `SERVO_MIN_ANGLE`, `SERVO_MAX_ANGLE`, `SERVO_STEP`, and `SERVO_COMMAND_INTERVAL_MS`. The browser cannot override `SERVO_STEP`.

## PhonePilot v2 network setup

The main HTTP listener binds to `127.0.0.1:8080` by default. `PHONEPILOT_LAN_BIND=auto:8081` opens the same HTTP handler on the private IPv4 address of the interface used by the primary route. If that selects a VPN or the address cannot be identified uniquely, set an explicit interface address such as `PHONEPILOT_LAN_BIND=192.168.1.10:8081`. A failed or ambiguous selection disables LAN with a diagnostic in the log. `PHONEPILOT_LAN_BIND=off` disables it deliberately. Allow TCP 8081 only on the trusted LAN; do not forward it on the router. The LAN listener still serves the admin UI, so only trusted devices should reach that port.

When the LAN listener starts, the server advertises `_phonepilot._tcp.local.` over mDNS with instance `phonepilot-<serverId>` and TXT `v=2` and `serverId=<UUID>`. Permit UDP 5353 multicast on the selected interface. If multicast is blocked, configure the LAN address manually on Android. `GET /api/network/info` returns `{ "v": 2, "serverId": "<UUID>" }` on both listeners. `GET /api/network/lan-probe?nonce=<base64url>` works only on the LAN listener from the interface subnet and echoes the nonce. Android must compare the returned serverId to the saved identity. The UUID is saved at `PHONEPILOT_SERVER_ID_FILE` (default `data/server-id`); preserve this file across upgrades and container restarts.

For access outside the LAN, set `PHONEPILOT_PUBLIC_URL` to the external HTTPS origin and `PHONEPILOT_CAMERA_SECRET_B64URL` to 32 random bytes encoded as unpadded Base64URL. Generate a secret with a cryptographic random generator, transfer it once to the Android app through a private channel, and keep it out of URLs and logs. Public mode refuses to start without the secret. The same challenge/response is required on both camera listeners when the secret is set. The nonce lasts one authentication attempt and the server waits at most five seconds. Without a public URL or secret, cameraId-only authentication is a **local legacy mode** limited to loopback and verified LAN peers.

Terminate TLS at a reverse proxy on the same host and proxy **only** `/ws/camera` and `/api/network/info` to `127.0.0.1:8080`. Do not publish `/ws/admin`, `/ws/robot`, `/api/network/lan-probe`, the web UI, login, or any other route. Preserve WebSocket Upgrade headers. Do not expose port 8080 directly. `PHONEPILOT_MAIN_BIND=0.0.0.0` is an explicit local deployment choice and is rejected in public mode; firewall it if used. Set `COOKIE_SECURE=true` when the browser UI itself is served through HTTPS on a trusted internal route.

Configure reachable `TURN_URLS`, `TURN_USERNAME`, and `TURN_CREDENTIAL` for WebRTC across NAT/CGNAT. Working WSS signaling alone does not guarantee remote video. The displayed path describes the active signaling WebSocket; it does not prove ESP32 or TURN connectivity. `lan_wifi` requires the LAN listener, a peer address within its interface subnet, and Android's `wifi` report. The public listener maps `cellular` to `mobile`, `wifi` to `remote_wifi`, and everything else to `unknown`. A closed camera socket is immediately `offline` even during camera grace.

For Docker, mount `data/` persistently and use host networking or explicit interface binding so the LAN listener and mDNS can reach the real LAN. The public TLS proxy must be able to reach the loopback main listener in its network namespace. Container port mapping alone does not provide this setup.

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

# Migration from the Node.js server

## Reason for the rewrite

The previous server was a single ~825-line `server.js` containing HTTP routes, auth, WebSocket upgrade handling, camera/viewer state, relay failover and servo logic. None of those responsibilities required Node-specific APIs, so Go is a good fit for a long-running signaling service: a single deployable binary, explicit concurrency control and a smaller runtime/dependency footprint.

## Preserved behavior

The Go server intentionally keeps the external protocol compatible with the old v0.7.x server. The existing Android app and copied browser assets can connect without a protocol migration.

Preserved concepts include:

- single Android WebRTC upstream to the elected SOURCE viewer;
- browser relay to additional viewers;
- automatic relay-to-SOURCE promotion;
- camera/viewer signaling grace windows;
- negotiation timeout;
- one-time admin WebSocket token;
- per-viewer servo rate limiting and server-side angle state;
- `/ws/robot` compatibility alias.

## Internal changes

- Express routes -> Go `net/http`.
- `ws` package -> small internal RFC6455 WebSocket transport built on Go standard-library networking; no external Go module is required.
- `express-session` -> random opaque in-memory session IDs in an HttpOnly cookie.
- `.env` loading -> small built-in parser; no dotenv dependency.
- browser assets -> embedded into the Go binary.
- shared mutable signaling state -> guarded by a central hub mutex; each WebSocket has a serialized writer.

## Operational differences

`SESSION_SECRET` is no longer required because the cookie contains only a cryptographically random opaque session ID; session data remains server-side. `COOKIE_SECURE` was added for HTTPS deployments.

As before, restarting the process clears login sessions, camera presence and viewer sessions. Persistent/horizontally-scaled state is a future architectural step, not part of this migration.

# M003 — Outbound + Multi-Extension + Admin REST Minimum

> Specification document for `gsd headless new-milestone --context M003-SPEC.md`. Read `00-VISION.md` first for the multi-milestone context and `M002-SUMMARY.md` (when available) for the AI-loop foundation this builds on. Self-contained — the GSD planner/worker/tester agents do not need additional input to execute M003.

---

## 1. Milestone vision

Extend the single-extension inbound voice bot (M001+M002) to a multi-extension production-grade SIP endpoint with outbound calling and an HTTP admin surface. By the end of M003, `bellerophon`:

1. Registers N SIP extensions concurrently from `devices.json` (multi-registrar pattern, identical schema to `voice-app/config/devices.json.example`).
2. Places outbound calls via `POST /api/outbound-call` (same request/response shape as voice-app).
3. Serves ~25 read-only admin endpoints over HTTP (auth-gated except `/health`), all matching voice-app's JSON shapes byte-for-byte so the existing admin GUI works unchanged when served from the binary's `embed.FS`.
4. Exposes the voice-app static admin SPA from the binary (`voice-app/static/admin/` embedded at build time).
5. Persists per-extension registration state in an embedded sqlite store (no Postgres — that arrives optional in M004).

The hard goal: a user who today runs `voice-app` via Docker can stop the container, run `./bellerophon --config config.yaml`, point the admin GUI at it, and see the same calls/devices/voices/status pages with no visible difference.

## 2. Success criteria (observable, measurable)

1. **Multi-registrar:** with three extensions in `devices.json`, all three appear online in the 3CX admin panel within 10 seconds of `bellerophon` boot.
2. **Outbound call:** `curl -X POST http://localhost:3000/api/outbound-call -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"to":"1002","from":"1001","message":"hello world"}'` returns `200` with `{"callId":"<uuid>"}` and the destination soft-phone rings within 2 seconds.
3. **API contract parity:** for every endpoint in §5.4 must-haves table, the JSON response shape (top-level keys + value types) matches what voice-app returns for the same request. Verified by golden-file diff against captured voice-app responses (fixture at `test/golden/m003/`).
4. **Auth:** `curl http://localhost:3000/api/devices` (no `Authorization` header) returns `401 Unauthorized` with body `{"error":"missing or invalid bearer token"}`. With the right token, returns `200`. `GET /health` and `GET /api/health/db` are exempt from auth.
5. **Admin GUI loads:** opening `http://localhost:3000/admin/` in a browser renders the voice-app SPA with no 404s for assets and no JS console errors (DOM ready + at least one `page:show` event fires).
6. **Reload:** after editing `devices.json` (add or remove an extension) and `curl -X POST /api/extensions/reload` with auth, `GET /api/extensions/health` reflects the change within 5 seconds without restarting the binary.
7. **Graceful shutdown:** SIGINT unregisters every active extension (sends REGISTER `Expires:0` to each), waits up to 5 s for outbound calls in flight to BYE, then exits. 3CX shows all extensions offline within 10 s.
8. **No CGO:** `CGO_ENABLED=0 go build` still succeeds (sqlite via `modernc.org/sqlite` is pure-Go).
9. **Coverage:** ≥ 80 % on `internal/registrar`, `internal/admin`, `internal/admin/handlers`, `internal/store`.
10. **Binary size growth:** stripped `linux/arm64` binary stays ≤ 40 MB despite the embedded SPA (+sqlite + gorilla/mux). Previous baseline from M001: ≤ 30 MB.

## 3. Out of scope for M003

- Sales agent + outbound campaigns. (M004.)
- Batch dialer + AMD detection. (M004.)
- Call recording (encrypted or not). (M004.)
- OpenAI Realtime bridge + LiveKit token mint + browser test-call. (M005.)
- 3CX XAPI extension provisioner. (M005.)
- Bark TTS. (M005.)
- Postgres support. (M004 — sqlite first.)
- IM integrations / Telegram / WhatsApp. (M005 nice-to-have, otherwise dropped.)
- Hold music, transfer, conference. (Not in voice-app either.)
- Per-device call recording delivery to Telegram. (M004.)
- HTTPS on the admin port. (M006.)

## 4. Reference material — read before planning

| Path | Why |
|------|-----|
| `voice-app/lib/multi-registrar.js` (225 LOC) | Today's multi-registrar. Source of truth for "how 3CX expects N parallel REGISTERs from the same client process". |
| `voice-app/lib/device-registry.js` (~170 LOC) | Device lookup + diacritic-insensitive name normalization (see `_normalizeName` — replicate in Go). |
| `voice-app/config/devices.json.example` | Schema. **Do not change it** — Go config must accept it byte-compatibly. |
| `voice-app/lib/outbound-handler.js` (369 LOC) | Outbound call initiation; SIP UAC dance with 3CX. |
| `voice-app/lib/outbound-session.js` (324 LOC) | Per-call lifecycle (state machine, hangup, cleanup). |
| `voice-app/lib/outbound-routes.js` (418 LOC) | REST contract for `POST /api/outbound-call` + variants. Request/response shape is binding. |
| `voice-app/lib/http-server.js` (297 LOC) | Express bootstrap, middleware order, body-parser placement (webhooks BEFORE json), CORS, auth pattern. |
| `voice-app/lib/admin-routes.js` (415 LOC) | Admin endpoints. Read first before designing handler signatures. |
| `voice-app/lib/admin-status.js` (679 LOC) | Status/health/diagnostics endpoints. Largest file in this milestone's reference set — scout-summarize don't fully port. |
| `voice-app/lib/query-routes.js` (484 LOC) | Read-only query endpoints. Subset relevant to M003. |
| `voice-app/static/admin/` | Vanilla HTML/JS SPA. Embedded as-is via `embed.FS`. Do not edit. |
| `voice-app/index.js` (top 100 lines) | Wiring order: how voice-app instantiates multi-registrar, claude bridge, audio fork, then HTTP. Useful as scaffold for our `cmd/bellerophon/main.go` wiring. |
| `CLAUDE-PHONE-INTEGRATION-SPEC.md` (/root/) | Authoritative API contract description (URL paths, request shapes, response shapes). Cite section numbers in `M003-CONTEXT.md`. |
| `https://github.com/gorilla/mux` | Router we standardize on. |
| `https://pkg.go.dev/modernc.org/sqlite` | Pure-Go sqlite driver (no CGO). |
| `RFC 3261` | SIP, specifically the UAC sections for outbound INVITE handling. Re-read §13 (Initiating a Session) and §15 (Terminating a Session). |

## 5. Slices

Planner: convert each slice into a `S0X-PLAN.md` with the listed must-haves and tasks. Boundary map at end of this section.

### S01 — Multi-extension registrar
**Goal:** Parse `devices.json`, register every extension concurrently to 3CX, expose per-extension status via Go API.
**Demo:** Start `bellerophon` with three extensions configured; 3CX admin shows all three online; `bellerophon` logs one `[sip] registered` line per extension within 10 s.

Must-haves:
- `internal/registrar/Registry` — loads `devices.json` (path from config, default `./devices.json` matching voice-app convention); exposes `GetByExtension(ext) *Device`, `GetByName(name) *Device` (case+diacritic-insensitive), `All() []*Device`.
- `internal/registrar/MultiRegistrar` — wraps M001 `sipua.Server`; calls `Register(...)` for each device in parallel; tracks state `{registered, registering, failed, unregistered}` per device.
- `Reload(devices.json)` — diffs old vs new device set; unregisters removed; registers added; leaves unchanged ones alone.
- Auth failure on one extension does NOT abort the others (resilient to partial failure).
- Logs at INFO per state transition; ERROR with cause on auth/network failure.
- Tests: 3-device golden config, fake SIP server (table-driven for 401/403/500/timeout responses).

Tasks:
- T01 `Device` struct + `devices.json` loader (must accept voice-app's exact schema including `salesMode`, `batchEnabled`, `voiceId`, `voiceProvider` — store-but-don't-use for now)
- T02 `Registry` API + diacritic normalization (port `_normalizeName` from voice-app)
- T03 `MultiRegistrar` parallel register + per-device status map
- T04 `Reload` diff logic + tests
- T05 Resilient error handling (one failure ≠ all failure)

### S02 — Outbound call dialer (sipua extension)
**Goal:** Place outbound INVITEs from a chosen extension. Handle 100/180/200/ACK/BYE on the UAC side.
**Demo:** `bellerophon --outbound 1002@sip.example.com --from 1001 --play examples/ready-beep.wav` places a call, plays the WAV when the callee answers, sends BYE after playback ends.

Must-haves:
- Extend `internal/sipua/Server` with `Dial(ctx, opts DialOptions) (*Call, error)` where `DialOptions{From, To, SDP, Headers}`.
- Returns a `Call` matching M001's inbound `Call` shape (same `OnBye`, `Reply` for re-INVITE later).
- Handles 100 (trying), 180 (ringing), 200 OK, ACK. Handles 4xx/5xx with structured error (`DialError{Code, Reason}`).
- Handles BYE from the remote side; calls registered `OnBye` callback.
- Outbound RTP wiring: reuses M001 `rtp.Session` + S05 `media.Playback` if a WAV is supplied at dial time.
- Cancellation via `ctx.Done()` sends CANCEL if mid-ring, BYE if connected.
- Logs every state transition at INFO.

Tasks:
- T01 `DialOptions` + `Dial(ctx, opts)` skeleton (no media yet)
- T02 100/180/200/ACK handling
- T03 4xx/5xx → structured `DialError`
- T04 CANCEL on ctx-mid-ring + BYE on ctx-mid-call
- T05 Wire RTP send path for `opts.PlayWAV` (uses M001 S05 `Playback`)
- T06 Tests: synthetic SIP server returns scripted responses

### S03 — HTTP admin server skeleton + auth + static SPA
**Goal:** Bootstrap an HTTP server with the middleware chain, bearer-token auth, and the voice-app static SPA served from `embed.FS`. No business endpoints yet (those land in S04/S05).
**Demo:** `curl http://localhost:3000/health` → `200 OK`. `curl http://localhost:3000/api/devices` → `401`. `curl -H "Authorization: Bearer $ADMIN_TOKEN" /api/devices` → `404` (handler not yet wired). Browser at `http://localhost:3000/admin/` renders the voice-app GUI shell.

Must-haves:
- `internal/admin/Server` — wraps `gorilla/mux.Router`; standardized JSON error responses (`{"error":"…","code":"…"}`).
- Middleware chain (outer-to-inner): request ID, structured slog logger, recover-panic-to-500, CORS (configurable origins, default `*` in dev / explicit list in prod via `ADMIN_CORS_ORIGINS` env), auth, rate-limit (basic token bucket per IP).
- Auth: `Authorization: Bearer <token>` matched against `ADMIN_API_TOKEN` env (matches voice-app's `IM_API_TOKEN` convention; documented in `M003-CONTEXT.md` after scout pass confirms voice-app's exact pattern). `/health`, `/api/health/db`, `/admin/*` (static) are exempt.
- Static SPA: `voice-app/static/admin/` embedded via `//go:embed all:static/admin` at build time, served at `/admin/*`. Path resolution must handle the SPA's hash-routing (serve `index.html` for any unknown `/admin/*` path).
- `/health` returns `{"status":"ok","version":"<sha>","uptime_seconds":<n>}` — no auth required.
- HTTPS: out of scope (defer to M006). HTTP-only for now.

Tasks:
- T01 `Server` + `gorilla/mux` wiring + JSON error helpers
- T02 Middleware: request-id, slog logger, panic recovery, CORS
- T03 Bearer auth middleware + `/health` exempt rule
- T04 Rate-limiter (token bucket, 100 req/min per IP default)
- T05 `embed.FS` of voice-app/static/admin + SPA fallback to `index.html`
- T06 Tests: 401 without token, 200 with, CORS preflight, panic→500, /health no-auth

### S04 — Read-only admin endpoints (~25)
**Goal:** Implement the read-only half of the voice-app HTTP API. Every response matches voice-app's JSON shape byte-for-byte.
**Demo:** Browser admin GUI's Devices, Extensions, Calls, Voices, Status pages all populate with data; no XHR returns 4xx/5xx; admin GUI's `cmd-k` palette finds devices by name.

Endpoints (exact paths from voice-app — see §4 catalog):

| Method | Path | Source data |
|--------|------|-------------|
| GET | `/health` | self (no auth) |
| GET | `/api/health/db` | sqlite ping (no auth) |
| GET | `/api/status` | runtime stats: uptime, goroutines, active calls, registered extensions |
| GET | `/api/devices` | `Registry.All()` |
| GET | `/api/device/:identifier` | `Registry.GetByExtension or GetByName` |
| GET | `/api/extensions` | live registration status (per-device) |
| GET | `/api/extensions/health` | summary: total / online / offline / failing |
| GET | `/api/calls` | in-flight call list (from sipua) |
| GET | `/api/call/:callId` | single call detail + transcript |
| GET | `/api/voices` | placeholder until M005 ElevenLabs fetch — returns `[]` + `{"provider":"elevenlabs","note":"populated in M005"}` |
| GET | `/api/stt-provider` | config: `{"provider":"whisper","model":"whisper-1",...}` |
| GET | `/api/tts-providers` | config: list of configured providers |
| GET | `/api/vad` | VAD config (echoes M002 vad section) |
| GET | `/api/wake-word` | placeholder `{}` — not in scope |
| GET | `/api/backend` | LLM backend config: `{"provider":"anthropic","model":"…"}` |
| GET | `/api/languages` | hardcoded `["en","it"]` to match voice-app voice-strings supported list |
| GET | `/api/config` | full merged config (with secrets masked: api_key/auth_password fields → `"***"`) |
| GET | `/api/audio` | list `*.wav`/`*.mp3` files in audio dir (defaults to `./audio`) |
| GET | `/api/audio/download/:filename` | stream the file (with content-type sniffing) |
| GET | `/api/prompts` | placeholder until M004 sales agent — returns `{}` |
| GET | `/api/tools` | placeholder until M004 sales agent — returns `[]` |
| GET | `/api/scripts` | placeholder until M004 sales agent — returns `[]` |
| GET | `/api/api-keys` | configured api-keys list with values masked |
| GET | `/` | redirects to `/admin/` |
| GET | `/admin/*` | static SPA (covered by S03) |

Must-haves:
- Each handler in `internal/admin/handlers/<area>.go` (one file per concern: `devices.go`, `calls.go`, `extensions.go`, `health.go`, `config.go`, `audio.go`, etc.)
- Response shape compatibility verified via golden-file diff against captured voice-app responses (collect fixtures in `test/golden/m003/` — one file per endpoint).
- Secret-masking helper used consistently: `internal/admin/mask.go` walks the config and replaces `api_key|password|auth_password|secret|token` fields with `"***"`.
- Placeholder endpoints (`/api/prompts`, `/api/tools`, `/api/scripts`, `/api/wake-word`, `/api/voices`) are explicitly documented in the handler comments as "stub until M004/M005".

Tasks:
- T01 health + status (foundation; smallest endpoints)
- T02 devices + extensions handlers (read from `internal/registrar`)
- T03 calls handler (read from `internal/sipua`)
- T04 config + api-keys + masking (read from `internal/config`)
- T05 audio listing + download
- T06 stt/tts/vad/backend/languages/wake-word/prompts/tools/scripts (mostly trivial readers)
- T07 Golden-file fixture capture + diff tests

### S05 — Outbound REST endpoints (write)
**Goal:** Wire the outbound-call REST API to the S02 dialer. Add reload and hangup endpoints.
**Demo:** `curl POST /api/outbound-call` places a real call; `curl POST /api/call/:callId/hangup` terminates it; `curl POST /api/extensions/reload` after editing `devices.json` picks up the change.

Endpoints:

| Method | Path | Body | Auth |
|--------|------|------|------|
| POST | `/api/outbound-call` | `{to, from, message?, voice_id?, prompt?}` | required |
| POST | `/api/call/:callId/hangup` | `{}` | required |
| POST | `/api/extensions/reload` | `{}` | required |
| POST | `/api/query` | `{device, prompt}` (matches voice-app `query-routes`) | required |

Must-haves:
- Request validation: missing/invalid fields → `400 {"error":"…","field":"…"}`. Reject obviously bad `to` (not a SIP URI or extension number).
- `POST /api/outbound-call`: picks `from` extension via `registrar.Registry`, calls `sipua.Dial`, returns immediately with `{callId, status:"dialing"}` (async — call lifecycle goes through M002's conversation loop if `prompt` or `message` provided).
- `POST /api/call/:callId/hangup`: looks up call by UUID, sends BYE, removes from active call map. Returns `{"callId":"…","status":"hung_up"}` or `404` if unknown.
- `POST /api/extensions/reload`: re-reads `devices.json`, calls `MultiRegistrar.Reload(...)`, returns `{added:[…], removed:[…], unchanged:[…]}` summary.
- `POST /api/query`: programmatic single-turn query against a configured device (reuses M002 conversation loop in single-turn mode). Body matches `voice-app/API-QUERY-CONTRACT.md`.
- All POSTs accept `application/json` only. Reject other content-types with `415`.

Tasks:
- T01 outbound-call handler + validation
- T02 hangup handler
- T03 extensions/reload handler
- T04 query handler (reuses M002 conversation-loop in one-shot mode)
- T05 Request-validation helper + tests
- T06 Integration tests: POST → real SIP signalling against a `sipp`-driven UAS

### S06 — sqlite store for state + devices schema
**Goal:** Embedded sqlite for fast device + call lookup. devices.json remains the source of truth for *configuration*; sqlite caches *state* (registration status, call history).
**Demo:** After 10 inbound calls + 5 outbound, `GET /api/calls` returns the full 15-row history with `started_at`, `ended_at`, `duration_ms`. Restart binary → history persists.

Must-haves:
- `internal/store/sqlite.go` — opens DB at `./bellerophon.db` (path from `STORE_PATH` env, default `./bellerophon.db`); creates schema if missing.
- Schema: `calls(uuid, direction, from_ext, to_uri, started_at, ended_at, status, transcript_path)`, `extensions_state(extension, last_registered_at, last_error, status)`.
- Migration: on boot, compare schema_version row, apply migrations in order (start at v1 — no prior versions).
- Write paths: `sipua` and `registrar` get injected with a `Store` interface; they call `Store.RecordCall(...)` and `Store.UpdateExtensionState(...)` async (non-blocking).
- Read paths: admin handlers `/api/calls`, `/api/call/:callId`, `/api/extensions/health` query the store.
- Driver: `modernc.org/sqlite` (pure-Go).
- Backups: out of scope (M006 nice-to-have).

Tasks:
- T01 sqlite driver wiring + schema migration runner
- T02 `Store` interface + sqlite impl (CRUD on calls + extensions_state)
- T03 Hook sipua + registrar to write async to store
- T04 Migrate admin handlers from in-memory to store-backed reads
- T05 Tests: in-memory sqlite for unit, real file for integration

### S07 — Integration test + live 3CX UAT
**Goal:** End-to-end verification against the live 3CX instance currently used by `voice-app`. Same gate pattern as M001 S07.
**Demo:** Three extensions registered; soft-phone places inbound call to ext A (M002 conversation loop runs); REST API places outbound call from ext B to a soft-phone; admin GUI shows both calls live in `/api/calls`; both calls hang up cleanly.

Must-haves:
- `docs/m003-uat.md` — step-by-step manual UAT covering all 10 success criteria from §2.
- `test/integration/multi3cx_test.go` — gated by `BELLEROPHON_LIVE_3CX=1`. Registers 3 extensions, places outbound call via REST, asserts SIP signalling correct via pcap dump.
- Golden-file fixture refresh: re-capture voice-app responses for the §5.4 endpoint table; diff against bellerophon responses; document drift in `M003-SUMMARY.md`.
- Run on Raspberry Pi 5 (Stefan has one) → `docs/m003-pi-benchmark.md` (boot, register times, RAM, concurrent-call ceiling).
- Migration notes: any voice-app behavior intentionally not replicated → `M003-SUMMARY.md`.

Tasks:
- T01 Manual UAT script
- T02 Live 3CX integration test
- T03 Golden-file capture + diff
- T04 Pi 5 hardware run + benchmark
- T05 Document quirks + close milestone

## 6. Boundary Map

```
S01 → S02
Produces:
  internal/registrar/registry.go   → Registry, Device, GetByExtension, GetByName, All
  internal/registrar/multi.go      → MultiRegistrar, Register, Reload, Status

Consumes from M001:
  internal/sipua.Server (Register, OnInvite from S03 of M001)
  internal/config.SIP (per-device auth fields)

S02 → S03
Produces:
  internal/sipua/dialer.go         → Server.Dial(ctx, DialOptions) (*Call, error)
                                       + DialError{Code, Reason}

Consumes from S01:
  registrar.Registry (to resolve "from" extension → auth credentials)

S03 → S04
Produces:
  internal/admin/server.go         → Server, Start(ctx) error, Stop(ctx) error
  internal/admin/middleware.go     → request-id, logger, recover, CORS, auth, rate-limit
  internal/admin/static.go         → embed.FS handler for /admin/*
  internal/admin/errors.go         → JSON error writers (400/401/403/404/500)

Consumes from S02:
  none directly (S03 ships an empty handler tree)

S04 → S05
Produces:
  internal/admin/handlers/devices.go    → GET /api/devices, /api/device/:identifier
  internal/admin/handlers/calls.go      → GET /api/calls, /api/call/:callId
  internal/admin/handlers/extensions.go → GET /api/extensions, /api/extensions/health
  internal/admin/handlers/health.go     → GET /health, /api/health/db, /api/status
  internal/admin/handlers/config.go     → GET /api/config + masking
  internal/admin/handlers/audio.go      → GET /api/audio, /api/audio/download/:filename
  internal/admin/handlers/misc.go       → /api/{stt-provider,tts-providers,vad,wake-word,backend,languages,prompts,tools,scripts,api-keys,voices}
  internal/admin/mask.go                → secret-masking helper

Consumes from S03:
  admin.Server (Mount("/api/...", handler))

S05 → S06
Produces:
  internal/admin/handlers/outbound.go    → POST /api/outbound-call
  internal/admin/handlers/hangup.go      → POST /api/call/:callId/hangup
  internal/admin/handlers/reload.go      → POST /api/extensions/reload
  internal/admin/handlers/query.go       → POST /api/query

Consumes from S02:
  sipua.Server.Dial
Consumes from S01:
  registrar.MultiRegistrar.Reload

S06 → S07
Produces:
  internal/store/sqlite.go               → Open, Migrate
  internal/store/store.go                → Store interface (RecordCall, UpdateExtensionState, etc.)
  internal/store/migrations/v1.sql       → initial schema

Consumes:
  none (slot-in via dependency injection into sipua + registrar)

S07 → (M004)
Produces:
  docs/m003-uat.md, docs/m003-pi-benchmark.md
  M003-SUMMARY.md (quirks, decisions, benchmarks)
  test/integration/multi3cx_test.go (template reused in M004 batch tests)
  test/golden/m003/ (response fixtures for regression)

Consumes from S01-S06: everything.
```

## 7. Risks specific to M003

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| `devices.json` schema has undocumented fields voice-app accepts but we drop | Medium | Medium | Scout pass on `voice-app/lib/device-registry.js` + `extension-provisioner/provisioner.js` in S01; round-trip test: load → marshal → load → assert deep equal. |
| 3CX rejects N parallel REGISTERs from the same Contact host (sees them as duplicates) | Medium | High | Use unique `;rinstance=<random>` Contact param per extension (voice-app does this — verify in scout). Live 3CX test in S07 catches this. |
| gorilla/mux retired/archived during milestone | Low | Low | Pin to v1.8.x in `go.mod`. If upstream archives mid-milestone, fork — it's small. |
| voice-app uses cookie auth or session-based auth (not bearer) for some endpoints | Medium | Medium | Scout `voice-app/lib/http-server.js` before writing S03 T03; document in `M003-CONTEXT.md`. If cookie auth needed, add session middleware after S03 close. |
| Embedded SPA outgrows embed.FS reasonable limits (10 MB+) | Low | Low | Measure at S03; if > 5 MB, gzip-compress at build time and decompress in middleware. |
| sqlite WAL mode + multi-extension concurrent writes cause "database is locked" | Medium | Medium | Open with `?_journal=WAL&_busy_timeout=5000` connection params; restrict writes to a single dedicated goroutine (channel-fan-in). |
| Outbound call from extension X uses extension Y's credentials due to registry race | Low | High | `registrar.Registry` reads are copy-on-snapshot; `Reload` swaps the whole map atomically (pointer swap). |
| Pi 5 ARM hits sqlite I/O bottleneck at > 5 concurrent calls | Low | Medium | Bench in S07; if bottleneck, move calls history to in-memory ring buffer + periodic flush. |

## 8. Hand-off checklist (when M003 closes)

Before tagging M003 complete and starting M004:

- [ ] All 10 success criteria from §2 hit.
- [ ] CI green on `linux/{amd64,arm64}`, `darwin/{amd64,arm64}`.
- [ ] `golangci-lint run` exit 0.
- [ ] Coverage: ≥ 80 % on `internal/registrar`, `internal/admin`, `internal/admin/handlers`, `internal/store`.
- [ ] Golden-file diff against captured voice-app responses passes (drift documented if any).
- [ ] `M003-SUMMARY.md` lists 3CX quirks + decisions + benchmarks (real Pi 5 numbers).
- [ ] `docs/m003-uat.md` + `docs/m003-pi-benchmark.md` shipped.
- [ ] Stefan signed off on the manual UAT.
- [ ] No `TODO M003` markers left in code.
- [ ] Linter clean.
- [ ] Draft PR open against main with `M003-SUMMARY.md` as the body.

## 9. Notes for the GSD planner agent

- **Iron rule:** each task fits one context window. If S04 T03 (calls handler) looks too big because it pulls in transcript serialization, split into T03a (active calls only, no transcript) + T03b (single-call detail with transcript).
- **Boundary map is binding.** Slices export only what's listed. If you need to add an export, document in `S0X-CONTEXT.md` and update the boundary map.
- **No premature abstraction.** The `Store` interface in S06 has exactly the methods sipua + registrar call. Don't add `Find...By...` methods speculatively for M004.
- **Reuse M001/M002 infrastructure.** sipua.Server already exists; you're adding `.Dial()` to it, not creating a new package. Admin handlers query `registrar.Registry` and `sipua.Server` directly — no service layer.
- **Reference voice-app behavior, don't blindly copy.** voice-app has Express middleware order that matters for webhooks (must be BEFORE `express.json()` — see `http-server.js`). We don't have webhooks in M003 so that constraint doesn't apply yet, but note it for M005 where IM channels would re-introduce it.
- **Static SPA: serve don't modify.** `voice-app/static/admin/` is the GUI for the next 6 months. If S04 endpoints don't match the shape the SPA expects, fix the endpoint, not the SPA.
- **Pure-Go is binding.** `modernc.org/sqlite` not `mattn/go-sqlite3` (the latter requires CGO).
- **Test fixtures are first-class.** The golden-file diff against voice-app is the contract test. If it drifts, that's the bug — investigate before adjusting the golden file.
- **Auth scheme: verify before assuming.** S03 T03 starts with a 10-minute scout pass on `voice-app/lib/http-server.js` to confirm bearer-token is actually what voice-app uses (vs cookie/session). Adjust the SPEC if scout finds different — log the decision in `DECISIONS.md`.

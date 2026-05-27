# Vision — Bellerophon Voice Stack: Go Rewrite

> Strategic vision document. Read this before any milestone in `/root/bellerophon/.gsd/proposals/go-rewrite/`. Not directly consumed by `gsd headless new-milestone` — use it as `M00x-CONTEXT.md` content or pass via `--context-text` when bootstrapping multi-milestone planning.

---

## 1. Goal in one paragraph

Replace the current Node.js + drachtio + FreeSWITCH + Docker voice stack with a single statically-linked Go binary (`bellerophon`) that registers to **any RFC 3261 SIP registrar / trunk** (MessageNet, generic SIP provider, self-hosted Asterisk/FreeSWITCH, 3CX, etc.), handles inbound/outbound SIP calls end-to-end, drives the AI conversation loop (STT → LLM → TTS), and exposes the same HTTP/WS admin surface as today's `voice-app`. A tutorial viewer must be able to run `./bellerophon --config config.yaml` on macOS/Linux/Raspberry Pi without installing Docker, Node, Python, FreeSWITCH, **or any specific PBX** — point it at a SIP trunk (or a free PBX of the user's choice) and it works. 3CX is one supported provider among several, not the design target. Feature parity with the current `voice-app` (~13k LOC, ~70 HTTP endpoints, 4 STT providers, 2 TTS providers, multi-extension, outbound + sales + batch + recording + OpenAI Realtime bridge + the SIP-providers pluggable layer + optional 3CX XAPI extension provisioner) is non-negotiable — this is a rewrite, not an MVP.

## 2. Why now

- Docker-on-Mac networking issues block first-run experience for tutorial viewers (`docker-compose.yml` requires `network_mode: host` + `EXTERNAL_IP` gymnastics that fail half the time on macOS).
- Node + native modules (`better-sqlite3`, `drachtio-fsmrf`) make the Pi/ARM story painful (separate native rebuild step — see `~/.local/bin/rebuild-gsd-native.sh` for the GSD parallel).
- FreeSWITCH is a 200 MB dependency for what is effectively "transcode G.711 ↔ PCM16k + play a WAV + capture mic". A pure-Go stack can do this in <30 MB.
- **PBX-lock-in is a tutorial blocker.** Today the project reads as "3CX-only" even though the codebase already has a `sip-providers/{3cx,messagenet,generic}.js` pluggable layer. A viewer with a MessageNet DID or a self-hosted Asterisk box should not have to install 3CX to follow along. The rewrite is the opportunity to elevate this provider-agnostic layer in the docs and binary.
- `sipgo` hit v1.0.0 (2025); `pion/rtp` is production-proven; the Go SIP+RTP ecosystem is finally usable without CGO.

## 3. Non-goals

- **Not a port of FreeSWITCH features we don't use.** We use: REGISTER, INVITE/200/ACK, RTP send/recv, G.711µ/A ↔ PCM16k transcode, WAV playback, mic capture, DTMF RFC 2833 detect, RTCP heartbeat, recording. Everything else FreeSWITCH does (conferencing, MoH server, IVR engine, mod_python, dialplan XML) is out of scope.
- **No new features.** Parity = same endpoints, same behavior, same env vars (where they still make sense). New features live in post-rewrite milestones.
- **No GUI rewrite.** The existing admin GUI (`voice-app/static/`) is plain HTML/JS — we serve it as-is from the Go binary's embed.FS. Visual changes are out of scope.
- **No protocol additions.** No WebRTC-native (the OpenAI Realtime bridge stays a server-side WS proxy). No SIP over WebSocket. No TLS-SIP (TCP/UDP only, matching today).

## 4. Architecture target

```
┌─────────────────────────────────────────────────────────────────┐
│ bellerophon (single Go binary, ~25 MB stripped)                │
│ ├── sipua/         sipgo-based SIP UA: REGISTER, INVITE, BYE   │
│ ├── sipprov/       pluggable SIP provider layer:               │
│ │                    generic (RFC 3261 default), messagenet,   │
│ │                    3cx, asterisk, freeswitch-self-hosted     │
│ │                    — DID parsing, registrar quirks, trunk    │
│ │                    auth, From-rewrite per provider           │
│ ├── rtp/           pion/rtp + custom jitter buffer + DTMF      │
│ ├── codec/         G.711µ/A ↔ PCM16k (pure Go)                 │
│ ├── media/         playback (WAV/MP3), capture, mix, record    │
│ ├── vad/           silence detection (RMS + hangover)          │
│ ├── stt/           providers: whisper, gemini, gcloud, openai  │
│ ├── tts/           providers: elevenlabs, bark (HTTP client)   │
│ ├── llm/           anthropic SDK (default) + http claude-api    │
│ ├── conversation/  state machine, barge-in, transcript turns   │
│ ├── outbound/      single + sales-agent + batch dialer         │
│ ├── admin/         ~70 HTTP endpoints + embed.FS static GUI    │
│ │                    (includes /api/sip-trunks for the         │
│ │                     provider-agnostic trunk-creds form)      │
│ ├── realtime/      OpenAI Realtime WS bridge + LiveKit tokens  │
│ ├── provisioner/   OPTIONAL: 3CX XAPI client                   │
│ │                  (loaded only when provider=3cx — bonus)     │
│ ├── store/         sqlite (modernc.org/sqlite, pure-Go, embed) │
│ │                  + optional Postgres (lib/pq) for prod       │
│ └── config/        YAML + env var override                     │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼ optional HTTP (when CLAUDE_API_URL set)
                ┌─────────────────────────────────────┐
                │ claude-api-server (Node, unchanged) │
                │ for codex/opencode/ollama/agentic   │
                └─────────────────────────────────────┘
```

Single binary. No CGO (verified: `sipgo`, `pion/*`, `modernc.org/sqlite`, `lib/pq` all pure-Go). Cross-compile to `linux/amd64`, `linux/arm64`, `darwin/arm64`, `darwin/amd64`, `windows/amd64`.

## 5. Roadmap (milestones)

| ID | Title | Slices | Demoable outcome |
|----|-------|--------|------------------|
| **M001** | SIP + media foundation | 6-7 | Binary registers to a SIP registrar (validated against MessageNet **and** 3CX), accepts INVITE, echoes RTP back, plays a WAV. No AI yet. |
| **M002** | Inbound AI conversation loop | 5-6 | A real inbound call gets Whisper-transcribed, prompts Claude, speaks ElevenLabs reply, supports barge-in. |
| **M003** | Outbound + multi-extension + admin REST minimum | 6-7 | Multi-registrar works across providers; `POST /outbound-call` places a call; ~30 read-only admin endpoints + auth equal to voice-app. SIP-trunks admin form lets the user paste credentials for any provider. |
| **M004** | Sales-agent + batch dialer + recording | 6-7 | Sales conversation stages, encrypted recording, batch campaigns with concurrency + AMD detection. |
| **M005** | OpenAI Realtime bridge + optional 3CX provisioner + Bark TTS | 4-5 | Browser ↔ Realtime works; **optional** 3CX-specific extension provisioning via XAPI (loaded only for 3CX users); Bark TTS via HTTP service. |
| **M006** | Production hardening + release pipeline | 4-5 | Cross-compiled binaries on GitHub Releases; smoke-test matrix on Pi/macOS/Linux; migration guide from Node stack; provider compatibility matrix doc. |

Each milestone is sized so a focused 1-2 week sprint can ship it. Total: ~2-3 months of focused work.

## 6. Non-functional targets (measure on every milestone)

| Metric | Target | How to measure |
|--------|--------|----------------|
| End-to-end audio→TTS-start latency (P95) | ≤ 2.5 s | `voice-app/test/` already has timing harness — port it. |
| Concurrent calls on a Raspberry Pi 5 (8 GB) | ≥ 8 | Load-test slice in M006. |
| Binary size (stripped, linux/arm64) | ≤ 30 MB | `make release` step. |
| Cold-start to "ready for INVITE" | ≤ 500 ms | Boot benchmark. |
| Audio-fork → STT pipeline overhead vs current Node | ≤ same | Side-by-side test in M002. |
| RTP packet loss tolerance | ≤ 5 % before audible glitch | Synthetic loss test in M001 S04. |

## 7. Hard constraints (do not break)

- **Provider compatibility:** the SIP UA is **provider-agnostic** — it speaks RFC 3261 + G.711 and must work against any standards-compliant registrar / trunk. Three providers are *integration-tested* at every milestone: **MessageNet** (the user's actual ITSP trunk for inbound DIDs), a **generic SIP registrar** (Asterisk in CI), and **3CX** (legacy compatibility — Stefan's existing deployment). SDP must include `a=ptime:20` and offer `PCMU,PCMA` (in that order), which is the intersection of what all three accept.
- **DID routing:** inbound INVITE → DID extracted from the `To:` URI (and the provider-specific quirks: MessageNet sends DIDs without `+39` country code, 3CX sometimes includes it) → routed to the matching device per `devices.json`. The `sipprov` layer is responsible for these per-provider quirks; the conversation loop never sees them.
- **Trunk credentials UI:** the existing admin SIP-trunks form (`voice-app/static/admin/sip-trunks.{js,css}` + backing `/api/sip-trunks` endpoints + `sip_trunks` SQLite table) is the source of truth for runtime trunk config. Users add a MessageNet/generic/3CX trunk through the UI without editing files. Carry this through unchanged.
- **Env var compatibility:** every env var listed in `voice-app/` source (run `grep -hoE "process\.env\.[A-Z_]+" voice-app/lib/**/*.js | sort -u` — there are ~60+) must either be honored verbatim or have a documented mapping in `M00x-CONTEXT.md`. No silent breakage.
- **`devices.json` schema compatibility:** the existing `voice-app/config/devices.json` must load unchanged. The schema is the source of truth for multi-extension config.
- **HTTP API contract compatibility:** every endpoint in the route catalog (Section 8 of `M001-SPEC.md`) keeps its path, method, request body shape, and response shape — explicitly including `/api/sip-trunks*` (the provider-agnostic trunk CRUD) and `/api/extensions/3cx-credentials` (3CX-specific, kept for back-compat). The admin GUI is unchanged — if it breaks against the Go binary, the Go binary is wrong.
- **3CX provisioner is OPTIONAL.** It loads only when a provider of type `3cx` is configured. Users on MessageNet, Asterisk, or any generic trunk never see it and the binary works without 3CX XAPI credentials.
- **No license regression:** all new dependencies must be MIT/BSD/Apache-2.0. Reject GPL/AGPL pulls. Verify with `go-licenses` in CI.

## 8. Decisions already made (do not re-litigate)

- **Scope:** Full feature parity with `voice-app`. Reasoned away "MVP first" because the tutorial value of "this is what you have today, but simpler to install" requires parity — a degraded MVP is a worse product.
- **Media stack:** Custom in Go (`sipgo` for signaling + `pion/rtp` for transport + own codec/jitter/DTMF/playback/record modules). Rejected baresip/PJSIP-via-CGO because it kills the single-binary cross-compile story.
- **LLM backend:** Hybrid. Default = direct Anthropic SDK call (zero deps for the viewer). Optional = `CLAUDE_API_URL` env points to existing `claude-api-server` for codex/opencode/ollama/agentic Claude Code. Both paths must work in M002.
- **DB:** sqlite via `modernc.org/sqlite` (pure-Go) for embedded use; `lib/pq` for optional Postgres in prod (matches today's `DATABASE_URL` env).
- **Config:** YAML primary, env vars override YAML, CLI flags override env. Identical to `viper` semantics but we can do it without viper to keep deps minimal.
- **Static GUI:** served from `embed.FS` — at build time, `voice-app/static/` is copied into the Go binary. Single source of truth stays in `voice-app/static/` until M006, then we fork.

## 9. Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Pure-Go media stack misses an RTP edge case (CN, comfort noise, marker bit) and 3CX rejects | Medium | High | M001 S04 includes synthetic RTP fuzz + live 3CX integration test before unlocking M002. |
| DTMF RFC 2833 detection too lossy | Medium | Medium | Cross-test against drachtio's DTMF capture on the same call; reject slice if accuracy < 99 %. |
| Jitter buffer tuning for high-loss networks | Medium | Medium | Default 60 ms adaptive, configurable via env; defer adaptive sophistication to M006. |
| sipgo bugs we discover late | Low | High | Pin to v1.0.0 + maintain a small patch fork. Track upstream issues. |
| Anthropic SDK Go ergonomics differ from Node usage | Low | Low | The LLM call is a thin wrapper — `llm.Query(prompt, sessionID) → (response, sessionID, error)` interface is identical to today's `claudeBridge.query()`. |
| Pi 5 ARM cross-compile audio jitter | Medium | Medium | M001 S04 runs on Pi hardware in CI; reject build if jitter test fails on `linux/arm64`. |

## 10. Definition of done (for the whole vision)

- Tutorial viewer downloads `bellerophon-${OS}-${ARCH}` from GitHub Releases, writes `config.yaml` (**or** opens the admin UI and pastes their SIP trunk credentials into the SIP-trunks form), runs the binary, places/receives a call against **a SIP trunk of their choice** (MessageNet, a generic ITSP, self-hosted Asterisk/FreeSWITCH, or 3CX), and has a conversation with Claude in <5 minutes from `curl -LO`. No specific PBX required.
- All `voice-app/test/` integration tests pass against the Go binary unchanged (HTTP fixtures + WS fixtures + `test/sip-providers.test.js`).
- Compatibility matrix in `docs/migration-go.md` documents at least three tested provider configurations (MessageNet trunk, generic SIP registrar, 3CX) with sample `config.yaml` snippets for each.
- Current `voice-app` is retired: removed from `docker-compose.yml`, README updated, migration doc shipped (`docs/migration-go.md`).
- M001-M006 all closed in `.gsd/` with their UAT signed.

---

## Reading order for the next agent

1. This file.
2. `M001-SPEC.md` (next file in this directory) — read in full before invoking `gsd headless new-milestone --context M001-SPEC.md`.
3. `M002-DRAFT.md` — bozza; expects refinement after M001 lands.
4. `/root/bellerophon/CLAUDE.md` — project conventions.
5. `/root/bellerophon/.gsd/CODEBASE.md` — current file map.
6. `/root/.gsd/agent/GSD-WORKFLOW.md` — GSD methodology.

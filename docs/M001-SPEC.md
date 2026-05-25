# M001 — SIP + Media Foundation (Go)

> Specification document for `gsd headless new-milestone --context M001-SPEC.md`. Read `00-VISION.md` first for the multi-milestone context. This file alone is sufficient context for the GSD planner/worker/tester agents to execute M001 without further user input.

---

## 1. Milestone vision

Build the SIP signalling and RTP media foundation of the Go binary. By the end of M001, `bellerophon` (the binary) can:

1. Read `config.yaml`, register itself as a SIP extension on the live 3CX instance.
2. Accept an inbound INVITE from a real soft-phone (Linphone / 3CX mobile app calling the registered extension).
3. Negotiate SDP with `PCMU,PCMA`, `a=ptime:20`.
4. Receive RTP from the caller and either **(a)** echo it back (M001 S04 demo) or **(b)** play a pre-recorded WAV file as a response (M001 S05 demo).
5. Detect DTMF (RFC 2833) and log digits.
6. Hang up cleanly on BYE.
7. No AI involvement at all (Whisper/Claude/ElevenLabs land in M002).

This is the load-bearing slab. If the SIP+RTP layer is wobbly, every downstream milestone collapses. We over-invest in tests here.

## 2. Success criteria (observable, measurable)

A reviewer agent must be able to verify these without re-reading the code:

1. **Register/Unregister:** Binary REGISTERs to 3CX on startup; logs `[sip] registered as ${SIP_EXTENSION}@${SIP_DOMAIN}`. On SIGINT, sends `REGISTER` with `Expires: 0` and exits clean. 3CX admin GUI shows the extension as offline within 5 s.
2. **Inbound INVITE:** A real call from a Linphone client (or another 3CX extension) to the bellerophon-registered extension is accepted, 200 OK sent, ACK received, RTP starts within 200 ms of ACK.
3. **Echo demo (S04 UAT):** Caller speaks "hello, this is a test"; the binary plays back the same audio after a 500 ms delay. No clicks, no warble — RMS deviation between input and echoed output (after delay-align) < 10 %.
4. **WAV playback (S05 UAT):** Caller is greeted by `voice-app/static/ready-beep.wav` followed by a TTS-generated WAV ("hello world, you have reached bellerophon"). Audio is clear, no codec artifacts beyond what G.711 inherently does.
5. **DTMF (S06 UAT):** Caller presses `1`, `2`, `3`, `*`, `#` on the keypad. Binary logs `[dtmf] received: 1`, `2`, `3`, `*`, `#` within 200 ms of each press. ≥ 99 % accuracy over 100 keypresses in a scripted test.
6. **Recording (S06 UAT):** With `RECORD_TO=/tmp/m001-test.wav` set, a 30 s call produces a valid WAV file (PCM16, 16 kHz, mono, length matches call duration ±50 ms), playable in VLC.
7. **Clean shutdown:** No goroutine leaks (`runtime.NumGoroutine()` returns to baseline ±2 after 100 calls). No file descriptor leaks (`lsof -p $(pgrep bellerophon) | wc -l` stable).
8. **Cross-compile:** `make release` produces working binaries for `linux/amd64`, `linux/arm64`, `darwin/arm64`, `darwin/amd64`. Each binary boots and registers (mock-3CX integration test) in CI.
9. **No CGO:** `CGO_ENABLED=0 go build` succeeds. `file ./bellerophon` reports `statically linked`.
10. **Binary size:** stripped `linux/arm64` binary ≤ 30 MB.

## 3. Out of scope for M001

- Anything AI: STT, LLM, TTS. (M002.)
- Outbound calls. (M003.)
- Admin HTTP/REST. (M003.)
- Multi-extension / multi-registrar. (M003.)
- DB / persistence. (M003 onwards.)
- VAD (voice activity detection). (M002 — only useful with STT.)
- Re-INVITE / hold / transfer. (Defer to M005 if needed; voice-app doesn't support hold either.)
- TLS (SIPS). (M006 nice-to-have; not in voice-app today.)

## 4. Reference material — read before planning

| Path | Why |
|------|-----|
| `voice-app/lib/sip-handler.js` (401 LOC) | Today's drachtio inbound INVITE handler. Source of truth for "what 3CX expects". |
| `voice-app/lib/multi-registrar.js` (225 LOC) | Today's registration logic. Only single-extension portion is M001 scope. |
| `voice-app/lib/audio-fork.js` (416 LOC) | FreeSWITCH→voice-app PCM stream. Shows the PCM16k 16 kHz mono format we must produce as STT input. |
| `voice-app/lib/drachtio-patch.js` | Wire-protocol crash workaround. The drachtio bug we patched there will NOT exist in sipgo, but the *behavior we depended on* matters. |
| `voice-app/config/devices.json` | Schema for multi-extension config. M001 supports only the first device. |
| `docker-compose.yml` | Shows current FreeSWITCH RTP range (30000-30100). Our Go binary should respect `RTP_PORT_RANGE` env (default `30000-30100`). |
| `CLAUDE-PHONE-INTEGRATION-SPEC.md` (/root/) | Definitive backend spec including network topology. |
| `https://github.com/emiago/sipgo` | sipgo v1.0.0 — README + examples/registrar + examples/uac. |
| `https://github.com/pion/rtp` | pion/rtp — Packet struct + Marshaller. |
| `RFC 3550` | RTP. Required reading for the jitter buffer slice (S04). |
| `RFC 2833` | DTMF over RTP. Required reading for S06. |

## 5. Slices

GSD planner: convert each slice into a `S0X-PLAN.md` with the listed must-haves and tasks. Boundary map at end of this section.

### S01 — Project skeleton + tooling
**Goal:** A buildable, lintable, testable Go project skeleton ready for sipgo/pion code. No SIP yet.
**Demo:** `make build test lint` passes; `./bin/bellerophon --version` prints `bellerophon 0.1.0-alpha (commit <sha>)`.

Must-haves:
- `go.mod` with module path `github.com/stefandsl/bellerophon` (or current owner; verify with `git config --get remote.origin.url`).
- Directory layout: `cmd/bellerophon/` (main), `internal/{sipua,rtp,codec,media,config,log}/`, `pkg/` (empty for now).
- `Makefile` targets: `build`, `test`, `lint`, `release` (cross-compiles linux/amd64+arm64, darwin/arm64+amd64), `clean`.
- `golangci-lint` config (`.golangci.yml`) enabling `govet,errcheck,staticcheck,unused,gofmt,goimports,gosec,bodyclose`.
- GitHub Actions workflow `.github/workflows/ci.yml`: matrix build × OS × arch + lint + test.
- `internal/log/` slog wrapper with leveled output (DEBUG/INFO/WARN/ERROR) + structured fields.
- Embedded `version.go` with `Version`, `Commit`, `BuildDate` set via `-ldflags`.

Tasks (planner will refine):
- T01 init module + layout
- T02 Makefile + ldflags version embed
- T03 golangci config + first lint pass green
- T04 GitHub Actions CI
- T05 slog wrapper

### S02 — Config loader (YAML + env + flags)
**Goal:** Single config struct populated by YAML, overridable by env vars (matching `voice-app` env names where applicable), overridable by CLI flags. No external config libraries (no viper) — keep deps minimal.
**Demo:** `./bellerophon --config config.yaml --sip.extension 1234` boots and prints the resolved config (no SIP yet).

Must-haves:
- `internal/config/Config` struct with fields: `SIP.{Domain,Registrar,RegistrarPort,Extension,AuthUsername,AuthID,AuthPassword,Expiry}`, `RTP.{ExternalIP,PortRange}`, `HTTP.{Port,TLSCert,TLSKey,TLSPort}`, `Logging.{Level,Format}`.
- `config.yaml` example file at `examples/config.yaml`.
- Env override: every config field maps to an env var. The mapping table is in `M001-CONTEXT.md` (next agent should write it) — uses the same env names as `voice-app` (SIP_DOMAIN, SIP_REGISTRAR, SIP_EXTENSION, etc.).
- CLI flag override via `flag` stdlib (no cobra dep yet — overkill for M001).
- Validation: missing required fields fail with helpful errors that name the field, the YAML key, the env var, and the flag.
- Unit tests: 90 % coverage on config package; test all three override layers and precedence.

Tasks:
- T01 Config struct + YAML unmarshal
- T02 env var override layer
- T03 flag override layer
- T04 validation + helpful errors
- T05 tests

### S03 — SIP UA: REGISTER + INVITE handler (sipgo)
**Goal:** Binary registers to 3CX as a SIP extension; accepts inbound INVITE; sends 100/180/200 OK; receives ACK; on BYE, sends 200 OK and tears down. No media yet — `200 OK` advertises a fake SDP, RTP slice closes the loop in S04.
**Demo:** `bellerophon` registered, soft-phone calls it, hears nothing but sees the call connect and then hang up cleanly when the user presses end.

Must-haves:
- `internal/sipua/Server` wraps sipgo's `Server`/`Client`.
- `Register(ctx, expiry) error` performs initial REGISTER + auto-refresh at 50 % of expiry. Handles 401 with auth.
- `OnInvite(handler InviteHandler)` registers a callback. Handler receives a `Call` struct with `From`, `To`, `CallID`, `RemoteSDP`, `Reply(code int, sdp string) error`, `OnBye(func())`.
- Auto-handles re-REGISTER, OPTIONS keepalives.
- Graceful shutdown: SIGINT → unregister (REGISTER w/ Expires:0) → wait 2 s → exit.
- Integration test against a `dockerized` opensips or against a real 3CX (gated by `BELLEROPHON_LIVE_3CX=1` env).
- Logs every state transition at INFO.

Tasks:
- T01 sipgo dependency + Server bootstrap (UDP/TCP listen on `SIP_PORT`)
- T02 REGISTER + auth (digest) + refresh
- T03 INVITE handler + 100/180/200/ACK
- T04 BYE handler + cleanup
- T05 Graceful shutdown + SIGINT handling
- T06 Integration test scaffolding

### S04 — RTP transport + jitter buffer + RTCP
**Goal:** Real RTP sessions tied to S03's calls. Echo demo: caller's audio comes back with a 500 ms delay. This proves the jitter buffer, codec, and timing all work end-to-end.
**Demo:** `bellerophon --echo-mode` — answer call, echo caller's audio back. Audible quality verified by human ear + automated RMS deviation < 10 %.

Must-haves:
- `internal/rtp/Session` opens a UDP socket in the configured port range (`RTP_PORT_RANGE`), sends/recvs pion/rtp packets.
- SDP offer/answer logic in `internal/sipua/sdp.go` — offers `PCMU,PCMA`, accepts whatever the caller answered with from that set. Includes `a=ptime:20`, `a=sendrecv`.
- Jitter buffer in `internal/rtp/jitter.go` — fixed 60 ms adaptive default. Discard packets older than `now - 100 ms`. Configurable via `RTP_JITTER_MS` env.
- RTCP SR/RR heartbeat every 5 s on `rtp_port + 1` per RFC 3550.
- Echo loop wired in `cmd/bellerophon/echo.go` (only active under `--echo-mode` flag).
- Synthetic loss test: drop 5 % of incoming packets — output must still be intelligible (no chained-error retransmits, no buffer underrun crashes).
- Marker bit, sequence number wraparound, RTP timestamp jumps (silence suppression CNG) all handled — write unit tests for each.

Tasks:
- T01 UDP socket + pion/rtp parse/marshal
- T02 SDP offer/answer
- T03 Jitter buffer
- T04 RTCP heartbeat
- T05 Echo mode wiring
- T06 Edge-case unit tests (loss, wrap, marker, CNG)

### S05 — Codec + media playback
**Goal:** G.711µ-law and A-law transcoding to/from PCM16 16 kHz mono (Whisper's format). WAV file playback over RTP (re-sample → encode → packetize → schedule send at 20 ms intervals).
**Demo:** `bellerophon --play examples/ready-beep.wav` — on inbound call, plays the WAV to the caller. No echo this time — pure playback.

Must-haves:
- `internal/codec/{pcmu,pcma}.go` — ITU-T G.711 reference tables, pure-Go, no CGO. Round-trip identity test: PCM16 → µ-law → PCM16 must match ITU reference within 1 LSB.
- `internal/codec/resample.go` — 8 kHz ↔ 16 kHz polyphase resampler (linear-phase, ~64-tap). Pure Go.
- `internal/media/wav.go` — read PCM WAV (16/8 kHz, mono/stereo, 8/16-bit). Reject MP3/M4A (point user at ffmpeg).
- `internal/media/playback.go` — `Playback.Play(rtpSession, wav) error` schedules 20 ms G.711 frames at exact intervals (use `time.Ticker` with monotonic correction). Supports interrupt (`ctx.Done()`).
- Tests: known WAV → expected RTP packet sequence (count, payload type, sequence, timestamp delta).
- Benchmark: must encode + send at >100x realtime on a Pi 4 (so concurrent calls scale).

Tasks:
- T01 PCMU codec + reference vector tests
- T02 PCMA codec + reference vector tests
- T03 Resampler 8↔16 kHz
- T04 WAV reader
- T05 Playback scheduler
- T06 Tests + benchmark

### S06 — DTMF detection + recording
**Goal:** RFC 2833 DTMF event detection from RTP stream. Optional call recording to WAV.
**Demo:** Caller presses keypad digits → logged. With `--record /tmp/call.wav`, the call audio (mixed both directions) is saved as a valid PCM16 WAV.

Must-haves:
- `internal/rtp/dtmf.go` — parse RFC 2833 events from RTP payload type 101 (or whatever the SDP negotiated). Emit `DTMFEvent{Digit byte, Duration time.Duration}` on a channel.
- Accuracy ≥ 99 % over 100 scripted keypresses (test harness sends synthetic RFC 2833 events).
- `internal/media/recorder.go` — mixes inbound + outbound PCM16k mono, writes WAV file. Atomic finalize (write to `.partial` then rename).
- Env: `RECORD_TO=/path/to/file.wav` triggers recording for the next call.
- Mixed audio level: simple sum + soft-clip (no AGC needed for M001).

Tasks:
- T01 RFC 2833 parser
- T02 DTMF event channel + integration with sipua
- T03 Recorder mix + write
- T04 Atomic finalize + cleanup on crash
- T05 Tests (synthetic events + golden WAV diff)

### S07 — Live 3CX integration test + UAT
**Goal:** End-to-end manual + automated test against the live 3CX instance currently used by `voice-app`. This is the gate before M002 starts.
**Demo:** Stefan dials the bellerophon-registered extension from his cell phone, hears the ready-beep, presses `1234#`, sees the digits logged, hangs up. Recording saved. No crashes, no stuck calls.

Must-haves:
- UAT script in `docs/m001-uat.md` — step-by-step manual test plan covering all 10 success criteria from Section 2.
- Automated integration test in `test/integration/live3cx_test.go` (skipped unless `BELLEROPHON_LIVE_3CX=1`) — performs REGISTER, places echo test against a `sipp`-driven UAC, validates audio round-trip.
- Run on Raspberry Pi 5 hardware (Stefan has one) — record results in `docs/m001-pi-benchmark.md` (boot time, register time, audio jitter, CPU at 8 concurrent calls).
- Migration notes: document gotchas discovered (3CX-specific SDP quirks, RTP IP advertisement issues) in `M001-SUMMARY.md`.

Tasks:
- T01 UAT manual script
- T02 sipp-driven integration test
- T03 Pi 5 hardware run + benchmark
- T04 Document quirks + close milestone

## 6. Boundary Map

```
S01 → S02
Produces:
  cmd/bellerophon/main.go     → main entry stub
  internal/log/log.go          → Logger interface + slog impl
  Makefile                     → build, test, lint, release targets

Consumes: nothing (leaf)

S02 → S03
Produces:
  internal/config/config.go    → Config struct + Load(path) (*Config, error)
  internal/config/validate.go  → Validate() error
  examples/config.yaml         → reference YAML

Consumes from S01:
  internal/log → Logger (config validation errors logged structured)

S03 → S04
Produces:
  internal/sipua/server.go     → Server, Register, OnInvite
  internal/sipua/call.go       → Call struct, Reply, OnBye, RemoteSDP
  internal/sipua/sdp.go        → ParseSDP, BuildSDP (stub for S03, real in S04)

Consumes from S02:
  Config.SIP.* fields

S04 → S05
Produces:
  internal/rtp/session.go      → Session.Send([]byte), Session.Recv() <-chan Packet
  internal/rtp/jitter.go       → JitterBuffer
  internal/sipua/sdp.go        → BuildSDP filled in (PCMU,PCMA negotiation)

Consumes from S03:
  Call.RemoteSDP, Call.Reply

S05 → S06
Produces:
  internal/codec/pcmu.go       → Encode/Decode
  internal/codec/pcma.go       → Encode/Decode
  internal/codec/resample.go   → Resample8to16, Resample16to8
  internal/media/wav.go        → ReadWAV
  internal/media/playback.go   → Playback.Play(rtpSess, wav)

Consumes from S04:
  Session.Send, Session.Recv

S06 → S07
Produces:
  internal/rtp/dtmf.go         → DTMFEvent channel
  internal/media/recorder.go   → Recorder.Start, Recorder.Stop

Consumes from S05:
  codec.Decode, media.WriteWAV

S07 → (M002)
Produces:
  docs/m001-uat.md             → UAT script (used as template for M002+)
  test/integration/            → sipp + live3cx test harness (reused)
  M001-SUMMARY.md              → list of 3CX quirks discovered

Consumes from S01-S06: everything
```

## 7. Risks specific to M001

1. **sipgo + 3CX REGISTER edge cases.** 3CX is picky about Contact header `transport=` and `;rinstance=` parameters. Mitigation: capture a packet trace from current drachtio REGISTER (`tcpdump -i any -w drachtio-register.pcap port 5060`) and replicate byte-for-byte.
2. **RTP IP advertisement.** Current FreeSWITCH uses `--ext-rtp-ip ${EXTERNAL_IP}`. The Go binary must offer the same external IP in SDP `c=` line or 3CX SBC drops audio. Mitigation: `RTP.ExternalIP` config + auto-detect fallback (STUN to `stun.l.google.com:19302`).
3. **Pi 5 ARM timing precision.** `time.Ticker` on Linux ARM has been known to drift under load. Mitigation: monotonic clock correction in `playback.go`; benchmark on real hardware in S07.
4. **3CX disconnects after 10 min on idle SIP.** Today drachtio handles OPTIONS keepalive. Verify sipgo does too.

## 8. Hand-off checklist (when M001 closes)

Before tagging M001 complete and starting M002, the reviewer must confirm:

- [ ] All 10 success criteria from Section 2 hit.
- [ ] CI green on linux/amd64, linux/arm64, darwin/arm64, darwin/amd64.
- [ ] `M001-SUMMARY.md` lists every 3CX quirk discovered with reproduction steps.
- [ ] `docs/migration-go.md` started (just a stub — full doc lands in M006).
- [ ] Stefan signed off on the UAT (`docs/m001-uat.md`).
- [ ] No `TODO M001` markers left in code (move to M002+ if needed).
- [ ] Coverage: ≥ 80 % on `internal/sipua`, `internal/rtp`, `internal/codec`, `internal/media`.
- [ ] Linter clean (`golangci-lint run` exit 0).

## 9. Notes for the GSD planner agent

- **Iron rule:** each task must fit in one context window. If S03 T03 (INVITE handler) looks too big, split into "T03a parse incoming INVITE" + "T03b build 200 OK + ACK reception".
- **Boundary map is binding.** Slices export only what the boundary map lists. If you need to add an export mid-slice, document it in `S0X-CONTEXT.md` and update the boundary map.
- **No premature abstraction.** Don't introduce interfaces "for testability" if the concrete type is already testable. The codec, jitter buffer, and recorder are stateful — keep them as concrete structs.
- **Tests are first-class.** A slice is not done until its tests are written and green. Reviewer rejects any T0X that ships code without tests in the same task.
- **Reference voice-app behavior, do not blindly copy.** voice-app has accumulated workarounds (`drachtio-patch.js`, `connection-retry.js`). Some are needed; some are scar tissue. When in doubt, ask the user (use `get-secrets-from-user` extension or pause for clarification).
- **Pure-Go is binding.** Reject any PR that introduces CGO without a recorded decision in `DECISIONS.md`.

# M002 — User Acceptance Test (UAT)

This document is the step-by-step manual verification plan for the M002
"Inbound AI Conversation Loop" milestone. Same shape as
[`m001-uat.md`](m001-uat.md) — each section maps one-to-one to a
success criterion from [`M002-DRAFT.md §2`](M002-DRAFT.md) (promote to
`M002-SPEC.md` before signoff), with a clear-cut go/no-go. **Any
section fail reopens the slice it falls under — do not sign off the
milestone with a partial pass.**

## Prerequisites

- A built `bellerophon` binary for the host running the test
  (`make build` produces `bin/bellerophon`).
- `sox` for converting recorded audio to the raw PCM16/16k fixtures
  the harness expects:

  ```sh
  sox in.wav -r 16000 -b 16 -c 1 -e signed-integer hello.pcm
  ```

- API keys for the three live legs (export before running):
  - `OPENAI_API_KEY` — Whisper (§B)
  - `ANTHROPIC_API_KEY` — direct LLM (§C-direct)
  - `ELEVENLABS_API_KEY` — TTS (§D)
- For the bridge LLM leg (§C-bridge): a running
  `claude-api-server` instance reachable at `CLAUDE_API_URL`
  (defaults to `http://localhost:3333`).
- For the parity comparison (§G): a working `voice-app` deployment
  with `docker compose up` operational, so calls can be placed
  against both stacks on the same audio.

## Required audio fixtures

Record once with your speakerphone or Linphone "save call audio"
and convert with `sox`:

| File | Phrase | Used by |
|---|---|---|
| `hello.pcm` | "Hello, how are you today?" | §B (Whisper) |
| `question.pcm` | "What's the weather like in Rome right now?" | §F (pipeline) |
| `goodbye.pcm` | "Goodbye, thanks for your help." | §F (goodbye keyword) |

Each MUST be mono PCM16 LE at 16 kHz. Anything else and Whisper will
hear noise.

---

## Section A — Build + offline guard

A.1 — `go test -race ./...` is green on the current branch.
A.2 — `go test -race -count=1 -run TestOfflineLoopOverhead ./test/integration/...`
       prints a per-turn overhead under 25 ms.

**Pass criterion**: all tests green; overhead per turn ≤ 25 ms.

---

## Section B — STT integration (M002 criterion §2.1)

```sh
OPENAI_API_KEY=sk-... \
BELLEROPHON_AUDIO_FIXTURE=$PWD/hello.pcm \
WHISPER_EXPECT=hello \
BELLEROPHON_LIVE_WHISPER=1 \
go test -v -run TestLive_STT_Whisper ./test/integration/...
```

**Expected log**: `Whisper returned "Hello, how are you today?" in <latency>`.
**Pass criterion**: transcript contains every keyword from `WHISPER_EXPECT`,
returned in under 5 s.

---

## Section C — LLM integration (M002 criterion §2.2)

Two paths must both pass.

### C-direct — Anthropic Messages API

```sh
ANTHROPIC_API_KEY=sk-ant-... \
BELLEROPHON_LIVE_ANTHROPIC=1 \
go test -v -run TestLive_LLM_Anthropic ./test/integration/...
```

**Expected**: turn 1 returns a non-empty reply, turn 2 references the
name "Bellerophon" from turn 1 (proving the multi-turn history is
honoured by the on-disk SessionStore).

### C-bridge — claude-api-server

In a separate terminal: `cd /path/to/bellerophon/claude-api-server &&
node server.js`. Confirm `GET http://localhost:3333/health` returns
`{ ok: true }`.

```sh
CLAUDE_API_URL=http://localhost:3333 \
BELLEROPHON_LIVE_BRIDGE=1 \
go test -v -run TestLive_LLM_Bridge ./test/integration/...
```

**Pass criterion** (both legs): non-empty reply, `EndSession` returns
without error.

---

## Section D — TTS integration (M002 criterion §2.3)

```sh
ELEVENLABS_API_KEY=... \
BELLEROPHON_LIVE_ELEVENLABS=1 \
go test -v -run TestLive_TTS_ElevenLabs ./test/integration/...
```

**Expected**: first synth ~400–800 ms, second synth (cache hit)
sub-ms; speedup ≥ 5×; returned bytes between 1 s and 10 s of PCM
duration for the test phrase.

**Pass criterion**: assertions all green; cache hit is dramatically
faster than the first call.

---

## Section E — VAD + barge-in (M002 criteria §2.4–§2.5)

E.1 — `go test -race -count=1 ./internal/vad/...` is green.
       (Pins end-of-utterance ≤ 1.5 s of silence.)
E.2 — `go test -race -count=1 -run TestEnergyBarge_LatencyUnder100ms
       ./internal/conversation/...` is green.
       (Pins barge-in detection latency ≤ 100 ms.)
E.3 — Manual live check (with an inbound call once the SIP integration
       lands in M003):
       1. Place a call to the registered extension.
       2. Wait for the greeting to start playing.
       3. Interrupt by talking over the TTS.
       4. **Expected**: TTS audibly cuts within ~100 ms; loop returns
          to LISTENING and the interruption is transcribed as the next
          turn.

**Pass criterion**: E.1 and E.2 green now; E.3 deferred to M003 when
the SIP-side wiring exists (this section reopens then).

---

## Section F — End-to-end pipeline latency (M002 criterion §2.7)

This is the headline benchmark. **P95 ≤ 2.5 s** from "caller stops
speaking" → "first TTS audio byte" — measured over at least 5 turns.

```sh
OPENAI_API_KEY=... \
ANTHROPIC_API_KEY=... \
ELEVENLABS_API_KEY=... \
BELLEROPHON_AUDIO_FIXTURE=$PWD/question.pcm \
PIPELINE_ITERATIONS=10 \
PIPELINE_P95_BUDGET_MS=2500 \
BELLEROPHON_LIVE_PIPELINE=1 \
go test -v -timeout 5m -run TestLive_Pipeline_E2ELatency ./test/integration/...
```

**Expected log table**:

```
stage       n    min    p50    p95    p99    max    mean
stt    10   <ms>  <ms>  <ms>  <ms>  <ms>  <ms>
llm    10   <ms>  <ms>  <ms>  <ms>  <ms>  <ms>
tts    10   <ms>  <ms>  <ms>  <ms>  <ms>  <ms>
total  10   <ms>  <ms>  <ms>  <ms>  <ms>  <ms>
budget: P95 ≤ 2500 ms
```

**Pass criterion**: `total` P95 ≤ 2500 ms. Per-stage outliers (e.g.
Whisper occasional 800 ms tail) are logged but don't fail the gate.

**Record the numbers into [`M002-SUMMARY.md`](M002-SUMMARY.md) §4 before signoff.**

---

## Section G — Goodbye + transcript JSONL (M002 criteria §2.6, §2.8)

G.1 — Unit tests cover the keyword list:
       `go test -count=1 -run TestIsGoodbye ./internal/conversation/...`
G.2 — Live: place a call (M003 wiring required) and say "goodbye" —
       the loop must play the farewell phrase and end the call.
G.3 — Verify `transcripts/<callid>.jsonl` was created and contains
       one `{turn, role, text, timestamp_ms}` line per spoken turn,
       parseable with `jq .` line-by-line.

**Pass criterion**: G.1 green now; G.2/G.3 deferred to the same M003
SIP-wiring step that unblocks E.3.

---

## Section H — Parity vs voice-app (M002 success-criterion §6)

H.1 — Boot voice-app on the same hardware:
       `cd bellerophon && docker compose up -d`.
       Confirm `/health` returns OK and a test call is answerable.
H.2 — Boot bellerophon-go on the same hardware (different SIP port to
       avoid REGISTER conflicts).
H.3 — Place an identical inbound call to each (deferred to M003).
       Compare:
       - Time to first TTS audio byte (P50, P95)
       - Transcript correctness for the same phrase
       - Audio quality (subjective; record both and listen)
H.4 — Document deltas in [`M002-SUMMARY.md`](M002-SUMMARY.md) §5.

**Pass criterion**: bellerophon-go is no worse than voice-app on
latency, transcript accuracy, and perceptual audio quality. A small
regression is acceptable if explicitly called out in §5 with a fix
scheduled for M005.

---

## Signoff

Stefan signs the milestone when:

- Sections A, B, C, D, F all green.
- Section E.1 + E.2 green (E.3 deferred to M003 with explicit note).
- Section G.1 green (G.2 + G.3 deferred to M003 with explicit note).
- Section H rows filled in `M002-SUMMARY.md` with measured deltas.

A partial pass (e.g. F.P95 above 2500 ms) reopens the slice that
covers the failing stage — STT failures reopen S02, LLM failures
reopen S03, TTS failures reopen S04, latency-budget failures reopen
S06 with an explicit optimization scope.

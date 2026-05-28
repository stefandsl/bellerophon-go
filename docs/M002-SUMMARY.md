# M002 — Inbound AI Conversation Loop — Summary

> **Status: DRAFT — awaiting Stefan's UAT runthrough.** This document
> closes the milestone once Sections 4 and 5 are filled with measured
> numbers from the UAT.

## 1. What shipped

Five slices, all merged via PRs #12–#16, plus this UAT/benchmark slice:

| Slice | Package | LOC | PR |
|---|---|---|---|
| S01 — VAD | `internal/vad` | ~250 + tests | #12 |
| S02 — STT (OpenAI Whisper) | `internal/stt` | ~220 + tests | #13 |
| S03 — LLM (Anthropic + bridge) | `internal/llm` | ~700 + tests | #14 |
| S04 — TTS (ElevenLabs) | `internal/tts` | ~600 + tests | #15 |
| S05 — Conversation loop + barge-in | `internal/conversation` | ~900 + tests | #16 |
| S06 — UAT harness + benchmark | `test/integration`, `docs/` | ~600 | this PR |

Every slice hand-rolls its upstream HTTP client (no `openai-go`, no
`anthropic-sdk-go`, no `elevenlabs-go`) for consistency with M001's
zero-vendor-SDK posture. Tests use `httptest.NewServer` end-to-end;
no live calls happen in CI.

## 2. Success criteria mapped to the code

From [`M002-DRAFT.md` §2](M002-DRAFT.md):

| # | Criterion | Status | Where it lives |
|---|---|---|---|
| 2.1 | STT integration (Whisper working) | ✅ | `internal/stt/whisper.go` + UAT §B |
| 2.2 | LLM integration (direct + HTTP fallback) | ✅ | `internal/llm/anthropic.go`, `bridge.go` + UAT §C |
| 2.3 | TTS integration (ElevenLabs default) | ✅ | `internal/tts/elevenlabs.go` + UAT §D |
| 2.4 | VAD end-of-utterance ≤ 1.5 s | ✅ | `internal/vad/vad.go` (unit tests pin) |
| 2.5 | Barge-in TTS abort ≤ 100 ms | ✅ | `internal/conversation/barge.go` (`TestEnergyBarge_LatencyUnder100ms`) |
| 2.6 | Goodbye keyword detection | ✅ | `internal/conversation/voicestrings.go` (`IsGoodbye`) |
| 2.7 | End-to-end P95 ≤ 2.5 s | ⏳ | UAT §F — fill in once measured |
| 2.8 | Transcript JSONL persistence | ✅ | `internal/conversation/transcript.go` |

## 3. Open-questions resolution log

[`M002-DRAFT.md §5`](M002-DRAFT.md) flagged five open questions to
decide before promoting to SPEC. Resolutions:

| Q | Question | Decision | Where |
|---|---|---|---|
| Q1 | anthropic-sdk-go vs hand-rolled? | Hand-rolled `net/http`. Matches S02 Whisper, no extra dep, full control over headers/errors. | S03 PR #14 |
| Q2 | OpenAI Whisper API vs `whisper.cpp` subprocess? | OpenAI API (mirrors voice-app). `whisper.cpp` for offline lands in M005. | S02 PR #13 |
| Q3 | ElevenLabs streaming vs full-file? | Full-file via `output_format=pcm_16000`. Streaming + barge-in is M005's job, and a streaming flag without a state machine to exploit it would be premature. | S04 PR #15 |
| Q4 | Transcript format? | `{turn, role, text, timestamp_ms}` JSONL per call. M003 admin endpoints will consume the same shape. | S05 PR #16 |
| Q5 | Barge-in sensitivity (single threshold or two)? | Separate detector (`EnergyBarge`) tuned tighter than the main VAD: RMS 4000 vs 3000, MinOnsetMs 60 vs MinSpeechMs 350, no hangover. | S05 PR #16 |

## 4. Latency benchmark (M002 §2.7)

> **TODO Stefan**: paste the table from `TestLive_Pipeline_E2ELatency`
> after running UAT §F. The criterion is **total P95 ≤ 2500 ms**.

```
stage       n    min    p50    p95    p99    max    mean
stt        TODO
llm        TODO
tts        TODO
total      TODO
```

Hardware: **TODO** (Pi 5 / dev laptop / which?).
Date: **TODO**.

### Observations

> **TODO**: any per-stage outlier patterns? Which stage dominates the
> P95? If we miss the budget, which slice gets reopened?

## 5. Parity vs voice-app

> **TODO Stefan**: fill from UAT §H.

| Metric | voice-app (Node) | bellerophon (Go) | Δ |
|---|---|---|---|
| P50 total latency | TODO | TODO | TODO |
| P95 total latency | TODO | TODO | TODO |
| Transcript accuracy on `question.pcm` | TODO | TODO | TODO |
| Audio quality (subjective 1–5) | TODO | TODO | TODO |
| Cold-start memory (RSS at idle) | TODO | TODO | TODO |
| Cold-start memory (RSS during call) | TODO | TODO | TODO |

### Verdict

> **TODO Stefan**: "go" / "go with caveats" / "regression — fix in
> M005". If "go with caveats", list each caveat and the scheduled
> follow-up slice.

## 6. What does NOT yet work end-to-end

The Go binary doesn't have a runnable inbound call path yet — the
conversation loop in `internal/conversation` is library code waiting
on a caller. The missing pieces all land in M003:

- **Player implementation**: production `conversation.Player` backed
  by `internal/rtp` (S01–S06 left it as an interface).
- **Audio capture goroutine**: the loop expects someone else to pump
  PCM into the VAD and the BargeIn detector. M003 wires this up to
  the RTP receive side.
- **SIP INVITE → conversation loop bridge**: today `internal/sipua`
  handles REGISTER + echo mode; M003's first slice spins the loop
  on every incoming call.

UAT sections **E.3, G.2, G.3** are explicitly deferred to M003 for
that reason. Their unit-test analogues (E.1, E.2, G.1) all pass now.

## 7. Decisions baked in that future slices should respect

1. **One `Player` interface across providers.** Future Bark / OpenAI
   TTS backends just need to satisfy `tts.Provider`; the loop
   doesn't see the provider name.
2. **One LLM `Client` interface.** Adding Gemini / Codex / OpenAI as
   non-bridge backends means another `internal/llm/foo.go`, not a
   new interface.
3. **Single PCM lingua franca: mono 16-bit LE @ 16 kHz.** TTS
   delivers it, VAD consumes it, the BargeIn detector consumes it.
   `internal/codec` handles 16 ↔ 8 kHz + G.711 at the RTP edge.
4. **Session persistence is the LLM client's concern, not the
   loop's.** The loop just threads `CallID`; what to remember per
   call (history vs sessionId vs nothing) is the implementation's
   choice.
5. **No SDKs, ever, for upstreams that publish a stable HTTP API.**
   Each new provider client is ~200 LOC of `net/http` with
   `httptest`-mockable seams. Saves dep churn, makes the audit
   surface explicit.

## 8. Signoff

- [ ] All UAT §A–§G pass (with E.3 / G.2 / G.3 deferred and noted).
- [ ] §4 latency table populated and P95 ≤ 2500 ms.
- [ ] §5 parity table populated.
- [ ] Verdict in §5 is "go" or "go with caveats" (caveats enumerated).
- [ ] Signed: Stefan, date.

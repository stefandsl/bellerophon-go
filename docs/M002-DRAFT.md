# M002 (DRAFT) — Inbound AI Conversation Loop

> **Draft.** Refine after M001 closes — concrete per-provider quirks (MessageNet, generic, 3CX) and timing data from M001's `M001-SUMMARY.md` will sharpen the success criteria here. Do NOT pass to `gsd headless new-milestone` until M001 S07 is signed off and this file is promoted from `M002-DRAFT.md` to `M002-SPEC.md`.

---

## 1. Milestone vision

Wire the M001 SIP+media foundation to the AI brain. M002 inherits M001's provider-agnostic SIP UA and `internal/sipprov/` layer — **everything below works identically whether the registrar is MessageNet, Asterisk, or 3CX.** No provider branching in the conversation loop, STT, LLM, or TTS code. By the end of M002:

1. An inbound call to the registered extension/DID is answered (provider transparent).
2. Caller speech is captured, voice-activity-detected, sent to Whisper (or another STT provider via config), transcribed.
3. Transcript is sent to Claude (default = direct Anthropic SDK; optional = HTTP to `claude-api-server`), session ID maintained across turns.
4. Claude's reply is sent to ElevenLabs TTS, audio streamed back to caller over RTP.
5. Caller can interrupt (barge-in): if caller speaks while TTS is playing, playback stops and STT resumes.
6. Goodbye keyword detection ends the call cleanly.
7. End-to-end P95 latency (caller-stops-speaking → first-TTS-audio-byte) ≤ 2.5 s.

This is the first milestone where a tutorial viewer can have a working voice-AI conversation. M001 was infrastructure; M002 is product.

## 2. Provisional success criteria

Refine after M001:

1. STT integration: at least Whisper (OpenAI) working. Provider plug-in interface defined and documented for Gemini/GCloud/openai-direct in M003.
2. LLM integration: both code paths work — direct Anthropic SDK call (default) AND HTTP to `CLAUDE_API_URL` when set. Session ID persisted across turns via `--resume`-equivalent semantics.
3. TTS integration: ElevenLabs default. Bark deferred to M005.
4. VAD: end-of-utterance detected within 1.5 s of silence (configurable via `VAD_SILENCE_MS`, matching today's env name).
5. Barge-in: TTS playback aborts within 100 ms of caller speech detected.
6. Goodbye detection: keyword list configurable, default `["bye", "goodbye", "ciao", "arrivederci", "hangup"]`. On match, plays a goodbye phrase then sends BYE.
7. Latency: P95 ≤ 2.5 s on the same hardware where voice-app runs today (measure both, side-by-side, document in `M002-SUMMARY.md`).
8. Transcript persistence: each call writes `transcripts/${CallUUID}.jsonl` with `{turn, role, text, timestamp_ms}` lines. Used by M003 admin endpoints.

## 3. Out of scope for M002

- Outbound calls. (M003.)
- Multi-extension. (M003.)
- Admin HTTP/REST. (M003 — only `/health` exposed here.)
- DB persistence. (M003 — transcripts stored as JSONL files for now.)
- Sales agent, batch dialer. (M004.)
- Other STT/TTS providers beyond Whisper + ElevenLabs. (M003 for STT, M005 for Bark.)
- Response cache. (M004 — optimization.)

## 4. Provisional slices (refine after M001)

### S01 — VAD + utterance segmentation
- Port `voice-app/lib/audio-fork.js` VAD logic to Go (`internal/vad/`).
- RMS + hangover; configurable thresholds (`VAD_SILENCE_THRESHOLD`, `VAD_SILENCE_MS`, `VAD_SPEECH_MS`).
- Emits `Utterance{PCM16k []byte, DurationMs int}` on a channel.

### S02 — STT provider interface + Whisper
- `internal/stt/Provider` interface: `Transcribe(ctx, pcm16k []byte) (text string, err error)`.
- Whisper implementation (OpenAI HTTP API).
- Provider selection via `STT_PROVIDER` env (only "whisper" supported here; "gemini"/"gcloud"/"openai" land in M003).

### S03 — LLM client (anthropic SDK + HTTP fallback)
- `internal/llm/Client` interface: `Query(ctx, prompt, sessionID) (response, newSessionID, err error)`.
- Default impl: direct Anthropic Messages API call (Anthropic Go SDK).
- HTTP impl: when `CLAUDE_API_URL` set, POST to `${CLAUDE_API_URL}/query` with body matching today's `voice-app/lib/claude-bridge.js` contract.
- Session ID persistence in memory + on-disk JSONL for resume across binary restarts.

### S04 — TTS provider interface + ElevenLabs
- `internal/tts/Provider` interface: `Synthesize(ctx, text, voiceID string) (pcm16k []byte, err error)`.
- ElevenLabs HTTP streaming API: stream PCM16k chunks as they arrive (don't wait for full file).
- Cache by `hash(text+voiceID)` to a configurable dir (matches `voice-app` cache behavior).

### S05 — Conversation state machine + barge-in
- Port `voice-app/lib/conversation-loop.js` state machine to Go (`internal/conversation/loop.go`).
- States: `IDLE → LISTENING → TRANSCRIBING → THINKING → SPEAKING → LISTENING ...` → `HANGUP`.
- Barge-in: while in SPEAKING, if VAD reports speech, jump to LISTENING (abort playback context).
- Goodbye detection on every transcript turn.
- Transcript JSONL writer.

### S06 — UAT + benchmark + parity test
- Side-by-side test: same call placed against `voice-app` and `bellerophon` Go binary. Compare P50/P95/P99 latency, audio quality, accuracy.
- Document in `M002-SUMMARY.md` what's better/worse/same.
- Stefan signs UAT.

## 5. Open questions to resolve before promoting to M002-SPEC.md

These are flagged for Stefan to decide between M001 close and M002 start:

1. **Anthropic SDK choice.** Use `github.com/anthropics/anthropic-sdk-go` (official) or hand-rolled `net/http` client? Official is ~5 MB add; hand-rolled is ~0 MB but more code to maintain.
2. **Whisper API choice.** OpenAI Whisper API or self-hosted `whisper.cpp`? Today's voice-app uses OpenAI's API (`/v1/audio/transcriptions`). Should we mirror, or include `whisper.cpp` via subprocess for offline?
3. **ElevenLabs streaming vs full-file.** Streaming = lower TTFB but more complex error handling. Full-file = simpler, matches today's voice-app behavior. Default to full-file in M002 S04, add streaming flag in M005?
4. **Transcript file format.** JSONL per call (`transcripts/${uuid}.jsonl`) — confirmed direction, but does M003 need a different schema? Coordinate with M003 admin endpoint design.
5. **Barge-in sensitivity.** Today's voice-app has a configurable barge-in threshold separate from VAD threshold. Replicate or simplify to "any VAD speech = barge-in"?

## 6. References (in addition to M001's)

| Path | Why |
|------|-----|
| `voice-app/lib/conversation-loop.js` (543 LOC) | The state machine to port. Read in full. |
| `voice-app/lib/claude-bridge.js` | HTTP contract with claude-api-server. |
| `voice-app/lib/whisper-client.js` | Whisper API call shape. |
| `voice-app/lib/tts-service.js` (327 LOC) | ElevenLabs integration + cache. |
| `voice-app/lib/stt/whisper.js` | Provider interface example. |
| `voice-app/lib/voice-strings.js` (264 LOC) | Goodbye keywords + multilingual phrases. Port verbatim. |
| `claude-api-server/backends/claude.js` | Reference for the HTTP fallback shape. |
| `https://github.com/anthropics/anthropic-sdk-go` | Anthropic Go SDK. |

## 7. Notes for whoever promotes this draft

1. After M001 S07 close, re-read `M001-SUMMARY.md` and integrate every per-provider quirk (MessageNet, generic, 3CX) discovered into M002's slices as test cases. M002 itself adds no provider-specific code — but the AI loop must be tested across all three providers to catch any unintended coupling.
2. Update Section 2 success criteria with **measured** numbers from M001's Pi 5 benchmark (not the "≤ 2.5 s" placeholder).
3. Resolve every open question in Section 5 by either asking Stefan or recording the answer in `DECISIONS.md`.
4. Rename file to `M002-SPEC.md` then run `gsd headless new-milestone --context M002-SPEC.md`.

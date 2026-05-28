# test/integration — live multi-provider tests

The tests in this directory exercise the full SIP/RTP path **and** the
M002 conversation pipeline against **real** upstreams. Categories:

- **M001 SIP/RTP legs** — generic Asterisk, MessageNet (DID provider),
  3CX. Exercise REGISTER + INVITE + RTP echo.
- **M002 conversation legs** — Whisper STT, Anthropic (direct + bridge)
  LLM, ElevenLabs TTS, and the full end-to-end pipeline benchmark.
- **Offline guards** — the `TestOfflineLoopOverhead` regression check
  that runs in every CI build (no env gate; uses fakes).

All `Live_*` tests are gated by environment variables so unit-test runs
(`go test ./...`) skip them with a clear message and finish quickly.

## Gates

| File | Env var to enable | Covers |
|---|---|---|
| `live_generic_test.go` | `BELLEROPHON_LIVE_GENERIC=1` | M001 §UAT-B |
| `live_messagenet_test.go` | `BELLEROPHON_LIVE_MESSAGENET=1` | M001 §UAT-A |
| `live_3cx_test.go` | `BELLEROPHON_LIVE_3CX=1` | M001 §UAT-C |
| `live_stt_whisper_test.go` | `BELLEROPHON_LIVE_WHISPER=1` | M002 §UAT-B |
| `live_llm_anthropic_test.go` | `BELLEROPHON_LIVE_ANTHROPIC=1` | M002 §UAT-C-direct |
| `live_llm_bridge_test.go` | `BELLEROPHON_LIVE_BRIDGE=1` | M002 §UAT-C-bridge |
| `live_tts_elevenlabs_test.go` | `BELLEROPHON_LIVE_ELEVENLABS=1` | M002 §UAT-D |
| `live_pipeline_test.go` | `BELLEROPHON_LIVE_PIPELINE=1` | M002 §UAT-F (the headline benchmark) |
| `bench_loop_test.go` | (none — always runs) | M002 §UAT-A offline guard |

A missing gate → `t.Skip()`. The gate variable name is echoed in the
skip message so a CI reviewer can see exactly what to set.

## Tooling required when enabled

### M001 SIP legs

- **`sipp`** in PATH (`apt install sip-tester` on Debian/Ubuntu).
- For `live_generic_test.go`: Docker + the bundled `docker-compose.asterisk.yml`
  (or any Asterisk instance reachable on the host network — pass its
  address via `ASTERISK_HOST` / `ASTERISK_PORT`).
- For the MessageNet test: an account on a DID provider (MessageNet by
  default) with at least one DID you can call from PSTN.

### M002 conversation legs

- An OpenAI account with Whisper access (`OPENAI_API_KEY`).
- An Anthropic account (`ANTHROPIC_API_KEY`).
- An ElevenLabs account (`ELEVENLABS_API_KEY`).
- For the bridge LLM leg: a running `claude-api-server` (in the
  `bellerophon` repo at `claude-api-server/`) reachable at
  `CLAUDE_API_URL` (default `http://localhost:3333`).
- For tests that need audio input: a recorded mono PCM16 LE 16 kHz
  fixture. The path is passed via `BELLEROPHON_AUDIO_FIXTURE`. Make
  one with `sox in.wav -r 16000 -b 16 -c 1 -e signed-integer out.pcm`.

See `docs/m001-uat.md` and `docs/m002-uat.md` for the matching
**manual** UAT scripts.

## Running

```sh
# Just the always-skipped sanity (verifies the scaffolding compiles)
# plus the offline overhead guard:
go test -v ./test/integration/...

# ---- M001 SIP legs ----

# Dockerized Asterisk leg:
docker compose -f test/integration/docker-compose.asterisk.yml up -d
BELLEROPHON_LIVE_GENERIC=1 go test -v ./test/integration/...

# MessageNet DID provider:
MESSAGENET_USER=... MESSAGENET_PASS=... MESSAGENET_DID=... \
  BELLEROPHON_LIVE_MESSAGENET=1 go test -v ./test/integration/...

# 3CX:
THREECX_DOMAIN=... THREECX_EXTENSION=... THREECX_PASSWORD=... \
  BELLEROPHON_LIVE_3CX=1 go test -v ./test/integration/...

# ---- M002 conversation legs ----

# Whisper STT (requires a recorded fixture):
OPENAI_API_KEY=... \
BELLEROPHON_AUDIO_FIXTURE=$PWD/hello.pcm \
WHISPER_EXPECT=hello \
BELLEROPHON_LIVE_WHISPER=1 \
  go test -v -run TestLive_STT_Whisper ./test/integration/...

# Anthropic direct LLM:
ANTHROPIC_API_KEY=... \
BELLEROPHON_LIVE_ANTHROPIC=1 \
  go test -v -run TestLive_LLM_Anthropic ./test/integration/...

# claude-api-server bridge (start server on :3333 first):
CLAUDE_API_URL=http://localhost:3333 \
BELLEROPHON_LIVE_BRIDGE=1 \
  go test -v -run TestLive_LLM_Bridge ./test/integration/...

# ElevenLabs TTS + cache speedup check:
ELEVENLABS_API_KEY=... \
BELLEROPHON_LIVE_ELEVENLABS=1 \
  go test -v -run TestLive_TTS_ElevenLabs ./test/integration/...

# Headline M002 §7 pipeline benchmark (P95 ≤ 2.5 s budget):
OPENAI_API_KEY=... ANTHROPIC_API_KEY=... ELEVENLABS_API_KEY=... \
BELLEROPHON_AUDIO_FIXTURE=$PWD/question.pcm \
PIPELINE_ITERATIONS=10 \
PIPELINE_P95_BUDGET_MS=2500 \
BELLEROPHON_LIVE_PIPELINE=1 \
  go test -v -timeout 5m -run TestLive_Pipeline_E2ELatency ./test/integration/...
```

## What each test asserts

### M001 SIP legs

All three follow the same shape (REGISTER → INVITE → RTP echo → BYE) so
a provider quirk that breaks one and not the other points straight at
`internal/sipprov/<provider>.go`:

1. Boot a `bellerophon --echo-mode` subprocess pointed at the provider.
2. Wait for the `[sip] registered ...` log line within 10 s.
3. Use `sipp` (the canonical SIP load tester) to place an INVITE with
   a 5-second G.711 sine payload.
4. Verify `sipp` reports the call as answered, the 200 OK has a
   negotiable SDP, and the echoed RTP arrives within ±50 ms of
   500 ms latency.
5. Send `Ctrl-C` to the subprocess and verify it unregisters and exits
   within 2 s.

Failures are diagnosed by dumping the subprocess log and the `sipp`
PCAP into `t.TempDir()` so the next reviewer can replay them.

### M002 conversation legs

- **`TestLive_STT_Whisper`** — POSTs the fixture to OpenAI, asserts the
  transcript contains every keyword from `WHISPER_EXPECT`, returns
  within 5 s.
- **`TestLive_LLM_Anthropic`** — two-turn exchange against
  `api.anthropic.com`, asserts turn 2 reply references a name from
  turn 1 (proves on-disk session resume works).
- **`TestLive_LLM_Bridge`** — same shape against a local
  `claude-api-server` `/ask` endpoint.
- **`TestLive_TTS_ElevenLabs`** — renders one phrase twice, asserts
  the second call is ≥5× faster than the first (proving cache hit),
  PCM duration is plausible for the input.
- **`TestLive_Pipeline_E2ELatency`** — the headline. Runs
  `PIPELINE_ITERATIONS` (default 5) full STT→LLM→TTS round-trips
  against real APIs, prints per-stage + cumulative P50/P95/P99 stats,
  fails only if cumulative P95 exceeds `PIPELINE_P95_BUDGET_MS`
  (default 2500, the M002 §7 budget). Per-stage outliers are logged
  but don't gate.
- **`TestOfflineLoopOverhead`** — always-runs regression guard. Uses
  fakes with fixed simulated latencies and asserts the conversation
  loop's per-turn overhead stays under 25 ms (catches regressions
  that would add a busy-wait or stray `time.Sleep`).

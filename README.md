# Bellerophon

A single-binary Go implementation of the Bellerophon voice stack: SIP signaling
(via [`sipgo`](https://github.com/emiago/sipgo)) and RTP media (via
[`pion/rtp`](https://github.com/pion/rtp)) wired to Whisper-style STT, a local
Claude bridge, and ElevenLabs-style TTS.

The original v1 stack (Docker + drachtio + FreeSWITCH + Node + Python) lives at
[`stefandsl/bellerophon`](https://github.com/stefandsl/bellerophon). This repo
is the simplified v2 path: one binary, no container runtime, no JS/Python
toolchains for end users.

## Status

Early. This is the project skeleton — `cmd/bellerophon` boots, loads YAML
config, validates it, and prints structured logs. The SIP/RTP/voice pipeline
packages exist as placeholders and arrive in later milestones (see
`docs/milestones.md`).

## Build

Requires Go 1.23+.

```bash
git clone https://github.com/stefandsl/bellerophon-go.git
cd bellerophon-go
go build -o bellerophon ./cmd/bellerophon
./bellerophon --version
```

## Run

```bash
cp config.example.yaml config.yaml
# edit config.yaml — set sip.*, stt/tts api_key_env vars, claude.endpoint
./bellerophon --check-config --config config.yaml
./bellerophon --config config.yaml
```

## CLI

| Flag | Purpose |
|---|---|
| `--config <path>` | YAML config file (required unless `--version`) |
| `--version` | Print version, commit, build date |
| `--check-config` | Validate config and exit; non-zero on structural errors |

`--check-config` distinguishes hard errors (missing required fields, malformed
values) from warnings (unset env-var secrets, placeholder values still in
place). Warnings do not fail the check.

## Releases (planned)

Cross-built artifacts will be published as:

```
bellerophon-darwin-arm64
bellerophon-darwin-amd64
bellerophon-linux-arm64
bellerophon-linux-amd64
```

Example download (not yet available):

```bash
curl -L -o bellerophon \
  https://github.com/stefandsl/bellerophon-go/releases/download/v0.1.0/bellerophon-linux-arm64
chmod +x bellerophon
./bellerophon --config config.yaml
```

## Layout

```
cmd/bellerophon/        # main; CLI, config load, log init
internal/config/        # YAML schema + validation
internal/logging/       # slog setup
internal/sip/           # (M3) sipgo registration + call handling
internal/rtp/           # (M5/M10) UDP RTP receive/send
internal/audio/         # G.711 codec, WAV debug
internal/vad/           # (M6) energy VAD + utterance buffering
internal/stt/           # (M7) Whisper-compatible client
internal/claude/        # (M8) HTTP client for the local Claude bridge
internal/tts/           # (M9) ElevenLabs-compatible client
internal/conversation/  # (M11) call-level state machine
docs/                   # plans and viewer-facing setup
```

## License

TBD.

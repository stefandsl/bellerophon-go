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
packages exist as placeholders and arrive in later milestones.

Roadmap (planning artifacts, copied from the v1 repo's `go-rewrite-proposal`
branch):

- [`docs/VISION.md`](docs/VISION.md) — multi-milestone vision M001 → M006,
  architecture target, NFRs, hard constraints, risk register
- [`docs/M001-SPEC.md`](docs/M001-SPEC.md) — SIP + media foundation (no AI),
  7 slices S01–S07, ends in live 3CX UAT
- [`docs/M002-DRAFT.md`](docs/M002-DRAFT.md) — pre-spec sketch for the inbound
  AI conversation loop; has open questions to resolve before promotion

## Install

### Option A — download a prebuilt binary

Pick the artifact for your platform from
[the latest release](https://github.com/stefandsl/bellerophon-go/releases/latest)
(`linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`). Example for
Linux amd64:

```bash
curl -fsSL -o bellerophon \
  https://github.com/stefandsl/bellerophon-go/releases/latest/download/bellerophon-linux-amd64
chmod +x bellerophon
./bellerophon --version
```

### Option B — build from source

Requires Go 1.23+. On a fresh Debian/Ubuntu box:

```bash
sudo apt-get update && sudo apt-get install -y git ca-certificates curl
# Install Go 1.23 if your distro ships an older one (`go version` to check):
curl -fsSL https://go.dev/dl/go1.23.4.linux-amd64.tar.gz \
  | sudo tar -C /usr/local -xz
export PATH=/usr/local/go/bin:$PATH

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

## Layout

```
cmd/bellerophon/        # main; CLI, config load, log init
internal/config/        # YAML schema + validation
internal/logging/       # slog setup
internal/sip/           # (M001 S03) sipgo REGISTER + INVITE handler
internal/rtp/           # (M001 S04) UDP RTP transport + jitter + RTCP
internal/audio/         # (M001 S05) G.711 codec + WAV playback
internal/vad/           # (M002) energy VAD + utterance buffering
internal/stt/           # (M002) Whisper-compatible client
internal/claude/        # (M002) HTTP client for the local Claude bridge
internal/tts/           # (M002) ElevenLabs-compatible client
internal/conversation/  # (M002) call-level state machine
docs/                   # plans and viewer-facing setup
```

## License

TBD.

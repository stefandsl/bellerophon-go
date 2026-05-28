# M001 — User Acceptance Test (UAT)

This document is the step-by-step manual verification plan for the M001
"SIP + media foundation" milestone. The same binary is exercised against
**three providers** in turn: MessageNet (Italian ITSP), a generic SIP
registrar (Asterisk), and 3CX. Each section covers all 10 success criteria
from [`M001-SPEC.md §2`](M001-SPEC.md).

If any section fails, the slice it falls under is reopened — do **not**
sign off the milestone with a partial pass.

## Prerequisites

- A built `bellerophon` binary for the host running the test (see
  [`README.md`](../README.md) for the build steps).
- A speakerphone or soft-phone you can dial from. Linphone is the
  recommended free option; mobile 3CX app works for the 3CX provider.
- For the per-provider sections: the relevant credentials. See
  [`config.example.yaml`](../config.example.yaml) for the YAML shape.
- A second machine on the same network (or `tcpdump`/`pcap` on the
  Bellerophon host) for the SIP / RTP packet capture in §6.
- Recommended hardware: at minimum the box you ship on (the Pi 5 in
  `docs/m001-pi-benchmark.md`). Audio quality assertions assume the
  Pi 5; faster hardware should comfortably exceed every threshold.

## Provider matrix

| Provider | Config file used | Required env / credentials |
|---|---|---|
| **MessageNet** | `config.messagenet.yaml` | `MESSAGENET_USER`, `MESSAGENET_PASS`, an outbound DID you can dial |
| **Generic (Asterisk)** | `config.asterisk.yaml` | `ASTERISK_*` SIP creds, a Linphone client to call the registered extension |
| **3CX** | `config.3cx.yaml` | `THREECX_*`, Stefan's existing 3CX deployment |

Each provider gets its own UAT section below. **Run all three.** A green
on MessageNet + 3CX with Asterisk skipped does NOT pass the milestone.

---

## Section A — Provider: MessageNet

### A.1 Register / Unregister (criterion §2.1)

1. Start: `./bellerophon --config config.messagenet.yaml`
2. **Expected log**: `[sip] registered as <DID>@sip.messagenet.it via provider=messagenet`
   within 5 s.
3. From the MessageNet admin GUI (or your account dashboard), confirm
   the SIP user shows as **online**.
4. Press `Ctrl-C`. **Expected**: log `bellerophon shutdown complete`
   within 2 s. MessageNet GUI shows the user **offline** within 5 s.

**Pass criterion**: registers, appears online, unregisters cleanly,
disappears within 5 s.

### A.2 Inbound INVITE (criterion §2.2)

1. Restart Bellerophon.
2. From a cell phone, dial the configured DID.
3. **Expected log**: `INVITE received` → `200 OK` → `ACK received`,
   all within 200 ms of each other.
4. **Expected**: `Call.LocalDID` in the structured log carries both
   `E164` (with `+39` prefix) and `Raw` (without). Cross-check it
   matches what MessageNet says it forwarded.

**Pass criterion**: call connects, DID normalization is correct,
no SIP errors in the log.

### A.3 Echo demo (criterion §2.3)

1. Restart Bellerophon with `--echo-mode`.
2. From a cell phone, dial the DID. When connected, speak
   `"hello, this is a test"` clearly into the handset.
3. **Expected**: you hear the same phrase echoed back ~500 ms later.
4. **Audio quality check (subjective)**: no clicks, no warble, no
   stutter. The echo is recognizably your voice.
5. (Optional) Record the test call (`--record /tmp/echo-mn.wav`),
   then run `scripts/echo-rms-diff.sh /tmp/echo-mn.wav` (TBD —
   pending S07 T03) and verify RMS deviation < 10 %.

**Pass criterion**: human ear says "that's me, with a delay." If
the recording-based RMS check is available, it must back that up
with deviation < 10 %.

### A.4 WAV playback (criterion §2.4)

> **Status note**: requires the playback wiring from S05 to land on the
> binary side (currently shipped in cmd/bellerophon only as the echo
> demo; a `--play <file>` flag is TBD). Mark this row as **deferred**
> until the binary exposes the playback path; that's a one-line wiring
> task on top of `internal/media/playback.go`.

1. Restart with `./bellerophon --config config.messagenet.yaml --play examples/ready-beep.wav`
2. Dial the DID. **Expected**: you hear the ready-beep, then silence
   while the call stays connected, then the TTS-rendered "hello world,
   you have reached bellerophon."
3. **Audio quality check**: clean playback, no codec artefacts beyond
   what G.711 inherently does.

**Pass criterion**: both files play back clearly; call stays connected
through the silence between them.

### A.5 DTMF (criterion §2.5)

1. Restart with `--echo-mode` (any mode that keeps the call open).
2. Dial the DID. When connected, on the cell phone keypad press in
   sequence: `1`, `2`, `3`, `*`, `#`, then any 5 random digits.
3. **Expected log** within 200 ms of each press:
   `[dtmf] received: 1`, `[dtmf] received: 2`, etc.
4. Repeat for a total of **100 keypresses** across multiple calls.
   Record the number of detected digits vs. pressed.

**Pass criterion**: ≥ 99 detected of 100. (Anything < 99 reopens
S06.)

### A.6 Recording (criterion §2.6)

1. Set `RECORD_TO=/tmp/m001-mn-test.wav` before restarting.
2. Restart Bellerophon. Dial the DID, speak for ≥ 30 s, hang up.
3. **Expected**: `/tmp/m001-mn-test.wav` exists (no `.partial`
   leftover), playable in VLC / mpv / ffplay.
4. Check duration: should equal call duration ±50 ms.
   `ffprobe -v error -show_entries format=duration /tmp/m001-mn-test.wav`
5. Check format: PCM16 mono 16 kHz.
   `ffprobe -v error -show_entries stream=codec_name,channels,sample_rate /tmp/m001-mn-test.wav`

**Pass criterion**: WAV exists, plays, format is correct, duration
matches.

### A.7 Clean shutdown (criterion §2.7)

1. Restart Bellerophon.
2. Place 100 short calls in quick succession (use the `sipp` driver
   from `test/integration/`).
3. Sample `runtime.NumGoroutine()` via `/debug/vars` (TBD endpoint;
   for now use `pprof` or just `lsof -p $(pgrep bellerophon) | wc -l`
   before and after).
4. **Expected**: post-test goroutine count is within ±2 of baseline.
   File descriptor count is stable.

**Pass criterion**: no leaks observable after 100 calls.

### A.8 Cross-compile (criterion §2.8)

> Verified in CI; nothing to do manually for this section.

### A.9 No CGO (criterion §2.9)

1. `CGO_ENABLED=0 go build -o /tmp/b ./cmd/bellerophon`
2. `file /tmp/b` → must include `statically linked`.

**Pass criterion**: builds with `CGO_ENABLED=0`, produces a static binary.

### A.10 Binary size (criterion §2.10)

1. `make release` (or equivalent cross-compile to `linux/arm64`).
2. `ls -lh dist/bellerophon-linux-arm64` → must be ≤ 30 MB.

**Pass criterion**: stripped Linux/ARM64 binary ≤ 30 MB.

---

## Section B — Provider: Generic (Asterisk)

Repeat **every** subsection of Section A with the generic-Asterisk
config. Asterisk-specific notes:

- Asterisk is usually permissive about Contact header parameters
  but **sensitive to `From: tag=` reuse on re-REGISTER**. The
  `sipprov.NewGeneric()` keepalive cadence of 30 s exercises this
  behavior.
- The "DID" for §2.2 in this section is just the extension number
  (e.g. `100`). `LocalDID.E164` and `LocalDID.Raw` should be identical
  — pass-through.
- Use Linphone (or any soft-phone you can register a second account
  on) as the inbound caller.

---

## Section C — Provider: 3CX

Repeat **every** subsection of Section A with the 3CX config.
3CX-specific things to watch for:

- **Section C.1 (Register)**: Capture the REGISTER packet with
  `tcpdump -i any -w /tmp/3cx-reg.pcap port 5060` during boot and
  verify `Contact:` carries `;transport=udp` AND `;rinstance=`
  (a 16-hex-char stable value). Without these, 3CX may register
  successfully but evict the binding silently within 5 minutes.
- **Section C.2 (Inbound)**: 3CX delivers extension numbers; both
  `LocalDID.E164` and `LocalDID.Raw` should equal the dialed
  extension (e.g. `100`). No `+39` prefix expected here.
- **Section C.7 (Shutdown)**: 3CX's silence-disconnect timer is
  ~10 min. Verify that with `sipprov.New3CX()` keepalive at 5 min,
  the binding never goes stale during a 30-min idle test.

---

## Sign-off

When all three sections (A, B, C) pass:

1. Update [`docs/m001-summary.md`](m001-summary.md) (TBD) with the
   per-provider quirks discovered during the run.
2. Run the Pi 5 benchmark (`docs/m001-pi-benchmark.md`, TBD) for
   each provider.
3. Open the M001-close PR with this file linked in the description
   and Stefan's sign-off marked **per provider**.

A partial sign-off (e.g. MessageNet ✅, 3CX ✅, Asterisk ⬜) is **not
sufficient** to close M001 — see the hand-off checklist in
[`M001-SPEC.md §8`](M001-SPEC.md).

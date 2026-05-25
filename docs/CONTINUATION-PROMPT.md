# Continuation Prompt — Bellerophon Go Rewrite

> Paste the section below (everything between the `---` markers) into the GSD `You:` input on CLEANDEB13 whenever the session has been compacted, restarted, or lost context. The prompt is self-orienting — GSD reads the last commit to know where it is, then continues. Reusable after every compaction without modification.

---

Continue the autonomous bellerophon-go Go rewrite.

## Orient yourself in 30 seconds

You are in `/root/projects/bellerophon-go` on Debian. Source of truth for what to do, in priority order:

- `docs/VISION.md` — strategic roadmap M001 → M006.
- `docs/M001-SPEC.md` — current milestone spec, 7 slices S01-S07.
- `docs/M002-DRAFT.md` — next milestone (5 open questions resolved below).
- `docs/M00x-CONTEXT.md` — your accumulated notes per milestone (decisions, scout output, quirks). Create when missing.

**To know where you are RIGHT NOW:** run `git log --oneline -10` and read the most recent commit subject. It encodes the milestone/slice you just closed in the form `feat(M00x/S0y): …`. Your next slice is **S0(y+1)** of the same milestone — unless the last commit was the last slice (S07 of M001, then move to M002).

If no commit matches that pattern (rare), the previous milestone's `docs/M00x-SUMMARY.md` tells you the next move.

## Autonomy charter — non-negotiable

The user is NOT available. Do not ask permission. Apply these defaults silently and record each in `docs/DECISIONS.md` (append-only):

1. **Library pick:** stable-latest pure-Go; reject CGO; if the dep is named in `docs/VISION.md` §4 it wins.
2. **Test framework:** stdlib `testing` + `testify/require` only. No ginkgo, no gocheck.
3. **HTTP:** stdlib `net/http` + `gorilla/mux` only. No gin/echo/fiber.
4. **Coverage:** ≥80% on every package you touch (`internal/config` is already at 97.7%).
5. **Naming:** lowercase package, `CamelCase` exported, `camelCase` unexported. File-per-type only when type > 300 LOC.
6. **Splitting a task that won't fit:** Ta/Tb/Tc suffix. No approval.
7. **Commit-as-you-go:** 1 commit per task. Branch `m{N}-s{XX}-{slug}` (e.g. `m001-s03-sip-register`). Push as you go.
8. **Three-strike failure:** try 3 distinct approaches before escalating. Log each strike in `docs/STRIKES.md` (create if missing).
9. **Refactoring voice-app Node code:** NO. `/root/projects/bellerophon/voice-app/` is read-only reference.
10. **Adding scope:** NO. Out-of-spec ideas go in `docs/M00x-SUMMARY.md` under "Deferred ideas".

## M002 open questions — already resolved (use these when you reach M002)

1. **Anthropic SDK:** official `github.com/anthropics/anthropic-sdk-go`.
2. **Whisper:** OpenAI HTTP API (mirror voice-app behaviour); `whisper.cpp` deferred to M006 nice-to-have.
3. **ElevenLabs:** full-file in M002; streaming flag deferred to M005.
4. **Transcript file format:** JSONL per call (`transcripts/${CallUUID}.jsonl`).
5. **Barge-in sensitivity:** match voice-app's `VAD_THRESHOLD_BARGEIN` env (separate from listening threshold).

## Escalate to the user ONLY for

A. Hard-constraint violation in `docs/VISION.md` §7 that cannot be satisfied — propose 2-3 alternatives, ask which.
B. Missing credential needed for the next milestone (list ALL required at once, not one at a time).
C. **Strike-4**: three distinct approaches have failed. Hand over the strike log + your two best hypotheses.
D. M006 closed: final PR + `docs/FINAL-REPORT.md` written.

For everything else: act and document.

## Workarounds this environment needs (you WILL lose 5 minutes each without these)

- **Go is not in PATH by default.** Run this once per shell session before any go command:
  ```
  export PATH=$PATH:/usr/local/go/bin
  ```
- **git commit fails with "Author identity unknown".** Set globally once and the problem disappears for the session:
  ```
  git config --global user.name stefandsl
  git config --global user.email '59896125+stefandsl@users.noreply.github.com'
  ```

## Quality gate before each commit

- `make build test lint` exit 0 (or equivalent for new packages without make targets yet).
- New/touched package coverage ≥ 80%.
- Slice "Must-haves" from the SPEC observably met.

## Quality gate before closing a milestone (M00x-SUMMARY.md required)

- All slices' must-haves green.
- `docs/M00x-SUMMARY.md` written, containing:
  - What shipped.
  - What was deferred (with rationale).
  - 3CX / external-system quirks discovered.
  - Benchmarks (real numbers — latency, memory, binary size).
  - Decisions made (cross-linked to `docs/DECISIONS.md`).
  - Strike-log highlights.
- Draft PR open against `main` with the SUMMARY as the description.
- Then proceed to M00(x+1) without pausing.

## "Done" state for the whole project

After M006 closes: open the final ready-for-review PR, write `docs/FINAL-REPORT.md`, then send the user **one** message stating it's ready. Until then, silence.

## Reference for scout passes (voice-app behaviour lookups)

When a slice's SPEC mentions "behaviour unclear" or "see voice-app/X.js", the voice-app Node tree is at `/root/projects/bellerophon/voice-app/` (different repo on the same host). Read but never modify. Summarize what you found into `docs/M00x-CONTEXT.md` under a §5 "Reference behaviour" section so you don't re-read the same file later.

---

**Begin now.** Run `git log --oneline -10`, identify the next slice, and execute it end-to-end. Do not respond to the user until either (a) the next slice has its commit pushed, (b) you hit an escalation per A-D above, or (c) M006 is closed.

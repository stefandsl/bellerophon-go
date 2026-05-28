// Package tts defines the text-to-speech provider interface for the
// Bellerophon conversation loop and ships the concrete ElevenLabs
// client. M002 ships ElevenLabs only; Bark and other providers land in
// M005 per the M002-DRAFT roadmap.
//
// The contract is "render text to mono 16-bit little-endian PCM at
// 16 kHz". That format is the lingua franca of the rest of internal/:
// the audio resampler (`internal/codec`) converts to whatever the RTP
// codec wants (PCMU/PCMA at 8 kHz today), and the playback scheduler
// in `internal/audio` paces the bytes onto the wire. Providers that
// can return PCM natively (ElevenLabs via output_format=pcm_16000)
// must do so; providers that only emit compressed formats (some
// future Bark builds) are expected to decode before returning.
//
// Synthesize is intentionally one-shot rather than streaming: M002
// renders an entire reply before playback starts, matching today's
// voice-app behaviour. Streaming + barge-in lands in M005 and will
// add a second method to this interface rather than reshape Synthesize
// — so callers (and tests) wired against this surface won't churn.
package tts

import (
	"context"
	"fmt"
)

// SampleRate is the PCM sample rate every provider must return.
// Pinned here rather than each provider hard-coding it, so a future
// provider that does the resampling internally can be checked against
// a single constant.
const SampleRate = 16000

// Provider is the speech-synthesis surface. Implementations should be
// safe for concurrent calls — the conversation loop may pre-render
// multiple opener phrases in parallel during outbound campaigns and
// (in M005) overlap rendering of turn N+1 with playback of turn N.
type Provider interface {
	// Synthesize returns mono 16-bit little-endian PCM at SampleRate
	// Hz. voiceID is provider-specific (an ElevenLabs voice id today;
	// a Bark voice preset name in M005). An empty voiceID asks the
	// provider for its configured default.
	//
	// ctx cancellation aborts the in-flight request and is the
	// caller's primary timeout mechanism.
	Synthesize(ctx context.Context, text, voiceID string) ([]byte, error)
}

// HTTPError is returned when the upstream service answers with a
// non-2xx status. Body holds the raw response (truncated) so debug
// logs surface the upstream error envelope verbatim. The retry layer
// in the conversation loop inspects StatusCode to branch transient
// (5xx, 429) vs hard (4xx) failures.
type HTTPError struct {
	// Source identifies which provider surfaced the error, e.g.
	// "elevenlabs". A single shared error type means call-site
	// branching stays uniform across providers.
	Source     string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Source, e.StatusCode, truncate(e.Body, 200))
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

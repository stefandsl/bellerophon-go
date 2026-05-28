// Package stt defines the speech-to-text provider interface for the
// Bellerophon conversation loop and the concrete OpenAI Whisper client.
// M002 ships Whisper only; additional providers (Gemini, Google Cloud
// Speech, openai-direct) land in M003 per the original roadmap.
//
// The interface contract is intentionally tiny — Transcribe takes mono
// PCM16 samples at a given rate and returns text — so future providers
// can be slotted in without touching the conversation state machine.
package stt

import "context"

// Provider is the speech-to-text interface every concrete client
// satisfies. Implementations should be safe for concurrent use because
// the conversation loop may keep multiple in-flight transcriptions (one
// per overlapping utterance during barge-in scenarios).
type Provider interface {
	// Transcribe converts a mono PCM16 sample stream into text.
	// sampleRate is in Hz (typically 16000 for Whisper). The returned
	// string is the recognized transcript, trimmed of leading/trailing
	// whitespace. ctx cancellation aborts the in-flight request and is
	// the caller's primary timeout mechanism.
	Transcribe(ctx context.Context, samples []int16, sampleRate int) (string, error)
}

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stefandsl/bellerophon-go/internal/tts"
)

// TestLive_TTS_ElevenLabs exercises the ElevenLabs client against
// api.elevenlabs.io. Gated on BELLEROPHON_LIVE_ELEVENLABS=1.
//
// Pins:
//
//  1. A trivial render returns a non-empty PCM body. (The bytes are
//     opaque without an MP3/PCM player; we only check length.)
//  2. The returned audio is plausible mono PCM16/16k — duration is
//     within 1 s / 30 s of "Hello." at 16 kHz mono.
//  3. The Cache short-circuits the second call (no second API hit).
//     Hard to verify externally; instead we re-synthesize the same
//     text and confirm latency drops by >5×, which is conservative
//     for any cache-vs-network ratio.
//
// Env contract:
//
//	ELEVENLABS_API_KEY  (required)
//	ELEVENLABS_VOICE_ID (optional override; default = client default)
//	ELEVENLABS_MODEL    (optional override; default = flash_v2_5)
func TestLive_TTS_ElevenLabs(t *testing.T) {
	requireEnv(t, "BELLEROPHON_LIVE_ELEVENLABS")
	apiKey := os.Getenv("ELEVENLABS_API_KEY")
	if apiKey == "" {
		t.Skip("ELEVENLABS_API_KEY not set")
	}

	cache, err := tts.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client, err := tts.NewElevenLabs(tts.ElevenLabsOptions{
		APIKey:         apiKey,
		DefaultVoiceID: os.Getenv("ELEVENLABS_VOICE_ID"),
		Model:          os.Getenv("ELEVENLABS_MODEL"),
		Cache:          cache,
	})
	if err != nil {
		t.Fatalf("NewElevenLabs: %v", err)
	}

	const phrase = "Hello, this is the M002 UAT live test."

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t0 := time.Now()
	pcm1, err := client.Synthesize(ctx, phrase, "")
	d1 := time.Since(t0)
	if err != nil {
		t.Fatalf("first synth: %v", err)
	}
	if len(pcm1) == 0 {
		t.Fatal("first synth returned empty body")
	}
	t.Logf("first synth: %d bytes in %v", len(pcm1), d1)

	// Sanity: at 16 kHz mono 16-bit, 1 s of audio = 32000 bytes. The
	// phrase reads in 2–4 s, so expect 60–130 kB.
	bytesPerSec := tts.SampleRate * 2
	durSec := float64(len(pcm1)) / float64(bytesPerSec)
	if durSec < 1.0 || durSec > 10.0 {
		t.Errorf("audio duration sanity check failed: %.2f s of PCM for a one-sentence phrase", durSec)
	}

	// Second call: cache hit. ElevenLabs answers production replies in
	// ~400–800 ms; a disk hit is sub-millisecond. Asserting >5× faster
	// is the conservative bound.
	t0 = time.Now()
	pcm2, err := client.Synthesize(ctx, phrase, "")
	d2 := time.Since(t0)
	if err != nil {
		t.Fatalf("cached synth: %v", err)
	}
	if len(pcm2) != len(pcm1) {
		t.Errorf("cached bytes len = %d, want %d", len(pcm2), len(pcm1))
	}
	t.Logf("cached synth: %d bytes in %v (%.1f× speedup)", len(pcm2), d2, float64(d1)/float64(d2))
	if d2*5 > d1 {
		t.Errorf("cache was not significantly faster: first=%v cached=%v", d1, d2)
	}
}

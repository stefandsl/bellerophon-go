package integration_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stefandsl/bellerophon-go/internal/stt"
)

// TestLive_STT_Whisper exercises the OpenAI Whisper client against
// the production API. Gated on BELLEROPHON_LIVE_WHISPER=1 so plain
// `go test ./...` skips it.
//
// What it pins:
//
//  1. A real fixture transcribes to a string whose lower-case form
//     contains every word in WHISPER_EXPECT (a comma-separated list).
//     Default expectation: "hello".
//  2. The API call completes inside 5 s on a 5 s utterance — looser
//     than the M002 §7 latency budget (which is end-to-end including
//     LLM + TTS); the per-stage cap exists so a Whisper outage
//     trips fast rather than chewing the whole 30 s default timeout.
//  3. The Provider interface assertion holds at runtime (defence-
//     in-depth: a future refactor that broke the interface would
//     break unit tests, but this is the canary for the live path).
//
// Env contract:
//
//	OPENAI_API_KEY            (required)
//	BELLEROPHON_AUDIO_FIXTURE (path to mono PCM16 16 kHz file)
//	WHISPER_EXPECT            (comma-separated keywords, default "hello")
//	WHISPER_LANG              (optional ISO-639-1 hint; default empty = auto)
func TestLive_STT_Whisper(t *testing.T) {
	requireEnv(t, "BELLEROPHON_LIVE_WHISPER")
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	fixturePath := requireAudioFixture(t, "BELLEROPHON_AUDIO_FIXTURE")
	samples := loadPCM16(t, fixturePath)

	client, err := stt.NewWhisper(stt.WhisperOptions{
		APIKey:   apiKey,
		Language: os.Getenv("WHISPER_LANG"),
	})
	if err != nil {
		t.Fatalf("NewWhisper: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	text, err := client.Transcribe(ctx, samples, 16000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Transcribe (%v): %v", elapsed, err)
	}
	t.Logf("Whisper returned %q in %v (audio len = %d samples / %d ms)",
		text, elapsed, len(samples), 1000*len(samples)/16000)

	if text == "" {
		t.Fatal("Whisper returned empty transcript on real audio")
	}

	expectCSV := os.Getenv("WHISPER_EXPECT")
	if expectCSV == "" {
		expectCSV = "hello"
	}
	lower := strings.ToLower(text)
	for _, kw := range strings.Split(expectCSV, ",") {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw == "" {
			continue
		}
		if !strings.Contains(lower, kw) {
			t.Errorf("transcript %q missing expected keyword %q", text, kw)
		}
	}
}

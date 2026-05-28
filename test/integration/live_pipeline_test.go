package integration_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stefandsl/bellerophon-go/internal/llm"
	"github.com/stefandsl/bellerophon-go/internal/stt"
	"github.com/stefandsl/bellerophon-go/internal/tts"
)

// TestLive_Pipeline_E2ELatency is the headline benchmark for the
// M002 §7 latency criterion: P95 ≤ 2.5 s end-to-end from "caller
// stops speaking" → "first TTS audio byte". Gated on
// BELLEROPHON_LIVE_PIPELINE=1 with all three upstream keys
// (OPENAI_API_KEY, ANTHROPIC_API_KEY, ELEVENLABS_API_KEY) present.
//
// The test runs PIPELINE_ITERATIONS (default 5) round-trips of:
//
//	loaded PCM16 fixture
//	  └─▶ Whisper transcribe       (stage A)
//	          └─▶ Anthropic reply  (stage B)
//	                  └─▶ ElevenLabs render  (stage C)
//
// and prints per-stage + cumulative P50/P95/P99 stats. The test
// fails only on the headline assertion (P95 cumulative ≤ 2500 ms)
// so per-stage outliers from OpenAI / Anthropic / ElevenLabs are
// surfaced but don't gate the milestone.
//
// Tighter bounds on the per-stage breakdown belong in M005's
// optimization pass once we have a baseline.
//
// Env contract:
//
//	BELLEROPHON_AUDIO_FIXTURE  (mono PCM16 16 kHz; ~3 s of speech)
//	PIPELINE_ITERATIONS        (optional; default 5)
//	PIPELINE_P95_BUDGET_MS     (optional; default 2500 — M002 §7)
//	OPENAI_API_KEY, ANTHROPIC_API_KEY, ELEVENLABS_API_KEY  (required)
//	ANTHROPIC_MODEL, ELEVENLABS_VOICE_ID, ELEVENLABS_MODEL (optional)
func TestLive_Pipeline_E2ELatency(t *testing.T) {
	requireEnv(t, "BELLEROPHON_LIVE_PIPELINE")
	openAIKey := requireOneOf(t, "OPENAI_API_KEY")
	anthKey := requireOneOf(t, "ANTHROPIC_API_KEY")
	elKey := requireOneOf(t, "ELEVENLABS_API_KEY")
	fixturePath := requireAudioFixture(t, "BELLEROPHON_AUDIO_FIXTURE")
	samples := loadPCM16(t, fixturePath)

	iters := envInt("PIPELINE_ITERATIONS", 5)
	budgetMs := envInt("PIPELINE_P95_BUDGET_MS", 2500)

	whisper, err := stt.NewWhisper(stt.WhisperOptions{APIKey: openAIKey})
	if err != nil {
		t.Fatalf("NewWhisper: %v", err)
	}
	anth, err := llm.NewAnthropic(llm.AnthropicOptions{
		APIKey: anthKey,
		Model:  os.Getenv("ANTHROPIC_MODEL"),
	})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	defer func() { _ = anth.EndSession(context.Background(), "live-pipeline") }()

	ttsCache, _ := tts.NewCache(t.TempDir(), 0)
	eleven, err := tts.NewElevenLabs(tts.ElevenLabsOptions{
		APIKey:         elKey,
		DefaultVoiceID: os.Getenv("ELEVENLABS_VOICE_ID"),
		Model:          os.Getenv("ELEVENLABS_MODEL"),
		Cache:          ttsCache,
	})
	if err != nil {
		t.Fatalf("NewElevenLabs: %v", err)
	}

	var (
		sttDurs, llmDurs, ttsDurs []time.Duration
		totalDurs                 []time.Duration
	)
	for i := 0; i < iters; i++ {
		// Fresh session per iter so the LLM does real work each time —
		// otherwise Anthropic might short-circuit on a repeated prompt.
		callID := fmt.Sprintf("live-pipeline-%d", i)

		// Stage A: STT
		ctxA, cancelA := context.WithTimeout(context.Background(), 30*time.Second)
		tA := time.Now()
		transcript, err := whisper.Transcribe(ctxA, samples, 16000)
		cancelA()
		dA := time.Since(tA)
		if err != nil {
			t.Fatalf("iter %d: Whisper: %v", i, err)
		}
		if transcript == "" {
			t.Fatalf("iter %d: empty transcript", i)
		}

		// Stage B: LLM
		ctxB, cancelB := context.WithTimeout(context.Background(), 30*time.Second)
		tB := time.Now()
		resp, err := anth.Query(ctxB, llm.Request{
			CallID:       callID,
			Prompt:       transcript,
			DevicePrompt: "You are Bellerophon, a phone assistant. Reply in one short sentence under 25 words.",
		})
		cancelB()
		dB := time.Since(tB)
		if err != nil {
			t.Fatalf("iter %d: LLM: %v", i, err)
		}

		// Stage C: TTS
		ctxC, cancelC := context.WithTimeout(context.Background(), 30*time.Second)
		tC := time.Now()
		pcm, err := eleven.Synthesize(ctxC, resp.Text, "")
		cancelC()
		dC := time.Since(tC)
		if err != nil {
			t.Fatalf("iter %d: TTS: %v", i, err)
		}
		if len(pcm) == 0 {
			t.Fatalf("iter %d: empty PCM", i)
		}

		sttDurs = append(sttDurs, dA)
		llmDurs = append(llmDurs, dB)
		ttsDurs = append(ttsDurs, dC)
		totalDurs = append(totalDurs, dA+dB+dC)

		t.Logf("iter %d: stt=%v llm=%v tts=%v total=%v | transcript=%q reply=%q",
			i, dA, dB, dC, dA+dB+dC, truncate(transcript, 60), truncate(resp.Text, 60))
	}

	stt := computeStats(sttDurs)
	llmS := computeStats(llmDurs)
	ttsS := computeStats(ttsDurs)
	tot := computeStats(totalDurs)

	t.Logf("\n=== M002 §7 latency benchmark (n=%d) ===\n", iters)
	t.Logf("stage       n    min    p50    p95    p99    max    mean")
	t.Logf("stt    %4d  %5v  %5v  %5v  %5v  %5v  %.0fms",
		stt.N, stt.Min, stt.P50, stt.P95, stt.P99, stt.Max, stt.MeanMs)
	t.Logf("llm    %4d  %5v  %5v  %5v  %5v  %5v  %.0fms",
		llmS.N, llmS.Min, llmS.P50, llmS.P95, llmS.P99, llmS.Max, llmS.MeanMs)
	t.Logf("tts    %4d  %5v  %5v  %5v  %5v  %5v  %.0fms",
		ttsS.N, ttsS.Min, ttsS.P50, ttsS.P95, ttsS.P99, ttsS.Max, ttsS.MeanMs)
	t.Logf("total  %4d  %5v  %5v  %5v  %5v  %5v  %.0fms",
		tot.N, tot.Min, tot.P50, tot.P95, tot.P99, tot.Max, tot.MeanMs)
	t.Logf("budget: P95 ≤ %d ms\n", budgetMs)

	if tot.P95 > time.Duration(budgetMs)*time.Millisecond {
		t.Errorf("M002 §7 FAILED: P95 = %v exceeds budget %d ms", tot.P95, budgetMs)
	}
}

func requireOneOf(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("skipped: set %s to run the live pipeline test", key)
	}
	return v
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

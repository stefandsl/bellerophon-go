package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stefandsl/bellerophon-go/internal/conversation"
	"github.com/stefandsl/bellerophon-go/internal/llm"
	"github.com/stefandsl/bellerophon-go/internal/vad"
)

// TestOfflineLoopOverhead measures the per-turn overhead the
// conversation loop itself adds, holding upstream-stage costs at a
// fixed simulated value via fakes. The point is to separate "the loop
// is the bottleneck" from "OpenAI is the bottleneck" when reading the
// live pipeline numbers from TestLive_Pipeline_E2ELatency.
//
// This test does NOT need API keys — it runs in CI on every PR as a
// regression guard. If the loop's scheduling overhead ever spikes
// (e.g. a future change adds a busy-wait or a forgotten
// time.Sleep), this catches it.
//
// The setup: each fake stage sleeps a fixed duration; the loop runs
// N turns; total wall time minus the sum of the sleeps is what the
// loop itself spent. We assert that overhead stays under
// LOOP_OVERHEAD_BUDGET_MS (default 25 ms per turn — generous on
// development hardware, tight enough to catch real regressions).
func TestOfflineLoopOverhead(t *testing.T) {
	t.Parallel()

	const turns = 5
	const sttDelay = 50 * time.Millisecond
	const llmDelay = 100 * time.Millisecond
	const ttsDelay = 80 * time.Millisecond

	src := newFakeUtteranceSource(turns + 1) // one extra for goodbye
	stt := &delaySTT{delay: sttDelay, transcripts: make([]string, turns+1)}
	for i := 0; i < turns; i++ {
		stt.transcripts[i] = "some user utterance"
	}
	stt.transcripts[turns] = "goodbye"

	llmFake := &delayLLM{delay: llmDelay, reply: "an answer"}
	ttsFake := &delayTTS{delay: ttsDelay}
	player := &instantPlayer{}

	for i := 0; i < turns+1; i++ {
		src.push()
	}

	loop, err := conversation.New(conversation.Options{
		CallID:           "offline-bench",
		VAD:              src,
		STT:              stt,
		LLM:              llmFake,
		TTS:              ttsFake,
		Player:           player,
		MaxTurns:         turns + 5,
		UtteranceTimeout: 500 * time.Millisecond,
		LLMTimeout:       2 * time.Second,
		TTSTimeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	if err := loop.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)

	// Lower bound: greeting TTS + N user turns (stt+llm+tts each) +
	// goodbye turn (stt + farewell TTS, no LLM call).
	upstreamPerTurn := sttDelay + llmDelay + ttsDelay
	expectedMin := ttsDelay + // greeting
		time.Duration(turns)*upstreamPerTurn + // user turns
		sttDelay + ttsDelay // goodbye turn: stt then farewell tts
	overhead := elapsed - expectedMin
	overheadPerTurn := overhead / time.Duration(turns+1)

	t.Logf("offline loop bench: %d turns, elapsed=%v, expected min=%v",
		turns, elapsed, expectedMin)
	t.Logf("overhead total=%v, per turn=%v", overhead, overheadPerTurn)

	// Budget: per-turn overhead must stay under 25 ms. Generous so
	// scheduling jitter on a loaded laptop doesn't flake CI, tight
	// enough that a regression that adds e.g. a 100 ms sleep would
	// trip immediately.
	budget := 25 * time.Millisecond
	if v := envInt("LOOP_OVERHEAD_BUDGET_MS", 25); v > 0 {
		budget = time.Duration(v) * time.Millisecond
	}
	if overheadPerTurn > budget {
		t.Errorf("loop overhead per turn = %v exceeds budget %v",
			overheadPerTurn, budget)
	}

	// Sanity: the loop actually ran the expected number of user turns.
	if loop.Turn() != turns+1 {
		t.Errorf("turn count = %d, want %d", loop.Turn(), turns+1)
	}
}

// ===== Fake backends with fixed simulated latency =====

type fakeUtteranceSource struct {
	ch chan vad.Utterance
}

func newFakeUtteranceSource(buf int) *fakeUtteranceSource {
	return &fakeUtteranceSource{ch: make(chan vad.Utterance, buf)}
}
func (f *fakeUtteranceSource) Utterances() <-chan vad.Utterance { return f.ch }
func (f *fakeUtteranceSource) push() {
	f.ch <- vad.Utterance{PCM16: []int16{0, 1, 2, 3}, DurationMs: 200}
}

type delaySTT struct {
	delay       time.Duration
	mu          sync.Mutex
	transcripts []string
}

func (d *delaySTT) Transcribe(ctx context.Context, _ []int16, _ int) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(d.delay):
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.transcripts) == 0 {
		return "goodbye", nil
	}
	t := d.transcripts[0]
	d.transcripts = d.transcripts[1:]
	return t, nil
}

type delayLLM struct {
	delay time.Duration
	reply string
}

func (d *delayLLM) Query(ctx context.Context, _ llm.Request) (llm.Response, error) {
	select {
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	case <-time.After(d.delay):
	}
	return llm.Response{Text: d.reply, SessionID: "sess", DurationMs: 1}, nil
}
func (d *delayLLM) EndSession(_ context.Context, _ string) error { return nil }

type delayTTS struct {
	delay time.Duration
}

func (d *delayTTS) Synthesize(ctx context.Context, text, _ string) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(d.delay):
	}
	return []byte("PCM:" + text), nil
}

type instantPlayer struct{}

func (p *instantPlayer) Play(ctx context.Context, _ []byte, _ int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

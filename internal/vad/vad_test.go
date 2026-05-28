package vad

import (
	"math"
	"sync"
	"testing"
	"time"
)

// genSine produces durMs of a sine wave at the given amplitude and
// frequency, sampled at rate Hz. Always int16, mono.
func genSine(rate, durMs int, freqHz, amp float64) []int16 {
	n := rate * durMs / 1000
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*freqHz*float64(i)/float64(rate)))
	}
	return out
}

// genSilence produces durMs of pure zero samples at rate Hz.
func genSilence(rate, durMs int) []int16 {
	return make([]int16, rate*durMs/1000)
}

func TestDetector_NoUtteranceFromPureSilence(t *testing.T) {
	t.Parallel()
	d := NewDetector(Options{})
	done := make(chan struct{})
	go func() {
		// Push 5 seconds of silence and close.
		for i := 0; i < 250; i++ {
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		d.Close()
		close(done)
	}()
	count := 0
	for range d.Utterances() {
		count++
	}
	<-done
	if count != 0 {
		t.Errorf("got %d utterances from pure silence, want 0", count)
	}
}

func TestDetector_EmitsUtteranceAfterSpeechAndSilence(t *testing.T) {
	t.Parallel()
	d := NewDetector(Options{})
	done := make(chan struct{})
	go func() {
		// 600 ms of speech (well above minSpeechMs=350) at amp=15000 RMS
		// of a sine is amp/√2 ≈ 10606 → above 3000 default.
		speech := genSine(DefaultSampleRate, 600, 440, 15000)
		d.Push(speech)
		// 1700 ms of silence (above endSilenceMs=1500) — triggers finalize.
		for i := 0; i < 85; i++ {
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		d.Close()
		close(done)
	}()
	utts := drainUtterances(d.Utterances())
	<-done

	if len(utts) != 1 {
		t.Fatalf("got %d utterances, want 1", len(utts))
	}
	u := utts[0]
	// Duration ≈ 600 ms speech + ~1500 ms hangover that was buffered.
	if u.DurationMs < 600 {
		t.Errorf("duration_ms=%d, want ≥ 600 (just the speech)", u.DurationMs)
	}
	if u.DurationMs > 2300 {
		t.Errorf("duration_ms=%d, want ≤ ~2200 (speech + hangover)", u.DurationMs)
	}
	if len(u.PCM16) == 0 {
		t.Error("utterance PCM is empty")
	}
}

func TestDetector_FalseStartFilteredBelowMinSpeechMs(t *testing.T) {
	t.Parallel()
	d := NewDetector(Options{})
	done := make(chan struct{})
	go func() {
		// 100 ms of speech — below minSpeechMs=350 — followed by long
		// silence. Must not emit.
		d.Push(genSine(DefaultSampleRate, 100, 440, 15000))
		for i := 0; i < 100; i++ {
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		d.Close()
		close(done)
	}()
	utts := drainUtterances(d.Utterances())
	<-done
	if len(utts) != 0 {
		t.Errorf("got %d utterances from a 100 ms burst, want 0 "+
			"(below minSpeechMs=%d)", len(utts), DefaultMinSpeechMs)
	}
}

func TestDetector_HangoverPreventsPrematureEnd(t *testing.T) {
	t.Parallel()
	d := NewDetector(Options{})
	done := make(chan struct{})
	go func() {
		// 500 ms speech, then 1 s silence (less than 1.5 s hangover),
		// then more speech — should be ONE utterance, not two.
		d.Push(genSine(DefaultSampleRate, 500, 440, 15000))
		for i := 0; i < 50; i++ {
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		d.Push(genSine(DefaultSampleRate, 500, 440, 15000))
		// Now the long silence to finalize.
		for i := 0; i < 100; i++ {
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		d.Close()
		close(done)
	}()
	utts := drainUtterances(d.Utterances())
	<-done
	if len(utts) != 1 {
		t.Errorf("expected 1 utterance (silence too short to split), got %d", len(utts))
	}
}

func TestDetector_TwoUtterancesSeparatedByLongSilence(t *testing.T) {
	t.Parallel()
	d := NewDetector(Options{})
	done := make(chan struct{})
	go func() {
		// First utterance: 500 ms speech + 1700 ms silence → finalize.
		d.Push(genSine(DefaultSampleRate, 500, 440, 15000))
		for i := 0; i < 85; i++ {
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		// Second utterance: 500 ms speech + 1700 ms silence → finalize.
		d.Push(genSine(DefaultSampleRate, 500, 880, 15000))
		for i := 0; i < 85; i++ {
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		d.Close()
		close(done)
	}()
	utts := drainUtterances(d.Utterances())
	<-done
	if len(utts) != 2 {
		t.Errorf("expected 2 utterances, got %d", len(utts))
	}
}

func TestDetector_FlushOnClose(t *testing.T) {
	t.Parallel()
	d := NewDetector(Options{})
	done := make(chan struct{})
	go func() {
		// Speech long enough to cross minSpeechMs but no trailing silence —
		// Close() should flush the in-progress utterance.
		d.Push(genSine(DefaultSampleRate, 500, 440, 15000))
		d.Close()
		close(done)
	}()
	utts := drainUtterances(d.Utterances())
	<-done
	if len(utts) != 1 {
		t.Errorf("expected 1 utterance from Close-flush, got %d", len(utts))
	}
}

func TestDetector_FlushOnCloseSkipsFalseStart(t *testing.T) {
	t.Parallel()
	d := NewDetector(Options{})
	done := make(chan struct{})
	go func() {
		// Below minSpeechMs — Close should NOT flush.
		d.Push(genSine(DefaultSampleRate, 100, 440, 15000))
		d.Close()
		close(done)
	}()
	utts := drainUtterances(d.Utterances())
	<-done
	if len(utts) != 0 {
		t.Errorf("expected 0 utterances (false start), got %d", len(utts))
	}
}

func TestDetector_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	d := NewDetector(Options{})
	d.Close()
	d.Close()                                           // must not panic
	d.Push(genSine(DefaultSampleRate, 100, 440, 15000)) // must not panic
}

func TestDetector_PeakOnsetCaughtBeforeRMSRises(t *testing.T) {
	t.Parallel()
	// A sharp single-sample spike whose RMS is low (one tall sample
	// among many zeros) but whose peak amplitude exceeds maxThreshold.
	d := NewDetector(Options{})
	frame := make([]int16, DefaultSampleRate*20/1000) // 20 ms
	frame[10] = 20000                                 // > MaxThreshold default 8000
	done := make(chan struct{})
	go func() {
		// Sustained spiky frames cumulating ≥ minSpeechMs.
		for i := 0; i < 25; i++ {
			d.Push(append([]int16(nil), frame...))
		}
		// Then long silence.
		for i := 0; i < 85; i++ {
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		d.Close()
		close(done)
	}()
	utts := drainUtterances(d.Utterances())
	<-done
	if len(utts) != 1 {
		t.Errorf("expected 1 utterance (peak threshold trigger), got %d", len(utts))
	}
}

func TestDetector_OptionsOverridesAreEffective(t *testing.T) {
	t.Parallel()
	// Lower minSpeechMs to 50 — a 60 ms burst should now pass.
	d := NewDetector(Options{MinSpeechMs: 50, EndSilenceMs: 200})
	done := make(chan struct{})
	go func() {
		d.Push(genSine(DefaultSampleRate, 60, 440, 15000))
		for i := 0; i < 15; i++ { // 300 ms silence > endSilenceMs=200
			d.Push(genSilence(DefaultSampleRate, 20))
		}
		d.Close()
		close(done)
	}()
	utts := drainUtterances(d.Utterances())
	<-done
	if len(utts) != 1 {
		t.Errorf("expected 1 utterance with reduced thresholds, got %d", len(utts))
	}
}

func TestDetector_ConcurrentPushIsSafe(t *testing.T) {
	t.Parallel()
	// Single producer is the documented contract, but the implementation
	// guards with a mutex; verify the race detector stays clean under
	// two writers.
	d := NewDetector(Options{BufSize: 64})
	speech := genSine(DefaultSampleRate, 600, 440, 15000)
	silence := genSilence(DefaultSampleRate, 20)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		d.Push(speech)
		for i := 0; i < 85; i++ {
			d.Push(silence)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			d.Push(silence)
		}
	}()
	wg.Wait()
	d.Close()
	_ = drainUtterances(d.Utterances())
}

func TestAnalyze_ZeroInputDoesNotPanic(t *testing.T) {
	t.Parallel()
	rms, peak := analyze(nil)
	if rms != 0 || peak != 0 {
		t.Errorf("analyze(nil) = (%v,%v), want (0,0)", rms, peak)
	}
	rms, peak = analyze([]int16{})
	if rms != 0 || peak != 0 {
		t.Errorf("analyze(empty) = (%v,%v), want (0,0)", rms, peak)
	}
}

func TestAnalyze_KnownVectors(t *testing.T) {
	t.Parallel()
	// All-zeros: rms=0, peak=0.
	rms, peak := analyze([]int16{0, 0, 0, 0})
	if rms != 0 || peak != 0 {
		t.Errorf("zeros: (%v,%v), want (0,0)", rms, peak)
	}
	// Constant amplitude → RMS == |amplitude|.
	rms, peak = analyze([]int16{1000, 1000, 1000, 1000})
	if math.Abs(rms-1000) > 0.5 || peak != 1000 {
		t.Errorf("constant 1000: (%v,%v), want (1000,1000)", rms, peak)
	}
	// Symmetric ±amplitude — RMS still == amplitude, peak == amplitude.
	rms, peak = analyze([]int16{1000, -1000, 1000, -1000})
	if math.Abs(rms-1000) > 0.5 || peak != 1000 {
		t.Errorf("symmetric ±1000: (%v,%v), want (1000,1000)", rms, peak)
	}
	// int16 min is the canonical overflow case for naive abs() —
	// confirm we don't blow it up.
	rms, peak = analyze([]int16{-32768, -32768, -32768, -32768})
	if peak != 32768 {
		t.Errorf("int16-min peak: %v, want 32768", peak)
	}
	if math.Abs(rms-32768) > 0.5 {
		t.Errorf("int16-min rms: %v, want 32768", rms)
	}
}

// drainUtterances reads everything off the channel until it closes, with
// a generous timeout to surface deadlocks as test failures.
func drainUtterances(ch <-chan Utterance) []Utterance {
	var out []Utterance
	timeout := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, u)
		case <-timeout:
			return out
		}
	}
}
